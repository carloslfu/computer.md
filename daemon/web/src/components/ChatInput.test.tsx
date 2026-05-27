// SPDX-License-Identifier: Apache-2.0

import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ChatInput } from "./ChatInput";

function renderInput(isWorking: boolean) {
  render(
    <ChatInput
      onSend={vi.fn()}
      onStop={vi.fn()}
      isWorking={isWorking}
      stopping={false}
      conversationId="conversation-a"
      ensureConversationId={() => "conversation-a"}
    />,
  );
}

describe("ChatInput", () => {
  it("shows normal send copy when another conversation is working", () => {
    renderInput(false);

    fireEvent.change(screen.getByRole("textbox", { name: "Message" }), {
      target: { value: "say hello" },
    });

    expect(screen.queryByText("Sending will interrupt the current task")).toBeNull();
    expect(screen.getByRole("button", { name: "Send message" })).toBeTruthy();
  });

  it("shows interrupt copy for typed input in the running conversation", () => {
    renderInput(true);

    fireEvent.change(screen.getByRole("textbox", { name: "Message" }), {
      target: { value: "actually stop" },
    });

    expect(screen.getByText("Sending will interrupt the current task")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Interrupt and send" })).toBeTruthy();
  });
});
