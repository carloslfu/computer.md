<!--
  This file is the reference daemon's lean default — the file every
  new machine gets on first read. Authored in
  daemon/core/computer_md.go as `ComputerMDDefault`; mirrored here
  verbatim. If they drift, the daemon is the source of truth — open
  a PR to update this file.
-->
# COMPUTER.md

This file is shared between you (the customer) and the manager (the AI agent on this machine).

- The manager reads it at the start of every conversation; whatever is here is part of its context.
- The manager writes here when you express a durable preference ("always X", "from now on Y", "remember that Z about this machine").
- You can edit it directly. Ask the manager "show me COMPUTER.md" to see the current contents in chat.

Keep it short and human-readable. This is a note to yourself and the agent, not a database.

## Worker preference

Default: prefer Claude Code (`claude`) for development builds. If Claude Code is not installed or fails, fall back to Codex (`codex`). If neither is available, the manager builds inline as a last resort.

To override, replace the paragraph above with your preference. Examples:
- "Prefer Codex first, then Claude Code."
- "Always use Claude Code; never use Codex."
- "Use Codex for short tasks, Claude Code for anything multi-file."

## Preferences

(Empty — the manager will append entries here as you teach it things, or edit directly.)

## What the manager has learned about this machine

(Empty — the manager appends durable, machine-specific facts here over time.)
