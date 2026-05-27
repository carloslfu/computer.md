// SPDX-License-Identifier: Apache-2.0

// Pure functions for normalising a daemon /conversations/{id} response
// into the dashboard's in-memory ChatMessage list. Kept free of React
// so we can test the rehydration rules — including the pending-approval
// reconstruction — without mounting the component.

import type { Attachment } from "./attachments";

export type ChatMessageRole = "user" | "agent" | "system";

export type ChatMessageType =
  | "text"
  | "screenshot"
  | "approval"
  | "activity_summary"
  | "credential_request"
  | "setup_request"
  | "auto_approved"
  | "system_notice";

export type ChatMessageAction = { label: string; action: "approve" | "edit" | "cancel" };

// Field types come straight from the daemon's CredentialFieldType enum.
// The UI picks the input control off this value.
export type CredentialFieldType = "text" | "password" | "url" | "token";

export type CredentialSpec = {
  name: string;
  label: string;
  type?: CredentialFieldType;
  hint?: string;
};

export type CredentialStored = {
  at: string;
  names: string[];
};

export type CredentialRequestPayload = {
  tool_use_id?: string;
  // task_id and message_id are embedded by the daemon so a rehydrated
  // or SSE-replayed card has everything it needs without side-channel
  // lookups. task_id targets the submit POST; message_id lets the
  // SSE handler dedupe against the rehydrated bubble.
  task_id?: string;
  message_id?: string;
  title: string;
  reason?: string;
  fields: CredentialSpec[];
  stored?: CredentialStored | null;
  // Mutually exclusive with stored. Set when the card is no longer
  // actionable — the task that asked for it ended, or the agent gave
  // up waiting. The UI renders an expired-receipt state, not a form.
  expired_at?: string | null;
  expired_reason?: string | null;
};

export type SetupField = {
  name: string;
  label: string;
  type?: CredentialFieldType;
  placeholder?: string;
  hint?: string;
};

export type SetupStored = {
  at: string;
};

export type SetupRequestPayload = {
  code: string;
  message_id?: string;
  title: string;
  body: string;
  footnote?: string;
  primary_label?: string;
  settings_path?: string;
  fields?: SetupField[];
  stored?: SetupStored | null;
};

export type ChatMessage = {
  id: string;
  role: ChatMessageRole;
  type: ChatMessageType;
  // For screenshot messages, `content` carries the caption (the one-line
  // narration the agent emitted right before the screenshot tool call).
  // For everything else, it's the raw message text.
  content: string;
  imageUrl?: string;
  description?: string;
  actions?: ChatMessageAction[];
  // Structured approval fields — populated when the daemon emitted an
  // ApprovalPayload (JSON-encoded in message.content / task.result).
  approvalTitle?: string;
  approvalReason?: string;
  approvalCommand?: string;
  // Soft Deny extension. severity "escalate" (default) renders the
  // neutral approval card; "soft_deny" renders the red-framed card with
  // a friction-gated override. friction picks the override gate:
  //   - "delay" disables the override button for a short countdown
  //   - "type_to_confirm" requires the customer to retype typeTarget
  approvalSeverity?: "escalate" | "soft_deny";
  approvalFriction?: "none" | "delay" | "type_to_confirm";
  approvalTypeTarget?: string;
  // Set on rehydrate (and live, on click) when the approval has been
  // decided. The card renders compactly with a decision pill and no
  // buttons. `decided` is the defensive fallback when we know the task
  // moved on but can't recover the specific decision — rare.
  // `expired` is the redirect / 24h-timeout terminal — same compact
  // render as decided but with a distinct pill colour and the
  // approvalExpiredReason explaining what happened.
  approvalResolution?: "approved" | "denied" | "decided" | "expired";
  // When approvalResolution === "expired", carries the daemon-supplied
  // reason ("customer redirected attention…" / "no response within 24h" /
  // "task ended") so the user understands why the card closed without
  // their input.
  approvalExpiredReason?: string;
  // Auto-approve pill. When the daemon emits task:auto_approved (the
  // classifier downgraded a Confirm to Allow), we render a small
  // inline message describing what just happened. content carries
  // the plain-English title, autoApproveReason carries the
  // classifier's specific reasoning, autoApproveCommand carries the
  // raw command behind a chevron toggle.
  autoApproveReason?: string;
  autoApproveCommand?: string;
  // Structured credential_request fields — populated when the daemon
  // emitted a CredentialRequestPayload. Request contains the original
  // spec and (once filled) the stored receipt.
  credentialRequest?: CredentialRequestPayload;
  setupRequest?: SetupRequestPayload;
  taskId?: string;
  // Files the user attached to this message (user role only).
  attachments?: Attachment[];
  timestamp: string;
};

