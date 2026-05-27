// SPDX-License-Identifier: Apache-2.0

import {
  decodeSetupRequest,
  isActivitySummaryForTask,
  isRunningActivitySummary,
  rehydrateMessages,
  upsertActivityMessage,
} from "./rehydrate";

describe("rehydrateMessages", () => {
  it("returns an empty list for empty input", () => {
    expect(rehydrateMessages({})).toEqual([]);
    expect(rehydrateMessages({ messages: [], tasks: [] })).toEqual([]);
  });

  it("rehydrates a setup_request card", () => {
    const payload = {
      code: "operator_openai_key_required",
      title: "Connect this computer's manager",
      body: "This connected computer needs your OpenAI API key before the manager can run tasks.",
      primary_label: "Save key",
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "setup-1",
          role: "assistant",
          type: "setup_request",
          content: JSON.stringify(payload),
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });

    expect(out[0].type).toBe("setup_request");
    expect(out[0].setupRequest?.code).toBe("operator_openai_key_required");
    expect(out[0].setupRequest?.message_id).toBe("setup-1");
  });

  it("turns the legacy raw manager-key repair text into a setup card", () => {
    const legacy =
      "OpenAI manager key is not configured. Add an operator-owned key to /etc/vibecraft/openai.key, set /etc/vibecraft/manager_key_mode to operator, and restart vibecraft-daemon.";
    const payload = decodeSetupRequest(legacy, "m-legacy");
    expect(payload?.code).toBe("operator_openai_key_required");

    const out = rehydrateMessages({
      messages: [
        {
          id: "m-legacy",
          role: "assistant",
          type: "text",
          content: legacy,
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });

    expect(out[0].type).toBe("setup_request");
    expect(out[0].content).not.toContain("/etc/vibecraft/openai.key");
  });

  it("parses a valid attachments array onto the message", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "m1",
          role: "user",
          type: "text",
          content: "look at this",
          attachments: [
            {
              name: "stored-photo.png",
              path: "/home/vibecraft/inbox/c/stored-photo.png",
              mime: "image/png",
              size: 4242,
              original: "photo.png",
            },
          ],
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out[0].attachments).toHaveLength(1);
    expect(out[0].attachments?.[0].original).toBe("photo.png");
    expect(out[0].attachments?.[0].mime).toBe("image/png");
  });

  it("drops malformed attachment entries and leaves a missing field undefined", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "m1",
          role: "user",
          type: "text",
          content: "garbage",
          attachments: [
            { name: "", path: "/x" }, // dropped: name is empty
            { name: "ok.pdf", path: "", mime: "application/pdf" }, // dropped: path is empty
            { name: "real.pdf", path: "/home/vibecraft/inbox/c/real.pdf", mime: "application/pdf", size: 10 },
          ],
          created_at: "2026-01-01T00:00:00Z",
        },
        {
          id: "m2",
          role: "user",
          type: "text",
          content: "clean",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out[0].attachments).toHaveLength(1);
    expect(out[0].attachments?.[0].name).toBe("real.pdf");
    expect(out[1].attachments).toBeUndefined();
  });

  it("normalizes role assistant → agent", () => {
    const out = rehydrateMessages({
      messages: [
        { id: "m1", role: "assistant", content: "hi", type: "text", created_at: "2026-01-01T00:00:00Z" },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].role).toBe("agent");
    expect(out[0].content).toBe("hi");
    expect(out[0].timestamp).toBe("2026-01-01T00:00:00Z");
  });

  it("wraps image_data as a data URL", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "m1",
          role: "assistant",
          type: "screenshot",
          content: "",
          image_data: "SGVsbG8=",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out[0].type).toBe("screenshot");
    expect(out[0].imageUrl).toBe("data:image/png;base64,SGVsbG8=");
  });

  it("stamps activity_summary messages with a rehydrated taskId when the marker is present", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "a1",
          role: "assistant",
          type: "activity_summary",
          content: "<!-- steps:5 -->\nDid stuff",
          created_at: "2026-01-01T00:00:00Z",
        },
        {
          id: "a2",
          role: "assistant",
          type: "activity_summary",
          content: "Did other stuff with no marker",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out[0].taskId).toBe("rehydrated-a1");
    expect(out[1].taskId).toBeUndefined();
  });

  it("detects running activity placeholders for live replacement", () => {
    expect(
      isRunningActivitySummary(
        "<!-- steps:0 -->\nTask: do work\nStatus: running\nDuration: 0s",
      ),
    ).toBe(true);
    expect(
      isRunningActivitySummary(
        "<!-- steps:4 -->\nTask: do work\nStatus: completed\nDuration: 2s",
      ),
    ).toBe(false);
  });

  it("matches activity summaries to their task ids", () => {
    const [message] = rehydrateMessages({
      messages: [
        {
          id: "act-task-1",
          role: "assistant",
          type: "activity_summary",
          content: "<!-- steps:0 -->\nTask: do work\nStatus: running",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });

    expect(isActivitySummaryForTask(message, "task-1")).toBe(true);
    expect(isActivitySummaryForTask(message, "task-2")).toBe(false);
  });

  it("moves live activity updates to the bottom instead of leaving Done above later messages", () => {
    const out = upsertActivityMessage(
      [
        {
          id: "act-task-1",
          role: "agent",
          type: "activity_summary",
          content: "<!-- steps:0 -->\nTask: do work\nStatus: running",
          taskId: "task-1",
          timestamp: "2026-01-01T00:00:00Z",
        },
        {
          id: "m-after",
          role: "agent",
          type: "text",
          content: "Waiting for Codex to respond.",
          timestamp: "2026-01-01T00:00:01Z",
        },
      ],
      {
        taskId: "task-1",
        summaryMarkdown: "Task: do work\nStatus: completed\nDuration: 16s",
        stepCount: 7,
        timestamp: "2026-01-01T00:00:02Z",
      },
    );

    expect(out.map((m) => m.id)).toEqual(["m-after", "act-task-1"]);
    expect(out[1].content).toContain("Status: completed");
    expect(out[1].content).toContain("<!-- steps:7 -->");
  });

  it("reconstructs a running work card from a running task on reload", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-user",
          role: "user",
          type: "text",
          content: "Open Codex and ask it something",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
      tasks: [
        {
          id: "task-running",
          instruction: "Open Codex and ask it something",
          status: "running",
          updated_at: "2026-01-01T00:00:02Z",
        },
      ],
    });

    expect(out).toHaveLength(2);
    expect(out[1].id).toBe("act-task-running");
    expect(out[1].type).toBe("activity_summary");
    expect(out[1].content).toContain("Status: running");
    expect(out[1].content).toContain("Current: Manager is working.");
  });

  it("reconstructs an approval card from a legacy plain-text task result", () => {
    const out = rehydrateMessages({
      messages: [
        { id: "m1", role: "user", content: "kill the daemon", type: "text", created_at: "2026-01-01T00:00:00Z" },
      ],
      tasks: [
        {
          id: "task-123",
          status: "waiting_for_input",
          result: "The agent wants to: bash\nCommand: kill 1234\nApprove? (yes/no)",
          updated_at: "2026-01-01T00:01:00Z",
        },
      ],
    });
    expect(out).toHaveLength(2);
    const approval = out[1];
    expect(approval.type).toBe("approval");
    expect(approval.id).toBe("task-123"); // must equal task id so SSE dedupe works
    expect(approval.role).toBe("agent");
    expect(approval.content).toMatch(/kill 1234/);
    // Legacy format has no structured title/reason/command — description
    // is the raw text so the card can still render a fallback view.
    expect(approval.description).toBe(approval.content);
    expect(approval.approvalTitle).toBeUndefined();
    expect(approval.approvalCommand).toBeUndefined();
    expect(approval.timestamp).toBe("2026-01-01T00:01:00Z");
    expect(approval.actions).toEqual([
      { label: "Approve", action: "approve" },
      { label: "Deny", action: "cancel" },
    ]);
  });

  it("decodes a JSON ApprovalPayload into structured fields", () => {
    const payload = {
      tool: "bash",
      command: "sudo apt remove nginx",
      title: "Remove software: nginx",
      reason: "this removes software from the machine",
    };
    const out = rehydrateMessages({
      tasks: [
        {
          id: "task-json",
          status: "waiting_for_input",
          result: JSON.stringify(payload),
          updated_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    const approval = out[0];
    expect(approval.type).toBe("approval");
    expect(approval.approvalTitle).toBe("Remove software: nginx");
    expect(approval.approvalReason).toBe("this removes software from the machine");
    expect(approval.approvalCommand).toBe("sudo apt remove nginx");
    // A readable description is synthesized from the payload so any
    // consumer that hasn't wired structured props still sees something.
    expect(approval.description).toContain("Remove software: nginx");
    expect(approval.description).toContain("sudo apt remove nginx");
  });

  it("decodes a persisted approval MESSAGE with JSON content", () => {
    // When the daemon persists the approval prompt, the message row's
    // content is the ApprovalPayload JSON. Rehydrate should still expose
    // structured fields so the card renders the polished view on reload.
    const payload = {
      tool: "bash",
      command: "sudo apt install -y htop",
      title: "Install htop",
      reason: "installing packages requires confirmation",
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "task-persist",
          role: "assistant",
          type: "approval",
          content: JSON.stringify(payload),
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].approvalTitle).toBe("Install htop");
    expect(out[0].approvalCommand).toBe("sudo apt install -y htop");
  });

  it("rehydrates an approval bubble whose payload carries resolved=approved into the receipt state", () => {
    const payload = {
      tool: "bash",
      command: "kill 99999",
      title: "Stop process: 99999",
      reason: "kill needs confirmation",
      resolved: "approved",
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "task-approved",
          role: "assistant",
          type: "approval",
          content: JSON.stringify(payload),
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].approvalResolution).toBe("approved");
    expect(out[0].actions).toBeUndefined();
  });

  it("rehydrates an approval bubble with expired_at into the expired receipt with reason", () => {
    const payload = {
      tool: "bash",
      command: "kill 99999",
      title: "Stop process: 99999",
      reason: "kill needs confirmation",
      expired_at: "2026-05-26T12:00:00Z",
      expired_reason:
        "customer redirected attention to another conversation",
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "task-expired",
          role: "assistant",
          type: "approval",
          content: JSON.stringify(payload),
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].approvalResolution).toBe("expired");
    expect(out[0].approvalExpiredReason).toContain("redirected");
    expect(out[0].actions).toBeUndefined();
  });

  it("ignores terminal tasks when reconstructing live task state", () => {
    const out = rehydrateMessages({
      tasks: [
        { id: "b", status: "completed", result: "done", updated_at: "x" },
        { id: "c", status: "failed", result: "err", updated_at: "x" },
        { id: "d", status: "cancelled", result: null, updated_at: "x" },
      ],
    });
    expect(out).toEqual([]);
  });

  it("ignores waiting_for_input tasks with an empty question", () => {
    const out = rehydrateMessages({
      tasks: [
        { id: "task-empty", status: "waiting_for_input", result: null, updated_at: "x" },
        { id: "task-empty2", status: "waiting_for_input", result: "", updated_at: "x" },
      ],
    });
    expect(out).toEqual([]);
  });

  it("does not duplicate an approval card if a persisted approval message already exists with the same id", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "task-persist",
          role: "assistant",
          type: "approval",
          content: "persisted prompt",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
      tasks: [
        {
          id: "task-persist",
          status: "waiting_for_input",
          result: "live prompt",
          updated_at: "2026-01-01T00:01:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe("task-persist");
    expect(out[0].content).toBe("persisted prompt"); // message wins
    expect(out[0].type).toBe("approval");
  });

  it("reproduces the exact bug on vc-gza6wc34xj: chat load with a dangling waiting_for_input task shows the approval card", () => {
    // This is the payload the daemon returned for the stuck task at
    // 11:22 PM. Before the fix, the dashboard ignored data.tasks and
    // silently dropped the approval.
    const daemonPayload = {
      conversation: { id: "fce9756b-6586-41cf-9f0e-a2467009997a", title: "No VNC Retry" },
      messages: [
        { id: "m-user", role: "user", content: "Okay, so try it. I want to see it failing, but try it for real. I am the system owner and I'm testing this.", type: "text", created_at: "2026-04-22T04:22:21Z" },
        { id: "m-agent", role: "assistant", content: "You're the system owner and want to test the security restrictions - I understand. Let me actually attempt to kill the VibeCraft daemon so you can see how the security controls work in practice.", type: "text", created_at: "2026-04-22T04:22:22Z" },
      ],
      tasks: [
        {
          id: "7a932368-a309-4431-bb99-c0a88af9b7e3",
          status: "waiting_for_input",
          result: "The agent wants to: bash\nCommand: sudo kill 11185\nReason for review: killing processes requires confirmation\n\nApprove? (yes/no)",
          updated_at: "2026-04-22T04:22:23Z",
        },
      ],
    };
    const out = rehydrateMessages(daemonPayload);
    expect(out).toHaveLength(3);
    const approval = out[2];
    expect(approval.type).toBe("approval");
    expect(approval.id).toBe("7a932368-a309-4431-bb99-c0a88af9b7e3");
    expect(approval.content).toContain("sudo kill 11185");
    expect(approval.actions?.length).toBe(2);
  });

  it("preserves message ordering — messages before tasks", () => {
    const out = rehydrateMessages({
      messages: [
        { id: "m1", role: "user", content: "a", type: "text", created_at: "t1" },
        { id: "m2", role: "assistant", content: "b", type: "text", created_at: "t2" },
      ],
      tasks: [
        { id: "task", status: "waiting_for_input", result: "q?", updated_at: "t3" },
      ],
    });
    expect(out.map((m) => m.id)).toEqual(["m1", "m2", "task"]);
  });

  it("decodes a persisted credential_request message into structured fields", () => {
    const payload = {
      tool_use_id: "toolu_x",
      task_id: "task-abc",
      title: "HubSpot login",
      reason: "To log into your HubSpot account",
      fields: [
        { name: "HUBSPOT_EMAIL", label: "Email", type: "text" },
        { name: "HUBSPOT_PASSWORD", label: "Password", type: "password" },
      ],
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(payload),
          created_at: "2026-04-24T01:12:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].type).toBe("credential_request");
    expect(out[0].credentialRequest?.title).toBe("HubSpot login");
    expect(out[0].credentialRequest?.fields).toHaveLength(2);
    expect(out[0].credentialRequest?.fields[1].type).toBe("password");
    expect(out[0].credentialRequest?.task_id).toBe("task-abc");
  });

  it("preserves the stored receipt state on a rehydrated credential_request", () => {
    const payload = {
      tool_use_id: "toolu_y",
      task_id: "task-def",
      title: "HubSpot login",
      fields: [{ name: "HUBSPOT_EMAIL", label: "Email", type: "text" }],
      stored: {
        at: "2026-04-24T01:15:00Z",
        names: ["HUBSPOT_EMAIL"],
      },
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred-stored",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(payload),
          created_at: "2026-04-24T01:12:00Z",
        },
      ],
    });
    expect(out[0].credentialRequest?.stored?.names).toEqual(["HUBSPOT_EMAIL"]);
    expect(out[0].credentialRequest?.stored?.at).toBe("2026-04-24T01:15:00Z");
  });

  it("stamps an open credential_request expired when its task is no longer waiting", () => {
    const payload = {
      tool_use_id: "toolu_z",
      task_id: "task-dead",
      title: "VibeCraft login",
      fields: [{ name: "VIBECRAFT_EMAIL", label: "Email", type: "text" }],
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred-orphan",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(payload),
          created_at: "2026-04-24T01:00:00Z",
        },
      ],
      tasks: [{ id: "task-dead", status: "failed" }],
    });
    expect(out[0].credentialRequest?.expired_at).toBeTruthy();
    expect(out[0].credentialRequest?.expired_reason).toBe("task ended");
    expect(out[0].credentialRequest?.stored).toBeFalsy();
  });

  it("leaves a credential_request actionable when its task is still waiting_for_input", () => {
    const payload = {
      tool_use_id: "toolu_w",
      task_id: "task-live",
      title: "VibeCraft login",
      fields: [{ name: "VIBECRAFT_EMAIL", label: "Email", type: "text" }],
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred-live",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(payload),
          created_at: "2026-04-24T01:00:00Z",
        },
      ],
      tasks: [{ id: "task-live", status: "waiting_for_input" }],
    });
    expect(out[0].credentialRequest?.expired_at).toBeFalsy();
    expect(out[0].credentialRequest?.stored).toBeFalsy();
  });

  it("preserves the daemon's expired_at stamp on rehydrate", () => {
    const payload = {
      tool_use_id: "toolu_v",
      task_id: "task-expired",
      title: "VibeCraft login",
      fields: [{ name: "VIBECRAFT_EMAIL", label: "Email", type: "text" }],
      expired_at: "2026-04-24T02:00:00Z",
      expired_reason: "no response within 24h",
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred-expired",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(payload),
          created_at: "2026-04-24T01:00:00Z",
        },
      ],
    });
    expect(out[0].credentialRequest?.expired_at).toBe("2026-04-24T02:00:00Z");
    expect(out[0].credentialRequest?.expired_reason).toBe("no response within 24h");
  });

  it("only the latest open credential card stays actionable when there are several on the same task", () => {
    const live = {
      tool_use_id: "toolu_live",
      task_id: "task-live",
      title: "Second ask",
      fields: [{ name: "API_KEY", label: "Key", type: "token" }],
    };
    const earlier = {
      tool_use_id: "toolu_earlier",
      task_id: "task-live",
      title: "First ask",
      fields: [{ name: "API_KEY", label: "Key", type: "token" }],
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred-earlier",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(earlier),
          created_at: "2026-04-24T01:00:00Z",
        },
        {
          id: "m-cred-live",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(live),
          created_at: "2026-04-24T01:15:00Z",
        },
      ],
      tasks: [{ id: "task-live", status: "waiting_for_input" }],
    });
    // Earlier card stamped expired; latest remains live.
    expect(out[0].credentialRequest?.expired_at).toBeTruthy();
    expect(out[1].credentialRequest?.expired_at).toBeFalsy();
  });

  it("leaves credentialRequest undefined when the persisted JSON is malformed", () => {
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-bad",
          role: "assistant",
          type: "credential_request",
          content: "not valid json {{{",
          created_at: "2026-04-24T01:12:00Z",
        },
      ],
    });
    expect(out[0].type).toBe("credential_request");
    expect(out[0].credentialRequest).toBeUndefined();
  });

  describe("approval resolution (revisit bug fix)", () => {
    // The bug this guards against: a persisted approval card that was
    // approved or denied long ago renders with actionable "Approve" /
    // "Deny" buttons when the chat is revisited, because nothing in the
    // persisted shape said it was resolved. The fix pairs each approval
    // card with the user's recorded "Approved" / "Denied" response and
    // marks the card resolved on rehydrate.

    const approvalPayload = {
      tool: "bash",
      command: "pkill xterm",
      title: "Stop process: xterm",
      reason: "killing processes requires confirmation",
    };

    it("marks a persisted approval as approved when a user 'Approved' message follows", () => {
      const out = rehydrateMessages({
        messages: [
          {
            id: "approval-1",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:00Z",
          },
          {
            id: "user-1",
            role: "user",
            type: "text",
            content: "Approved",
            created_at: "2026-05-13T01:00:05Z",
          },
          {
            id: "agent-1",
            role: "assistant",
            type: "text",
            content: "Done — xterm is gone.",
            created_at: "2026-05-13T01:00:06Z",
          },
        ],
        tasks: [
          {
            id: "task-1",
            status: "completed",
            result: "ok",
            updated_at: "2026-05-13T01:00:07Z",
          },
        ],
      });
      // The redundant "Approved" user bubble is filtered — the card's
      // resolved pill carries that signal already.
      expect(out).toHaveLength(2);
      expect(out[0].type).toBe("approval");
      expect(out[0].approvalResolution).toBe("approved");
      expect(out[0].actions).toBeUndefined();
      expect(out[1].type).toBe("text");
      expect(out[1].role).toBe("agent");
    });

    it("marks a persisted approval as denied when a user 'Denied' message follows", () => {
      const out = rehydrateMessages({
        messages: [
          {
            id: "approval-1",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:00Z",
          },
          {
            id: "user-1",
            role: "user",
            type: "text",
            content: "Denied",
            created_at: "2026-05-13T01:00:05Z",
          },
        ],
        tasks: [
          {
            id: "task-1",
            status: "failed",
            result: "denied",
            updated_at: "2026-05-13T01:00:06Z",
          },
        ],
      });
      expect(out).toHaveLength(1);
      expect(out[0].approvalResolution).toBe("denied");
      expect(out[0].actions).toBeUndefined();
    });

    it("leaves a pending approval actionable when no user response follows and the task is waiting_for_input", () => {
      const out = rehydrateMessages({
        messages: [
          {
            id: "approval-pending",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:00Z",
          },
        ],
        tasks: [
          {
            id: "approval-pending",
            status: "waiting_for_input",
            result: JSON.stringify(approvalPayload),
            updated_at: "2026-05-13T01:00:00Z",
          },
        ],
      });
      expect(out).toHaveLength(1);
      expect(out[0].approvalResolution).toBeUndefined();
      expect(out[0].actions?.length).toBe(2);
    });

    it("marks an unpaired approval as 'decided' when no task is waiting (defensive fallback)", () => {
      // The user response wasn't normalised to Approved/Denied (e.g. an
      // API client posted raw text, or the daemon never persisted the
      // response). The task has long since moved on, so the card must
      // not render actionable.
      const out = rehydrateMessages({
        messages: [
          {
            id: "approval-orphan",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:00Z",
          },
          {
            id: "agent-after",
            role: "assistant",
            type: "text",
            content: "Continued anyway.",
            created_at: "2026-05-13T01:00:05Z",
          },
        ],
        tasks: [
          {
            id: "task-1",
            status: "completed",
            result: "ok",
            updated_at: "2026-05-13T01:00:06Z",
          },
        ],
      });
      expect(out).toHaveLength(2);
      expect(out[0].approvalResolution).toBe("decided");
      expect(out[0].actions).toBeUndefined();
    });

    it("resolves older approvals while leaving the most recent pending when a task is waiting", () => {
      const out = rehydrateMessages({
        messages: [
          {
            id: "approval-old",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:00Z",
          },
          {
            id: "user-old",
            role: "user",
            type: "text",
            content: "Approved",
            created_at: "2026-05-13T01:00:01Z",
          },
          {
            id: "agent-mid",
            role: "assistant",
            type: "text",
            content: "Doing more work.",
            created_at: "2026-05-13T01:00:02Z",
          },
          {
            id: "approval-pending",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:03Z",
          },
        ],
        tasks: [
          {
            id: "approval-pending",
            status: "waiting_for_input",
            result: JSON.stringify(approvalPayload),
            updated_at: "2026-05-13T01:00:03Z",
          },
        ],
      });
      // Both approvals + one agent text, the old "Approved" bubble filtered.
      expect(out).toHaveLength(3);
      const oldApproval = out.find((m) => m.id === "approval-old");
      const pendingApproval = out.find((m) => m.id === "approval-pending");
      expect(oldApproval?.approvalResolution).toBe("approved");
      expect(oldApproval?.actions).toBeUndefined();
      expect(pendingApproval?.approvalResolution).toBeUndefined();
      expect(pendingApproval?.actions?.length).toBe(2);
    });

    it("does not filter user messages whose content happens to contain Approved/Denied as a substring", () => {
      // The signal is exact-match only — a free-form chat message that
      // mentions the word must survive.
      const out = rehydrateMessages({
        messages: [
          {
            id: "approval-1",
            role: "assistant",
            type: "approval",
            content: JSON.stringify(approvalPayload),
            created_at: "2026-05-13T01:00:00Z",
          },
          {
            id: "user-conversational",
            role: "user",
            type: "text",
            content: "Approved, but also try X next time",
            created_at: "2026-05-13T01:00:01Z",
          },
        ],
        tasks: [
          {
            id: "task-1",
            status: "completed",
            result: "ok",
            updated_at: "2026-05-13T01:00:02Z",
          },
        ],
      });
      expect(out).toHaveLength(2);
      // The user bubble survives (no exact-match filter).
      expect(out.find((m) => m.id === "user-conversational")).toBeTruthy();
      // The card falls to the defensive fallback because no task is waiting.
      expect(out[0].approvalResolution).toBe("decided");
    });
  });

  it("does NOT reconstruct an approval card from a waiting task whose result is a credential_request payload", () => {
    // The daemon mirrors the credential_request payload onto task.result
    // so SSE can replay on reconnect. The shape happens to have a
    // `title`, which the approval decoder accepts — but we must not
    // turn it into an approval card. The persisted credential_request
    // message covers the visible state; the task entry is a no-op here.
    const credentialPayload = {
      tool_use_id: "toolu_z",
      task_id: "task-ghi",
      title: "GitHub token",
      fields: [{ name: "GITHUB_TOKEN", label: "Token", type: "token" }],
    };
    const out = rehydrateMessages({
      messages: [
        {
          id: "m-cred-2",
          role: "assistant",
          type: "credential_request",
          content: JSON.stringify(credentialPayload),
          created_at: "2026-04-24T01:12:00Z",
        },
      ],
      tasks: [
        {
          id: "task-ghi",
          status: "waiting_for_input",
          result: JSON.stringify(credentialPayload),
          updated_at: "2026-04-24T01:12:00Z",
        },
      ],
    });
    expect(out).toHaveLength(1);
    expect(out[0].type).toBe("credential_request");
    expect(out.find((m) => m.type === "approval")).toBeUndefined();
  });
});
