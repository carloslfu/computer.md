// SPDX-License-Identifier: Apache-2.0

import { useState, useEffect, useCallback, useMemo, useRef } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { Menu, X, Eye, ChevronLeft, Square } from "lucide-react";
import { ConversationSidebar } from "./ConversationSidebar";
import { ChatThread } from "./ChatThread";
import { ChatInput } from "./ChatInput";
import { NotificationBell } from "./NotificationBell";
import {
  PresenceAvatars,
  type PresenceUser,
} from "./PresenceAvatars";
import { apiFetch } from "../api/fetch";
import { PLATFORM_BASE, type WhoAmI } from "../auth/types";
import {
  isActivitySummaryForTask,
  isRunningActivitySummary,
  rehydrateMessages,
  upsertActivityMessage,
  type ChatMessage,
  type CredentialRequestPayload,
  type SetupRequestPayload,
} from "../lib/rehydrate";
import type { Attachment } from "../lib/attachments";
import {
  publishTaskEvent,
  type TaskEventKind,
} from "../lib/task-events";

const TASK_LIFECYCLE_EVENTS: ReadonlySet<string> = new Set<TaskEventKind>([
  "task:created",
  "task:started",
  "task:completed",
  "task:cancelled",
  "task:failed",
  "task:activity",
]);

type Conversation = {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
};

type MachineIdentity = {
  id: string;
  name: string;
  host: string;
};

export function nextConversationAfterLoad(
  current: string | null,
  loaded: readonly { id: string }[],
  suppressAutoSelect: boolean,
): string | null {
  if (current) return current;
  if (suppressAutoSelect) return null;
  return loaded[0]?.id ?? null;
}