export function isRunningActivitySummary(content: string): boolean {
  return /^Status:\s*running\s*$/im.test(
    content.replace(/<!--\s*steps:\d+\s*-->\s*/g, ""),
  );
}

export function isActivitySummaryForTask(
  message: ChatMessage,
  taskId: string,
): boolean {
  return (
    message.type === "activity_summary" &&
    (message.taskId === taskId || message.id === `act-${taskId}`)
  );
}

type ActivityUpdate = {
  taskId: string;
  summaryMarkdown: string;
  stepCount?: number;
  timestamp?: string;
};

export function upsertActivityMessage(
  messages: ChatMessage[],
  update: ActivityUpdate,
): ChatMessage[] {
  if (!update.taskId || !update.summaryMarkdown) return messages;
  const marker =
    typeof update.stepCount === "number"
      ? `<!-- steps:${update.stepCount} -->\n`
      : "";
  const content = marker + update.summaryMarkdown.replace(/<!--\s*steps:\d+\s*-->\s*/g, "");
  const existingIndex = messages.findIndex((m) =>
    isActivitySummaryForTask(m, update.taskId),
  );
  const existing = existingIndex >= 0 ? messages[existingIndex] : undefined;
  const nextMessage: ChatMessage = {
    ...(existing ?? {
      id: `act-${update.taskId}`,
      role: "agent" as const,
      type: "activity_summary" as const,
      content: "",
      taskId: update.taskId,
      timestamp: update.timestamp || new Date().toISOString(),
    }),
    id: existing?.id || `act-${update.taskId}`,
    role: "agent",
    type: "activity_summary",
    content,
    taskId: update.taskId,
    timestamp: update.timestamp || existing?.timestamp || new Date().toISOString(),
  };
  if (existingIndex < 0) return [...messages, nextMessage];
  return [
    ...messages.slice(0, existingIndex),
    ...messages.slice(existingIndex + 1),
    nextMessage,
  ];
}

type ApprovalPayload = {
  tool?: string;
  command?: string;
  title?: string;
  reason?: string;
  severity?: "escalate" | "soft_deny";
  friction?: "none" | "delay" | "type_to_confirm";
  type_target?: string;
  // New (v0.71+): daemon embeds these so a rehydrated or replayed card
  // can render its resolved / expired state without a side-channel
  // lookup. Mirrors the credential_request payload shape.
  message_id?: string;
  resolved?: "approved" | "denied" | "";
  expired_at?: string;
  expired_reason?: string;
};

// decodeApproval turns a JSON-encoded ApprovalPayload (as stored in
// message.content or task.result) back into structured fields. Returns
// null for legacy plain-text prompts so callers fall through to the
// description-only rendering.
function decodeApproval(raw: string): ApprovalPayload | null {
  const trimmed = typeof raw === "string" ? raw.trimStart() : "";
  if (!trimmed.startsWith("{")) return null;
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (!parsed || typeof parsed !== "object") return null;
    const p = parsed as ApprovalPayload;
    if (!p.title && !p.command) return null;
    return p;
  } catch {
    return null;
  }
}

function applyApprovalPayload(msg: ChatMessage, raw: string) {
  const payload = decodeApproval(raw);
  if (!payload) {
    msg.description = raw;
    return;
  }
  msg.approvalTitle = payload.title;
  msg.approvalReason = payload.reason;
  msg.approvalCommand = payload.command;
  msg.approvalSeverity = payload.severity ?? "escalate";
  msg.approvalFriction = payload.friction ?? "none";
  msg.approvalTypeTarget = payload.type_target;
  // Carry the terminal state across rehydrate / SSE replay so a
  // reconnecting client paints the receipt, not the live form. expired
  // wins over resolved if both are set (defensive — daemon should never
  // do that).
  if (payload.expired_at) {
    msg.approvalResolution = "expired";
    msg.approvalExpiredReason = payload.expired_reason || "";
    msg.actions = undefined;
  } else if (payload.resolved === "approved" || payload.resolved === "denied") {
    msg.approvalResolution = payload.resolved;
    msg.actions = undefined;
  }
  // Keep a plain-text description as a fallback for any consumer that
  // hasn't wired the structured fields through yet.
  const parts = [payload.title, payload.reason, payload.command ? `Command: ${payload.command}` : ""].filter(Boolean);
  msg.description = parts.join("\n\n");
}

