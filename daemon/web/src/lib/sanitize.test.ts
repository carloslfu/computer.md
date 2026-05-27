// SPDX-License-Identifier: Apache-2.0

import { sanitizeAgentMarkdown } from "./sanitize";

describe("sanitizeAgentMarkdown", () => {
  it("strips an empty plain fenced block", () => {
    expect(sanitizeAgentMarkdown("```\n```")).toBe("");
  });

  it("strips an empty language-tagged fenced block", () => {
    expect(sanitizeAgentMarkdown("```bash\n```")).toBe("");
  });

  it("strips an empty fenced block with a blank body line", () => {
    expect(sanitizeAgentMarkdown("```\n\n```")).toBe("");
  });

  it("leaves a fenced block with real content alone", () => {
    const input = "```bash\ngrep foo bar\n```";
    expect(sanitizeAgentMarkdown(input)).toBe(input);
  });

  it("leaves inline backticks alone", () => {
    expect(sanitizeAgentMarkdown("Use `grep` to search.")).toBe(
      "Use `grep` to search.",
    );
  });

  it("strips a ghost fence between two blocks of content", () => {
    const input = "The line is:\n\n```\n```\n\n```\n[LOG]\n```";
    expect(sanitizeAgentMarkdown(input)).toBe(
      "The line is:\n\n```\n[LOG]\n```",
    );
  });

  it("strips multiple ghost fences in a row", () => {
    expect(sanitizeAgentMarkdown("```\n```\n```\n```\nafter")).toBe("after");
  });

  it("preserves indented content inside a block", () => {
    const input = "```\n  indented code\n```";
    expect(sanitizeAgentMarkdown(input)).toBe(input);
  });

  it("trims leading/trailing whitespace", () => {
    expect(sanitizeAgentMarkdown("\n\nhi\n\n")).toBe("hi");
  });

  it("collapses runs of blank lines to a single blank", () => {
    expect(sanitizeAgentMarkdown("a\n\n\n\nb")).toBe("a\n\nb");
  });
});
