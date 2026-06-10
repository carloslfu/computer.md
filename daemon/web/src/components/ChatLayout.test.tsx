// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";
import {
  isForeignConversationEvent,
  nextConversationAfterLoad,
} from "./ChatLayout";

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