// decodeCredentialRequest parses a persisted credential_request payload
// back into structured form. Returns null for anything that doesn't
// look like a valid payload so callers can fall through to a text
// rendering.
export function decodeCredentialRequest(raw: string): CredentialRequestPayload | null {
  const trimmed = typeof raw === "string" ? raw.trimStart() : "";
  if (!trimmed.startsWith("{")) return null;
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (!parsed || typeof parsed !== "object") return null;
    const p = parsed as CredentialRequestPayload;
    if (!Array.isArray(p.fields) || p.fields.length === 0) return null;
    if (!p.title) return null;
    return p;
  } catch {
    return null;
  }
}

function defaultOperatorOpenAISetupPayload(messageId?: string): SetupRequestPayload {
  return {
    code: "operator_openai_key_required",
    message_id: messageId,
    title: "Connect this computer's manager",
    body: "This connected computer needs your OpenAI API key before the manager can run tasks. Add it once. It stays on this machine.",
    footnote:
      "Connected machines use your OpenAI account directly. VibeCraft hosted credits are not used for these manager calls.",
    primary_label: "Save key",
    settings_path: "/settings?tab=manager",
    fields: [
      {
        name: "openai_key",
        label: "OpenAI API key",
        type: "token",
        placeholder: "sk-...",
        hint: "Use a key from your OpenAI project.",
      },
    ],
  };
}

export function decodeSetupRequest(raw: string, messageId?: string): SetupRequestPayload | null {
  const text = typeof raw === "string" ? raw : "";
  if (
    text.includes("OpenAI manager key is not configured") &&
    text.includes("/etc/vibecraft/openai.key")
  ) {
    return defaultOperatorOpenAISetupPayload(messageId);
  }

  const trimmed = text.trimStart();
  if (!trimmed.startsWith("{")) return null;
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (!parsed || typeof parsed !== "object") return null;
    const p = parsed as SetupRequestPayload;
    if (!p.code || !p.title || !p.body) return null;
    return { ...p, message_id: p.message_id || messageId };
  } catch {
    return null;
  }
}

type DaemonMessage = {
  id?: unknown;
  role?: unknown;
  content?: unknown;
  type?: unknown;
  image_data?: unknown;
  attachments?: unknown;
  created_at?: unknown;
  timestamp?: unknown;
};

function parseAttachments(raw: unknown): Attachment[] | undefined {
  if (!Array.isArray(raw) || raw.length === 0) return undefined;
  const out: Attachment[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object") continue;
    const rec = item as Record<string, unknown>;
    const name = typeof rec.name === "string" ? rec.name : "";
    const path = typeof rec.path === "string" ? rec.path : "";
    const mime = typeof rec.mime === "string" ? rec.mime : "";
    const size = typeof rec.size === "number" ? rec.size : 0;
    const original = typeof rec.original === "string" ? rec.original : undefined;
    if (!name || !path) continue;
    out.push({ name, path, mime, size, original });
  }
  return out.length > 0 ? out : undefined;
}

type DaemonTask = {
  id?: unknown;
  instruction?: unknown;
  status?: unknown;
  result?: unknown;
  created_at?: unknown;
  started_at?: unknown;
  updated_at?: unknown;
};

type DaemonConversationPayload = {
  messages?: DaemonMessage[];
  tasks?: DaemonTask[];
};

const APPROVAL_ACTIONS: ChatMessageAction[] = [
  { label: "Approve", action: "approve" },
  { label: "Deny", action: "cancel" },
];

function oneLine(raw: unknown, fallback = ""): string {
  if (typeof raw !== "string") return fallback;
  return raw.replace(/\s+/g, " ").trim() || fallback;
}

function runningActivityFromTask(task: DaemonTask): ChatMessage | null {
  if (task.status !== "running") return null;
  const taskId = typeof task.id === "string" ? task.id : "";
  if (!taskId) return null;
  const instruction = oneLine(task.instruction, "Current request");
  const timestamp = String(
    task.updated_at || task.started_at || task.created_at || "",
  );
  return {
    id: `act-${taskId}`,
    role: "agent",
    type: "activity_summary",
    taskId,
    content:
      `<!-- steps:0 -->\nTask: ${instruction}\nStatus: running\n` +
      "Duration: In progress\n\nCurrent: Manager is working.",
    timestamp,
  };
}

