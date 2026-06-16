// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";
import {
  applyApprovalDecision,
  isForeignConversationEvent,
  nextConversationAfterLoad,
  revertApprovalDecision,
} from "./ChatLayout";
import type { ChatMessage } from "../lib/rehydrate";

function approvalCard(id: string): ChatMessage {
  return {
    id,
    role: "agent",
    type: "approval",
    content: "Run rm -rf /tmp/cache?",
    approvalTitle: "Delete the cache directory",
    actions: [
      { label: "Approve", action: "approve" },
      { label: "Deny", action: "cancel" },
    ],
    timestamp: "2026-06-14T00:00:00.000Z",
  };
}

describe("nextConversationAfterLoad", () => {
  it("keeps a draft new conversation blank while another task refreshes the list", () => {
    expect(
      nextConversationAfterLoad(
        null,
        [{ id: "running-conversation" }],
        true,
      ),
    ).toBeNull();
  });

  it("auto-selects the first loaded conversation during normal initial load", () => {
    expect(
      nextConversationAfterLoad(
        null,
        [{ id: "latest-conversation" }],
        false,
      ),
    ).toBe("latest-conversation");
  });

  it("preserves the current conversation when one is already selected", () => {
    expect(
      nextConversationAfterLoad(
        "current-conversation",
        [{ id: "latest-conversation" }],
        true,
      ),
    ).toBe("current-conversation");
  });
});

describe("isForeignConversationEvent", () => {
  it("drops an event from a different conversation (no cross-conversation bleed)", () => {
    expect(isForeignConversationEvent("conv-B", "conv-A")).toBe(true);
  });

  it("keeps an event for the active conversation", () => {
    expect(isForeignConversationEvent("conv-A", "conv-A")).toBe(false);
  });

  it("does not filter when the event carries no conversation_id (older daemon)", () => {
    expect(isForeignConversationEvent(undefined, "conv-A")).toBe(false);
    expect(isForeignConversationEvent("", "conv-A")).toBe(false);
  });

  it("does not filter when no conversation is active yet", () => {
    expect(isForeignConversationEvent("conv-B", null)).toBe(false);
  });
});

describe("approval decision optimistic flip + revert (web-ui-2)", () => {
  it("optimistically stamps the decision and drops the action buttons", () => {
    const before = [approvalCard("task-1")];
    const after = applyApprovalDecision(before, "task-1", "approved");
    expect(after[0].approvalResolution).toBe("approved");
    expect(after[0].actions).toBeUndefined();
  });

  it("reverts the optimistic flip when the server rejects (re-arms buttons)", () => {
    // Simulate: optimistic flip, then a 403 from /respond → revert.
    const flipped = applyApprovalDecision(
      [approvalCard("task-1")],
      "task-1",
      "denied",
    );
    expect(flipped[0].approvalResolution).toBe("denied");

    const reverted = revertApprovalDecision(flipped, "task-1", "denied");
    expect(reverted[0].approvalResolution).toBeUndefined();
    // The card must be actionable again — the user never actually denied.
    expect(reverted[0].actions).toEqual([
      { label: "Approve", action: "approve" },
      { label: "Deny", action: "cancel" },
    ]);
  });

  it("does NOT clobber a terminal state the daemon pushed via SSE meanwhile", () => {
    // Optimistic "approved", but an SSE task:approval_expired flipped the
    // card to "expired" before the failed respond response landed. The
    // revert must leave the authoritative terminal state untouched.
    const flipped = applyApprovalDecision(
      [approvalCard("task-1")],
      "task-1",
      "approved",
    );
    const expiredMidflight: ChatMessage[] = flipped.map((m) => ({
      ...m,
      approvalResolution: "expired",
      approvalExpiredReason: "task ended",
      actions: undefined,
    }));

    const reverted = revertApprovalDecision(
      expiredMidflight,
      "task-1",
      "approved",
    );
    expect(reverted[0].approvalResolution).toBe("expired");
    expect(reverted[0].actions).toBeUndefined();
  });

  it("does not touch a different approval card", () => {
    const flipped = applyApprovalDecision(
      [approvalCard("task-1"), approvalCard("task-2")],
      "task-1",
      "approved",
    );
    const reverted = revertApprovalDecision(flipped, "task-1", "approved");
    // task-2 was never flipped; it stays fully actionable and untouched.
    expect(reverted[1].approvalResolution).toBeUndefined();
    expect(reverted[1].actions).toHaveLength(2);
  });
});
