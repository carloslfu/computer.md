// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";
import { nextConversationAfterLoad } from "./ChatLayout";

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