// rehydrateMessages merges the persisted message list with any live
// task state that should be surfaced as a chat bubble. The daemon
// stores approval prompts in tasks[].result when a task is paused in
// waiting_for_input; the UI needs to render them as approval cards on
// page load, so we reconstruct them here.
//
// The result order is: all persisted messages (in their stored order),
// followed by any pending approvals. This matches the chronological
// intuition — the approval is the most recent thing that happened on
// the task, so it belongs at the bottom of the chat.
export function rehydrateMessages(data: DaemonConversationPayload): ChatMessage[] {
  const out: ChatMessage[] = [];

  if (Array.isArray(data.messages)) {
    for (const m of data.messages) {
      const type = (typeof m.type === "string" ? m.type : "text") as ChatMessageType;
      const role: ChatMessageRole =
        m.role === "assistant" ? "agent" : ((m.role as ChatMessageRole) ?? "agent");
      const msg: ChatMessage = {
        id: String(m.id ?? ""),
        role,
        type,
        content: typeof m.content === "string" ? m.content : "",
        timestamp: String(m.timestamp ?? m.created_at ?? ""),
        imageUrl: m.image_data ? `data:image/png;base64,${String(m.image_data)}` : undefined,
        attachments: parseAttachments(m.attachments),
      };

      if (type === "activity_summary") {
        const markerMatch = (typeof m.content === "string" ? m.content : "").match(
          /<!--\s*steps:\d+\s*-->/,
        );
        if (markerMatch && m.id) {
          msg.taskId = `rehydrated-${String(m.id)}`;
        }
      }

      if (type === "approval") {
        // A persisted approval card carries everything the UI needs to
        // re-render without touching tasks[]. Content is either a JSON
        // ApprovalPayload (new format) or a legacy plain-text prompt.
        applyApprovalPayload(msg, msg.content);
        // Only attach the live action buttons if the payload didn't
        // already mark the card as resolved/expired. If it did,
        // applyApprovalPayload cleared msg.actions so the card renders
        // as a receipt and the resolution loop below doesn't downgrade
        // "approved"/"denied"/"expired" to a generic "decided".
        if (!msg.approvalResolution) {
          msg.actions = APPROVAL_ACTIONS;
        }
        // The approval card's id must equal the task id so the SSE
        // dedupe in ChatLayout (id === taskId) kicks in when the
        // live task:waiting event arrives for the same task.
      }

      if (type === "credential_request") {
        // The persisted content is the CredentialRequestPayload JSON.
        // If it has .stored set, the card renders as a receipt; if
        // not, as the input form — the component decides based on the
        // payload alone.
        const payload = decodeCredentialRequest(msg.content);
        if (payload) {
          msg.credentialRequest = payload;
        }
      }

      if (type === "setup_request") {
        const payload = decodeSetupRequest(msg.content, msg.id);
        if (payload) {
          msg.setupRequest = payload;
        }
      }

      if (type === "text" && role === "agent") {
        const payload = decodeSetupRequest(msg.content, msg.id);
        if (payload) {
          msg.type = "setup_request";
          msg.setupRequest = payload;
          msg.content = JSON.stringify(payload);
        }
      }

      out.push(msg);
    }
  }

  // Resolve persisted approval cards by pairing each with the user's
  // recorded response. The daemon stores the response as a user-role text
  // message with content exactly "Approved" or "Denied" (engine.go
  // normalises yes/y and no/n). The card carries the decision; the user
  // message bubble is redundant alongside it, so we filter it out.
  const skipIndices = new Set<number>();
  const pendingTaskIds = new Set<string>();
  if (Array.isArray(data.tasks)) {
    for (const t of data.tasks) {
      if (t.status === "waiting_for_input" && typeof t.id === "string") {
        pendingTaskIds.add(t.id);
      }
    }
  }

  let lastApprovalIndex = -1;
  for (let i = out.length - 1; i >= 0; i--) {
    if (out[i].type === "approval") {
      lastApprovalIndex = i;
      break;
    }
  }

  for (let i = 0; i < out.length; i++) {
    const card = out[i];
    if (card.type !== "approval") continue;

    // If the persisted payload already carries an authoritative
    // terminal state (resolved approved/denied or expired), trust it.
    // The user-message pairing and "decided" fallback below are for
    // older daemons that didn't embed the receipt in the payload.
    if (card.approvalResolution) continue;

    let pairedDecision: "approved" | "denied" | null = null;
    for (let j = i + 1; j < out.length; j++) {
      const candidate = out[j];
      if (candidate.role !== "user") continue;
      if (candidate.type !== "text") continue;
      const content = (candidate.content || "").trim();
      if (content === "Approved") {
        pairedDecision = "approved";
        skipIndices.add(j);
      } else if (content === "Denied") {
        pairedDecision = "denied";
        skipIndices.add(j);
      }
      break; // pair only with the immediately-next user message
    }

    if (pairedDecision) {
      card.approvalResolution = pairedDecision;
      card.actions = undefined;
      continue;
    }

    // Defensive fallback: a persisted approval with no paired user
    // response is still "decided" if the task has moved past
    // waiting_for_input — or if this isn't the latest approval (an older
    // one is implicitly resolved before a newer one was created, since
    // the engine pauses on every approval and a new one can only appear
    // after the previous unblocks). Either signal means: don't render
    // the actionable buttons. Mark `decided` and the card renders the
    // neutral resolved state.
    const isPending =
      pendingTaskIds.size > 0 && i === lastApprovalIndex;
    if (!isPending) {
      card.approvalResolution = "decided";
      card.actions = undefined;
    }
  }

  // Resolve persisted credential_request cards the same way. The card is
  // either: (a) already filled (`stored` set) and renders a receipt; (b)
  // already expired (`expired_at` set) and renders an expired receipt; or
  // (c) still open. If still open AND its task is not actually in
  // waiting_for_input, the task ended without the daemon stamping it
  // — older daemons, race on reconnect, etc. Stamp it expired locally so
  // the UI doesn't show a form that can't be submitted.
  let lastCredentialIndex = -1;
  for (let i = out.length - 1; i >= 0; i--) {
    if (out[i].type === "credential_request") {
      lastCredentialIndex = i;
      break;
    }
  }
  for (let i = 0; i < out.length; i++) {
    const card = out[i];
    if (card.type !== "credential_request") continue;
    const payload = card.credentialRequest;
    if (!payload) continue;
    if (payload.stored || payload.expired_at) continue;

    // The latest open credential card is allowed to remain actionable
    // ONLY if its task is still in waiting_for_input. Anything else
    // (older card, no live waiting task) is implicitly expired.
    const taskId = payload.task_id || card.taskId || "";
    const isPending =
      i === lastCredentialIndex && !!taskId && pendingTaskIds.has(taskId);
    if (!isPending) {
      payload.expired_at = new Date().toISOString();
      payload.expired_reason = payload.expired_reason || "task ended";
      card.credentialRequest = { ...payload };
    }
  }

  const filteredOut: ChatMessage[] = [];
  for (let i = 0; i < out.length; i++) {
    if (skipIndices.has(i)) continue;
    filteredOut.push(out[i]);
  }

  if (Array.isArray(data.tasks)) {
    const seenActivityTaskIds = new Set(
      filteredOut
        .filter((m) => m.type === "activity_summary" && m.taskId)
        .map((m) => m.taskId as string),
    );
    for (const t of data.tasks) {
      const activity = runningActivityFromTask(t);
      if (!activity || seenActivityTaskIds.has(activity.taskId || "")) {
        continue;
      }
      filteredOut.push(activity);
      seenActivityTaskIds.add(activity.taskId || "");
    }
  }

  // Pending approvals from tasks in waiting_for_input. De-duped against
  // any persisted approval message that already carries the same task
  // id — the daemon may emit both, but the message is durable so it
  // wins.
  const seenApprovalTaskIds = new Set(
    filteredOut.filter((m) => m.type === "approval").map((m) => m.id),
  );

  if (Array.isArray(data.tasks)) {
    for (const t of data.tasks) {
      if (t.status !== "waiting_for_input") continue;
      const taskId = typeof t.id === "string" ? t.id : "";
      if (!taskId || seenApprovalTaskIds.has(taskId)) continue;
      const question = typeof t.result === "string" ? t.result : "";
      if (!question) continue;

      // Credential requests already have a persisted credential_request
      // message (see rehydration above). The task.result mirrors the
      // same payload so the daemon can replay it on SSE reconnect, but
      // we must not turn it into an approval card here.
      if (decodeCredentialRequest(question)) continue;

      const approval: ChatMessage = {
        id: taskId,
        role: "agent",
        type: "approval",
        content: question,
        actions: APPROVAL_ACTIONS,
        timestamp: String(t.updated_at ?? ""),
      };
      applyApprovalPayload(approval, question);
      filteredOut.push(approval);
      seenApprovalTaskIds.add(taskId);
    }
  }

  return filteredOut;
}
