// SPDX-License-Identifier: Apache-2.0

import { chipLabel } from "./attachments";

describe("chipLabel", () => {
  it("returns the name unchanged when within the limit", () => {
    expect(chipLabel("report.pdf", 28)).toBe("report.pdf");
  });

  it("preserves a short extension when truncating a long stem", () => {
    const out = chipLabel("a_very_long_attachment_filename.pdf", 28);
    expect(out.length).toBeLessThanOrEqual(28);
    expect(out.endsWith(".pdf")).toBe(true);
    expect(out).toContain("…");
  });

  it("head-truncates when there is no usable extension", () => {
    const out = chipLabel("x".repeat(40), 28);
    expect(out.length).toBe(28);
    expect(out.endsWith("…")).toBe(true);
  });

  it("never exceeds maxLength when the final segment is longer than the limit", () => {
    // A long trailing segment after the last dot used to force a negative
    // stem length, which made name.slice(0, negative) return almost the
    // whole name and produced a label far longer than maxLength.
    const name = "x".repeat(22) + "." + "y".repeat(30);
    const out = chipLabel(name, 28);
    expect(out.length).toBeLessThanOrEqual(28);
    expect(out.endsWith("…")).toBe(true);
  });
});
