// SPDX-License-Identifier: Apache-2.0

import js from "@eslint/js";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";

// D.1.6: every component fetches through apiFetch(); nothing reaches
// the raw fetch(). This rule makes that a lint failure instead of a
// code-review hope. The wrapper itself (api/fetch.ts) is exempt. A
// handful of call sites genuinely cannot route through apiFetch and
// carry an inline eslint-disable with a justification:
//
//   - auth/refresh.ts        the refresh primitive itself — apiFetch
//                            depends on it, so it can't depend back
//   - components/ChatLayout  the SSE stream — a streaming Response;
//                            apiFetch's 401-retry would consume the
//                            body and break the reader
//   - components/Notification cross-origin to www.vibecraft.so; apiFetch
//                            is same-origin-daemon only
//   - lib/attachments.ts     blob fetch for inbox images; the JSON
//                            wrapper is the wrong shape
//
// Every one of those is a conscious, reviewed exception. New raw
// fetch() anywhere else fails CI.

const banRawFetch = {
  rules: {
    "no-restricted-syntax": [
      "error",
      {
        selector:
          "CallExpression[callee.name='fetch'], CallExpression[callee.property.name='fetch']",
        message:
          "Use apiFetch() from src/api/fetch.ts. Raw fetch() bypasses credentials, the JSON CSRF header, and silent-refresh. If this call genuinely cannot use the wrapper (SSE stream, cross-origin, blob, the refresh primitive), add an eslint-disable-next-line with a one-line reason.",
      },
    ],
  },
};

export default tseslint.config(
  { ignores: ["dist", "node_modules", "*.config.js", "*.config.ts"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["src/**/*.{ts,tsx}"],
    plugins: { "react-hooks": reactHooks },
    rules: {
      ...banRawFetch.rules,
      ...reactHooks.configs.recommended.rules,
      // The SPA leans on `any` at a few daemon-payload boundaries that
      // are validated at runtime; don't let that block the fetch rule.
      "@typescript-eslint/no-explicit-any": "off",
      "@typescript-eslint/no-unused-vars": [
        "warn",
        { argsIgnorePattern: "^_" },
      ],
    },
  },
  {
    // The wrapper is the one place raw fetch() is correct.
    files: ["src/api/fetch.ts"],
    rules: { "no-restricted-syntax": "off" },
  },
  {
    // Tests stub global fetch; the ban is irrelevant there.
    files: ["src/**/*.test.{ts,tsx}"],
    rules: { "no-restricted-syntax": "off" },
  },
);