export function ChatLayout({
  who,
  initialConversation,
}: {
  who: WhoAmI;
  initialConversation?: string | null;
}) {
  const navigate = useNavigate();
  const location = useLocation();
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [activeConversation, setActiveConversation] = useState<string | null>(
    initialConversation ?? null,
  );
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [agentStatus, setAgentStatus] = useState<"online" | "working">(
    "online",
  );
  const [runningTaskId, setRunningTaskId] = useState<string | null>(null);
  const [runningTaskConversationId, setRunningTaskConversationId] = useState<
    string | null
  >(null);
  const [stopping, setStopping] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // A starter prompt tapped in the empty state, handed to the composer to
  // fill (not send). The bumped nonce refills even when the text repeats.
  const [composerPrefill, setComposerPrefill] = useState<
    { text: string; nonce: number } | undefined
  >(undefined);
  const [vaultNames, setVaultNames] = useState<string[]>([]);
  // Live presence of users connected to this machine's SSE stream.
  // Populated by the daemon's `presence` event after every sub/unsub
  // and once on initial connect. The current user is filtered out in
  // PresenceAvatars so we only render "others are here" signals.
  const [presence, setPresence] = useState<PresenceUser[]>([]);
  const [machineIdentity, setMachineIdentity] =
    useState<MachineIdentity | null>(null);
  const sseReadyRef = useRef(false);
  const [conversationsLoaded, setConversationsLoaded] = useState(false);
  const [messagesLoading, setMessagesLoading] = useState(false);
  const fetchVaultNamesRef = useRef<() => Promise<void>>(async () => {});
  const activeConversationRef = useRef<string | null>(activeConversation);
  useEffect(() => {
    activeConversationRef.current = activeConversation;
  }, [activeConversation]);
  const suppressAutoSelectRef = useRef(false);
  const messagesRef = useRef<ChatMessage[]>(messages);
  useEffect(() => {
    messagesRef.current = messages;
  }, [messages]);
  const [rehydrateTrigger, setRehydrateTrigger] = useState(0);

  const isViewOnly = who.access === "view";
  const canControl = who.access === "control";
  const machineHost =
    machineIdentity?.host ||
    (typeof window !== "undefined" ? window.location.host : "");
  const machineLabel = machineIdentity?.name || machineHost;

  useEffect(() => {
    const controller = new AbortController();
    apiFetch("/api/machine", { signal: controller.signal })
      .then((res) => {
        if (!res.ok) throw new Error("Failed to load machine identity");
        return res.json() as Promise<MachineIdentity>;
      })
      .then((identity) => setMachineIdentity(identity))
      .catch(() => {
        // Non-critical. The host remains a stable fallback.
      });
    return () => controller.abort();
  }, []);

  useEffect(() => {
    function onMachineNameUpdated(event: Event) {
      const detail = (event as CustomEvent<MachineIdentity>).detail;
      if (detail?.id) setMachineIdentity(detail);
    }
    window.addEventListener("vibecraft:machine-name-updated", onMachineNameUpdated);
    return () =>
      window.removeEventListener(
        "vibecraft:machine-name-updated",
        onMachineNameUpdated,
      );
  }, []);

  // Sync activeConversation to URL — keeps /c/{id} reflecting state.
  useEffect(() => {
    if (activeConversation && location.pathname !== `/c/${activeConversation}`) {
      navigate(`/c/${activeConversation}`, { replace: true });
    } else if (!activeConversation && location.pathname.startsWith("/c/")) {
      navigate("/", { replace: true });
    }
  }, [activeConversation, location.pathname, navigate]);

  const fetchVaultNames = useCallback(async () => {
    try {
      const res = await apiFetch("/api/vault");
      if (!res.ok) return;
      const data = (await res.json()) as Array<{ name?: string }> | null;
      if (Array.isArray(data)) {
        setVaultNames(
          data.map((s) => s?.name ?? "").filter((n) => n.length > 0),
        );
      }
    } catch {
      // Non-critical
    }
  }, []);

  const fetchConversations = useCallback(async (signal?: AbortSignal) => {
    try {
      const res = await apiFetch("/api/conversations", { signal });
      const data = await res.json();
      if (Array.isArray(data)) {
        setConversations(data);
        if (data.length > 0) {
          setActiveConversation((prev) =>
            nextConversationAfterLoad(prev, data, suppressAutoSelectRef.current),
          );
        }
      }
    } catch {
      // Abort errors ignored
    } finally {
      setConversationsLoaded(true);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    fetchConversations(controller.signal);
    fetchVaultNames();
    return () => controller.abort();
  }, [fetchConversations, fetchVaultNames]);

  useEffect(() => {
    fetchVaultNamesRef.current = fetchVaultNames;
  }, [fetchVaultNames]);

  // SSE stream — same-origin, cookie-authenticated. Reconnects with
  // exponential backoff. Replays any waiting tasks on reconnect via the
  // daemon's bootstrap event sequence.
  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    let retryDelay = 1000;

    let currentEvent = "";
    let currentData = "";

    function handleSSEEvent(event: string, data: Record<string, unknown>) {
      if (TASK_LIFECYCLE_EVENTS.has(event)) {
        publishTaskEvent({
          kind: event as TaskEventKind,
          taskId: data.task_id as string | undefined,
          conversationId: data.conversation_id as string | undefined,
          status: data.status as string | undefined,
        });
      }

      switch (event) {
        case "presence": {
          const users = (data as { users?: PresenceUser[] }).users;
          if (Array.isArray(users)) setPresence(users);
          break;
        }
        case "task:started":
        case "task:thinking": {
          const tid = data.task_id as string | undefined;
          const cid = data.conversation_id as string | undefined;
          if (tid) setRunningTaskId(tid);
          if (cid) setRunningTaskConversationId(cid);
          setAgentStatus("working");
          break;
        }
        case "task:completed": {
          const result = data.result as string;
          const completedId = data.task_id as string | undefined;
          setMessages((prev) => {
            const updated = completedId
              ? prev.map((m) =>
                  m.type === "approval" &&
                  m.id === completedId &&
                  !m.approvalResolution
                    ? {
                        ...m,
                        approvalResolution: "decided" as const,
                        actions: undefined,
                      }
                    : m,
                )
              : prev;
            if (!result) return updated;
            return [
              ...updated,
              {
                id: `sse-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
                role: "agent",
                type: "text",
                content: result,
                timestamp: new Date().toISOString(),
              },
            ];
          });
          setAgentStatus("online");
          setRunningTaskId(null);
          setRunningTaskConversationId(null);
          setStopping(false);

          if (completedId) {
            const convAtComplete = activeConversationRef.current;
            setTimeout(() => {
              if (!convAtComplete) return;
              if (activeConversationRef.current !== convAtComplete) return;
              const hasSettledActivity = messagesRef.current.some(
                (m) =>
                  isActivitySummaryForTask(m, completedId) &&
                  !isRunningActivitySummary(m.content),
              );
              if (hasSettledActivity) return;
              setRehydrateTrigger((t) => t + 1);
            }, 6000);
          }
          break;
        }
        case "task:failed": {
          const errMsg = (data.error as string) || "Task failed";
          const failedId = data.task_id as string | undefined;
          setMessages((prev) => {
            const updated = failedId
              ? prev.map((m) => {
                  if (
                    m.type === "approval" &&
                    m.id === failedId &&
                    !m.approvalResolution
                  ) {
                    return {
                      ...m,
                      approvalResolution: "decided" as const,
                      actions: undefined,
                    };
                  }
                  if (
                    m.type === "credential_request" &&
                    m.credentialRequest &&
                    (m.credentialRequest.task_id === failedId ||
                      m.taskId === failedId) &&
                    !m.credentialRequest.stored &&
                    !m.credentialRequest.expired_at
                  ) {
                    return {
                      ...m,
                      credentialRequest: {
                        ...m.credentialRequest,
                        expired_at: new Date().toISOString(),
                        expired_reason: "task ended",
                      },
                    };
                  }
                  return m;
                })
              : prev;
            return [
              ...updated,
              {
                id: `sse-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
                role: "agent",
                type: "text",
                content: errMsg,
                timestamp: new Date().toISOString(),
              },
            ];
          });
          setAgentStatus("online");
          setRunningTaskId(null);
          setRunningTaskConversationId(null);
          setStopping(false);
          break;
        }
        case "task:credentials_expired": {
          const expiredMsgId = (data.message_id as string) || "";
          const expiredTaskId = (data.task_id as string) || "";
          const reason = (data.reason as string) || "task ended";
          if (!expiredMsgId && !expiredTaskId) break;
          setMessages((prev) =>
            prev.map((m) => {
              if (m.type !== "credential_request" || !m.credentialRequest) {
                return m;
              }
              const matchesId = expiredMsgId && m.id === expiredMsgId;
              const matchesTask =
                expiredTaskId &&
                (m.credentialRequest.task_id === expiredTaskId ||
                  m.taskId === expiredTaskId);
              if (!matchesId && !matchesTask) return m;
              if (
                m.credentialRequest.stored ||
                m.credentialRequest.expired_at
              ) {
                return m;
              }
              return {
                ...m,
                credentialRequest: {
                  ...m.credentialRequest,
                  expired_at: new Date().toISOString(),
                  expired_reason: reason,
                },
              };
            }),
          );
          break;
        }
        case "task:approval_expired": {
          // Approval-card analog of task:credentials_expired. The daemon
          // emits this when an approval card is invalidated without a
          // user answer — 24h timeout, parent task ended, or the
          // customer redirected attention to another conversation. We
          // flip the card to its expired-receipt state so a user
          // returning to this conversation sees what happened.
          const expiredMsgId = (data.message_id as string) || "";
          const expiredTaskId = (data.task_id as string) || "";
          const reason = (data.reason as string) || "task ended";
          if (!expiredMsgId && !expiredTaskId) break;
          setMessages((prev) =>
            prev.map((m) => {
              if (m.type !== "approval") return m;
              const matchesId = expiredMsgId && m.id === expiredMsgId;
              const matchesTask =
                expiredTaskId &&
                (m.id === expiredTaskId || m.taskId === expiredTaskId);
              if (!matchesId && !matchesTask) return m;
              if (m.approvalResolution) return m;
              return {
                ...m,
                approvalResolution: "expired" as const,
                approvalExpiredReason: reason,
                actions: undefined,
              };
            }),
          );
          break;
        }
        case "task:cancelled": {
          const cancelledId = data.task_id as string | undefined;
          setMessages((prev) => {
            const filtered = cancelledId
              ? prev.filter(
                  (m) => !(m.type === "approval" && m.id === cancelledId),
                )
              : prev;
            const updated = cancelledId
              ? filtered.map((m) => {
                  if (
                    m.type === "credential_request" &&
                    m.credentialRequest &&
                    (m.credentialRequest.task_id === cancelledId ||
                      m.taskId === cancelledId) &&
                    !m.credentialRequest.stored &&
                    !m.credentialRequest.expired_at
                  ) {
                    return {
                      ...m,
                      credentialRequest: {
                        ...m.credentialRequest,
                        expired_at: new Date().toISOString(),
                        expired_reason: "task ended",
                      },
                    };
                  }
                  return m;
                })
              : filtered;
            return [
              ...updated,
              {
                id: `stop-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
                role: "system",
                type: "system_notice",
                content: "Task stopped",
                taskId: cancelledId,
                timestamp: new Date().toISOString(),
              },
            ];
          });
          setAgentStatus("online");
          setRunningTaskId(null);
          setRunningTaskConversationId(null);
          setStopping(false);
          break;
        }
        case "task:waiting": {
          const question = data.question as string;
          const taskId = data.task_id as string;
          const approval = data.approval as
            | {
                tool?: string;
                command?: string;
                title?: string;
                reason?: string;
                severity?: "escalate" | "soft_deny";
                friction?: "none" | "delay" | "type_to_confirm";
                type_target?: string;
              }
            | undefined;
          if ((question || approval?.title) && taskId) {
            setMessages((prev) => {
              if (
                prev.some((m) => m.id === taskId && m.type === "approval")
              ) {
                return prev;
              }
              return [
                ...prev,
                {
                  id: taskId,
                  role: "agent",
                  type: "approval",
                  content: question,
                  description: question,
                  approvalTitle: approval?.title,
                  approvalReason: approval?.reason,
                  approvalCommand: approval?.command,
                  approvalSeverity: approval?.severity ?? "escalate",
                  approvalFriction: approval?.friction ?? "none",
                  approvalTypeTarget: approval?.type_target,
                  actions: [
                    { label: "Approve", action: "approve" },
                    { label: "Deny", action: "cancel" },
                  ],
                  timestamp: new Date().toISOString(),
                },
              ];
            });
          }
          break;
        }
        case "task:auto_approved": {
          const taskId = typeof data.task_id === "string" ? data.task_id : "";
          const title =
            typeof data.title === "string" ? data.title : "Auto-approved action";
          const reason = typeof data.reason === "string" ? data.reason : "";
          const command =
            typeof data.command === "string" ? data.command : "";
          const timestamp =
            typeof data.timestamp === "string"
              ? data.timestamp
              : new Date().toISOString();
          setMessages((prev) => [
            ...prev,
            {
              id: `aa-${taskId || "x"}-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
              role: "agent",
              type: "auto_approved",
              content: title,
              autoApproveReason: reason,
              autoApproveCommand: command,
              taskId: taskId || undefined,
              timestamp,
            },
          ]);
          break;
        }
        case "task:screenshot": {
          const imageBase64 = data.image as string;
          const caption =
            typeof data.caption === "string" ? data.caption : "";
          const taskIdStr =
            typeof data.task_id === "string" ? data.task_id : undefined;
          if (imageBase64) {
            setMessages((prev) => [
              ...prev,
              {
                id: `ss-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
                role: "agent",
                type: "screenshot",
                content: caption,
                imageUrl: `data:image/png;base64,${imageBase64}`,
                taskId: taskIdStr,
                timestamp: new Date().toISOString(),
              },
            ]);
          }
          break;
        }
        case "task:message": {
          const content = data.content as string;
          const timestamp =
            (data.timestamp as string) || new Date().toISOString();
          if (content) {
            setMessages((prev) => [
              ...prev,
              {
                id: `msg-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
                role: "agent",
                type: "text",
                content,
                timestamp,
              },
            ]);
          }
          break;
        }
        case "task:activity":
        case "task:progress": {
          const taskId = data.task_id as string;
          const summaryMarkdown = data.summary_markdown as string;
          const stepCount = data.step_count as number | undefined;
          const timestamp =
            (data.timestamp as string) || new Date().toISOString();
          if (!summaryMarkdown || !taskId) break;
          setMessages((prev) =>
            upsertActivityMessage(prev, {
              taskId,
              summaryMarkdown,
              stepCount,
              timestamp,
            }),
          );
          break;
        }
        case "task:resumed": {
          const resumedId = data.task_id as string | undefined;
          if (resumedId) {
            setMessages((prev) =>
              prev.map((m) =>
                m.type === "approval" &&
                m.id === resumedId &&
                !m.approvalResolution
                  ? {
                      ...m,
                      approvalResolution: "decided" as const,
                      actions: undefined,
                    }
                  : m,
              ),
            );
          }
          setAgentStatus("working");
          break;
        }
        case "task:credentials_requested": {
          const messageId = data.message_id as string | undefined;
          const payload = data.payload as CredentialRequestPayload | undefined;
          if (!messageId || !payload) break;
          setMessages((prev) => {
            if (prev.some((m) => m.id === messageId)) return prev;
            return [
              ...prev,
              {
                id: messageId,
                role: "agent",
                type: "credential_request",
                content: JSON.stringify(payload),
                credentialRequest: payload,
                taskId: payload.task_id,
                timestamp: new Date().toISOString(),
              },
            ];
          });
          break;
        }
        case "task:credentials_stored": {
          const messageId = data.message_id as string | undefined;
          const payload = data.payload as CredentialRequestPayload | undefined;
          if (!messageId || !payload) break;
          setMessages((prev) =>
            prev.map((m) =>
              m.id === messageId && m.type === "credential_request"
                ? {
                    ...m,
                    credentialRequest: payload,
                    content: JSON.stringify(payload),
                  }
                : m,
            ),
          );
          fetchVaultNamesRef.current();
          break;
        }
      }
    }

    async function connectSSE() {
      while (!cancelled) {
        try {
          // eslint-disable-next-line no-restricted-syntax -- SSE stream: a long-lived streaming Response; apiFetch's 401-retry would consume the body and break the reader
          const res = await fetch("/api/stream", {
            credentials: "include",
            signal: controller.signal,
          });
          if (!res.ok || !res.body) {
            if (cancelled) return;
            await new Promise((r) => setTimeout(r, retryDelay));
            retryDelay = Math.min(retryDelay * 2, 30000);
            continue;
          }
          retryDelay = 1000;
          const reader = res.body.getReader();
          const decoder = new TextDecoder();
          let buffer = "";
          sseReadyRef.current = true;

          while (!cancelled) {
            const { done, value } = await reader.read();
            if (done) break;

            buffer += decoder.decode(value, { stream: true });
            const lines = buffer.split("\n");
            buffer = lines.pop() || "";

            for (const line of lines) {
              if (line.startsWith("event: ")) {
                currentEvent = line.slice(7).trim();
              } else if (line.startsWith("data: ")) {
                currentData += (currentData ? "\n" : "") + line.slice(6);
              } else if (line === "") {
                if (currentData) {
                  try {
                    const data = JSON.parse(currentData);
                    handleSSEEvent(currentEvent, data);
                  } catch {
                    // Ignore malformed events
                  }
                }
                currentEvent = "";
                currentData = "";
              }
            }
          }

          sseReadyRef.current = false;
          if (!cancelled) {
            await new Promise((r) => setTimeout(r, retryDelay));
          }
        } catch {
          sseReadyRef.current = false;
          if (!cancelled) {
            await new Promise((r) => setTimeout(r, retryDelay));
            retryDelay = Math.min(retryDelay * 2, 30000);
          }
        }
      }
      sseReadyRef.current = false;
    }

    connectSSE();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, []);

  // Fetch messages when conversation changes.
  useEffect(() => {
    if (!activeConversation) return;

    setMessagesLoading(true);
    const controller = new AbortController();

    apiFetch(`/api/conversations/${activeConversation}`, {
      signal: controller.signal,
    })
      .then(async (res) => {
        if (!res.ok) return;
        const data = await res.json();
        setMessages(rehydrateMessages(data));
      })
      .catch(() => {})
      .finally(() => {
        if (!controller.signal.aborted) setMessagesLoading(false);
      });

    return () => controller.abort();
  }, [activeConversation, rehydrateTrigger]);

  const onSelectConversation = useCallback((id: string) => {
    suppressAutoSelectRef.current = false;
    setActiveConversation(id);
    setSidebarOpen(false);
  }, []);

  // Cross-conversation "queued behind" indicator. When the engine is
  // occupied on a task in a different conversation than the one the
  // user is looking at, the current chat shows a clear panel naming the
  // blocking conversation instead of the misleading "Working…" dots.
  // Same-conversation runs and the idle case are null.
  //
  // Engine semantics (post v0.71): a fresh CreateTask in a DIFFERENT
  // conversation auto-expires any open approval/credential card via
  // NotifyNewTaskInConversation, so this state only persists while a
  // genuine task is RUNNING elsewhere. The user posting in this conv
  // is effectively a queue join — short-lived, surfaced honestly.
  const queuedBehind = useMemo<{ id: string; title: string } | null>(() => {
    if (!runningTaskConversationId || !activeConversation) return null;
    if (runningTaskConversationId === activeConversation) return null;
    const blocking = conversations.find(
      (c) => c.id === runningTaskConversationId,
    );
    const title = blocking?.title?.trim() || "another conversation";
    return { id: runningTaskConversationId, title };
  }, [runningTaskConversationId, activeConversation, conversations]);

  const onOpenSettings = useCallback(() => {
    navigate("/settings");
  }, [navigate]);

  const stopTask = useCallback(
    async (taskId?: string | null) => {
      const id = taskId ?? runningTaskId;
      if (!id) return;
      setStopping(true);
      window.setTimeout(() => setStopping(false), 8000);
      try {
        const res = await apiFetch(`/api/task/${id}/cancel`, { method: "POST" });
        if (!res.ok) setStopping(false);
      } catch {
        setStopping(false);
      }
    },
    [runningTaskId],
  );

  const ensureConversationId = useCallback((): string => {
    if (activeConversation) return activeConversation;
    const id = crypto.randomUUID();
    suppressAutoSelectRef.current = false;
    setActiveConversation(id);
    return id;
  }, [activeConversation]);

  async function sendMessage(content: string, attachments: Attachment[] = []) {
    if (isViewOnly) return;
    if (!content && attachments.length === 0) return;

    // Wait briefly for the SSE reader to be active before posting.
    if (!sseReadyRef.current) {
      const deadline = Date.now() + 3000;
      while (!sseReadyRef.current && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 50));
      }
    }

    const convId = activeConversation ?? ensureConversationId();

    if (
      agentStatus === "working" &&
      runningTaskId &&
      runningTaskConversationId === convId
    ) {
      stopTask(runningTaskId);
    }

    const userMessage: ChatMessage = {
      id: `msg-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      role: "user",
      type: "text",
      content,
      attachments: attachments.length > 0 ? attachments : undefined,
      timestamp: new Date().toISOString(),
    };
    setMessages((prev) => [...prev, userMessage]);
    setAgentStatus("working");

    try {
      const res = await apiFetch("/api/task", {
        method: "POST",
        body: JSON.stringify({
          instruction: content,
          conversation_id: convId,
          attachments,
        }),
      });

      if (res.ok) {
        const data = await res.json().catch(() => ({}));
        // Task gates: the daemon held this task before creating work
        // (budget spent, or the manager key is not configured). It
        // already persisted the user message + a manager reply to the
        // conversation. Keep the optimistic user bubble, append the
        // explanation, and stand down — there's no task/SSE to wait for.
        if (data.budget_blocked || data.manager_unavailable) {
          const heldMessages: ChatMessage[] = [];
          if (typeof data.local_answer === "string" && data.local_answer.trim()) {
            heldMessages.push({
              id: `held-local-${Date.now()}`,
              role: "agent" as const,
              type: "text" as const,
              content: data.local_answer,
              timestamp: new Date().toISOString(),
            });
          }
          if (data.setup_required && typeof data.setup_required === "object") {
            heldMessages.push({
              id: `setup-${Date.now()}`,
              role: "agent" as const,
              type: "setup_request" as const,
              content: JSON.stringify(data.setup_required),
              setupRequest: data.setup_required as SetupRequestPayload,
              timestamp: new Date().toISOString(),
            });
          } else {
            heldMessages.push({
              id: `held-${Date.now()}`,
              role: "agent" as const,
              type: "text" as const,
              content:
                data.message ||
                "New tasks are paused until this computer is ready.",
              timestamp: new Date().toISOString(),
            });
          }
          setMessages((prev) => [...prev, ...heldMessages]);
          setAgentStatus("online");
          return;
        }
        fetchConversations();
      } else {
        const data = await res.json().catch(() => ({}));
        setMessages((prev) => [
          ...prev.filter((m) => m.id !== userMessage.id),
          {
            id: `err-${Date.now()}`,
            role: "agent" as const,
            type: "text" as const,
            content:
              data.error || "Failed to send message. Please try again.",
            timestamp: new Date().toISOString(),
          },
        ]);
        setAgentStatus("online");
      }
    } catch {
      setMessages((prev) => [
        ...prev.filter((m) => m.id !== userMessage.id),
        {
          id: `err-${Date.now()}`,
          role: "agent" as const,
          type: "text" as const,
          content: "Connection error. Please try again.",
          timestamp: new Date().toISOString(),
        },
      ]);
      setAgentStatus("online");
    }
  }

  async function handleApproval(messageId: string, action: string) {
    const resolution: "approved" | "denied" =
      action === "approve" ? "approved" : "denied";
    setMessages((prev) =>
      prev.map((m) =>
        m.type === "approval" && m.id === messageId
          ? { ...m, approvalResolution: resolution, actions: undefined }
          : m,
      ),
    );

    try {
      await apiFetch(`/api/task/${messageId}/respond`, {
        method: "POST",
        body: JSON.stringify({ input: action === "approve" ? "yes" : "no" }),
      });
    } catch {
      // Error surfaces via SSE.
    }
  }

  function newConversation() {
    suppressAutoSelectRef.current = true;
    setActiveConversation(null);
    setMessages([]);
  }

  return (
    <div className="flex h-screen flex-col overflow-hidden bg-[#f4f3ee]">
      <div className="flex min-h-0 flex-1">
        <button
          onClick={() => setSidebarOpen(!sidebarOpen)}
          aria-label={sidebarOpen ? "Close sidebar" : "Open sidebar"}
          className="fixed bottom-4 left-4 z-40 flex h-10 w-10 items-center justify-center rounded-full bg-slate-950 text-white shadow-lg sm:hidden"
        >
          {sidebarOpen ? (
            <X className="h-5 w-5" aria-hidden="true" />
          ) : (
            <Menu className="h-5 w-5" aria-hidden="true" />
          )}
        </button>

        <aside
          className={`${
            sidebarOpen ? "translate-x-0" : "-translate-x-full"
          } fixed inset-y-0 left-0 z-30 w-64 border-r border-slate-200/60 bg-white/30 transition-transform sm:relative sm:translate-x-0`}
        >
          <ConversationSidebar
            conversations={conversations}
            activeConversation={activeConversation}
            onSelect={onSelectConversation}
            onNew={newConversation}
            onSettings={onOpenSettings}
            loading={!conversationsLoaded}
          />
        </aside>

        <div className="flex flex-1 flex-col">
          <header className="flex items-center justify-between border-b border-slate-200/60 px-4 py-3 sm:px-6">
            <div className="flex items-center gap-3">
              <a
                href={PLATFORM_BASE + "/dashboard"}
                className="flex items-center gap-0.5 font-poppins text-sm font-semibold text-slate-950 transition-colors hover:text-slate-600"
              >
                <ChevronLeft
                  className="h-4 w-4 text-slate-400"
                  aria-hidden="true"
                />
                All machines
              </a>
              <span className="text-xs text-slate-300" aria-hidden="true">
                |
              </span>
              <div className="min-w-0" title={machineHost}>
                <div className="truncate text-xs font-medium text-slate-700">
                  {machineLabel}
                </div>
                {machineHost && machineHost !== machineLabel && (
                  <div className="truncate font-mono text-[10px] text-slate-400">
                    {machineHost}
                  </div>
                )}
              </div>
              <div
                className="flex items-center gap-1.5"
                role="status"
                aria-live="polite"
              >
                <div
                  className={`h-2 w-2 rounded-full ${
                    agentStatus === "working"
                      ? "animate-pulse bg-yellow-400"
                      : "bg-green-500"
                  }`}
                  aria-hidden="true"
                />
                <span className="text-xs text-slate-500">
                  {agentStatus === "working" ? "Working..." : "Online"}
                </span>
              </div>
              {agentStatus === "working" &&
                runningTaskId &&
                !isViewOnly &&
                canControl && (
                  <button
                    type="button"
                    onClick={() => stopTask()}
                    disabled={stopping}
                    aria-label={stopping ? "Stopping task" : "Stop task"}
                    title="Stop task (Cmd/Ctrl + .)"
                    className="inline-flex items-center gap-1 rounded-full border border-slate-200/60 bg-white/70 px-2.5 py-0.5 text-[11px] font-medium text-slate-600 transition hover:border-slate-300 hover:text-slate-950 disabled:opacity-60"
                  >
                    {stopping ? (
                      <span
                        className="h-2.5 w-2.5 animate-spin rounded-full border-[1.5px] border-slate-300 border-t-slate-600"
                        aria-hidden="true"
                      />
                    ) : (
                      <Square
                        className="h-2.5 w-2.5 fill-current"
                        aria-hidden="true"
                      />
                    )}
                    {stopping ? "Stopping" : "Stop"}
                  </button>
                )}
              {isViewOnly && (
                <span className="flex items-center gap-1 rounded-full bg-slate-100 px-2 py-0.5 text-[10px] font-medium text-slate-500">
                  <Eye className="h-3 w-3" aria-hidden="true" />
                  Viewing
                </span>
              )}
            </div>

            <div className="flex items-center gap-3">
              <PresenceAvatars users={presence} selfUserId={who.sub} />
              <NotificationBell />
              <span className="text-xs text-slate-500">
                {who.name || who.email}
              </span>
            </div>
          </header>

          <ChatThread
            messages={messages}
            onApproval={handleApproval}
            isWorking={agentStatus === "working"}
            loading={!conversationsLoaded || messagesLoading}
            conversationId={activeConversation}
            vaultNames={vaultNames}
            queuedBehind={queuedBehind}
            onOpenBlocker={onSelectConversation}
            onStopBlocker={
              !isViewOnly && canControl && runningTaskId
                ? () => stopTask(runningTaskId)
                : undefined
            }
            blockerStopping={stopping}
            onPickPrompt={
              !isViewOnly
                ? (text) =>
                    setComposerPrefill((p) => ({
                      text,
                      nonce: (p?.nonce ?? 0) + 1,
                    }))
                : undefined
            }
          />

          {!isViewOnly && (
            <ChatInput
              onSend={sendMessage}
              onStop={() => stopTask()}
              isWorking={Boolean(
                agentStatus === "working" &&
                  runningTaskId &&
                  activeConversation &&
                  runningTaskConversationId === activeConversation,
              )}
              stopping={stopping}
              conversationId={activeConversation}
              ensureConversationId={ensureConversationId}
              prefill={composerPrefill}
            />
          )}
        </div>
      </div>
    </div>
  );
}
