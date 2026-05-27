// SPDX-License-Identifier: Apache-2.0

// Agent-markdown sanitization used before feeding assistant text into
// Streamdown. Strips ghost fenced code blocks (empty bodies) and collapses
// runaway blank lines. Kept free of React / framework imports so it can
// be unit-tested and reused from non-browser contexts.

// Matches a fenced code block with an empty body. Allowed:
//   ```\n```
//   ```bash\n```
//   ```text\n   \n```
// Rejected (has real content): ```bash\ngrep foo\n```
const EMPTY_FENCE_RE = /^```[^\n`]*\n[\t ]*\n?```[\t ]*$/gm;

export function sanitizeAgentMarkdown(md: string): string {
  return md
    .replace(EMPTY_FENCE_RE, "")
    .replace(/\n{3,}/g, "\n\n")
    .replace(/^\s+|\s+$/g, "");
}
