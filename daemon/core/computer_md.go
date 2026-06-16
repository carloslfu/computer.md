// SPDX-License-Identifier: Apache-2.0

// COMPUTER.md is the per-machine config file shared between the
// manager (the AI agent) and the customer. It captures durable
// preferences ("always prefer Codex"), machine facts the agent has
// learned ("the user's main project lives in ~/work/sofia"), and any
// other context that should persist across conversations.
//
// The file is *user-facing*: it lives at /home/vibecraft/COMPUTER.md
// so the user can `cat` or edit it directly. It is separate from the
// daemon's opaque memory store (daemon/memory) — that store is for the
// agent's internal scratchpad; COMPUTER.md is for things the user
// might want to see, tweak, or audit.
//
// Read/write semantics:
//
//   - The manager reads COMPUTER.md at the start of every agent-loop
//     iteration via the ContextBuilder. Whatever's in the file becomes
//     part of the system prompt for that turn.
//   - The manager writes to COMPUTER.md using its normal file-edit
//     tool (str_replace_based_edit_tool) — no special tool needed.
//   - The dashboard reads/writes via GET/PUT /api/computer-md.
//   - The file is auto-created with a sensible default on first read
//     so a fresh machine doesn't surprise the agent with an empty
//     context.
//
// We deliberately keep this tiny: a path constant, a default
// template, a read-or-create helper, and an atomic write helper. No
// schema, no parser, no migration story. The file is markdown and
// stays markdown.
package core

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// ComputerMDPath is the canonical location of the per-machine config
// file. Lives in the customer's home directory so it's visible in
// `ls ~` but doesn't shout for attention.
const ComputerMDPath = "/home/vibecraft/COMPUTER.md"

// ComputerMDDefault is written on first read if the file is missing.
// Keep this short and human-readable — the user might be reading it
// the first time they pull it up.
const ComputerMDDefault = `# COMPUTER.md

This file is shared between you (the customer) and the manager (the AI agent on this machine).

- The manager reads it at the start of every conversation; whatever is here is part of its context.
- The manager writes here when you express a durable preference ("always X", "from now on Y", "remember that Z about this machine").
- You can edit it directly. Ask the manager "show me COMPUTER.md" to see the current contents in chat.

Keep it short and human-readable. This is a note to yourself and the agent, not a database.

## Worker preference

Default: prefer Claude Code (` + "`claude`" + `) for development builds. If Claude Code is not installed or fails, fall back to Codex (` + "`codex`" + `). If neither is available, the manager builds inline as a last resort.

To override, replace the paragraph above with your preference. Examples:
- "Prefer Codex first, then Claude Code."
- "Always use Claude Code; never use Codex."
- "Use Codex for short tasks, Claude Code for anything multi-file."

## Preferences

(Empty — the manager will append entries here as you teach it things, or edit directly.)

## What the manager has learned about this machine

(Empty — the manager appends durable, machine-specific facts here over time.)
`

// ReadOrCreateComputerMD returns the current contents of
// /home/vibecraft/COMPUTER.md.
//
// If the file does not exist, it writes the default template and
// returns that. If any other I/O error occurs (permission denied,
// disk full, etc.), it logs a warning and returns an empty string —
// the agent loop must not fail because of this file.
//
// On every read (existing or newly created), the file's ownership and
// permissions are reset to vibecraft:vibecraft mode 0664. This is
// idempotent on a correctly-owned file and repairs files left behind
// by older daemon versions that wrote them as root. Without this, the
// agent's bash sandbox (which runs as the vibecraft user and sees
// host UIDs through a user namespace) cannot edit the file via
// str_replace_based_edit_tool.
func ReadOrCreateComputerMD() string {
	content, err := os.ReadFile(ComputerMDPath)
	if err == nil {
		// File exists — make sure ownership is sane for the agent's
		// bash sandbox before returning.
		fixComputerMDOwnership()
		return string(content)
	}
	if !os.IsNotExist(err) {
		// Permission denied / I/O error — degrade gracefully so the
		// agent loop keeps running. Log to stderr; we don't want a
		// missing/broken COMPUTER.md to break every conversation.
		fmt.Fprintf(os.Stderr, "warning: reading %s failed: %v\n", ComputerMDPath, err)
		return ""
	}

	// File missing — write the default and return it.
	if err := os.MkdirAll(filepath.Dir(ComputerMDPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: creating dir for %s failed: %v\n", ComputerMDPath, err)
		return ComputerMDDefault
	}
	if err := os.WriteFile(ComputerMDPath, []byte(ComputerMDDefault), 0o664); err != nil {
		fmt.Fprintf(os.Stderr, "warning: writing default %s failed: %v\n", ComputerMDPath, err)
	}
	fixComputerMDOwnership()
	return ComputerMDDefault
}

// WriteComputerMD replaces the file contents atomically (write to a
// temp file in the same directory, then rename). The atomic swap
// matters because the manager may be re-reading the file mid-stream;
// a partial write would leak garbage into the system prompt.
//
// After the rename, ownership is reset to vibecraft:vibecraft so the
// agent's bash sandbox can still edit the file via str_replace on
// subsequent turns.
func WriteComputerMD(content string) error {
	dir := filepath.Dir(ComputerMDPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := ComputerMDPath + ".tmp"
	// O_NOFOLLOW: a malicious symlink planted by the vibecraft user at
	// COMPUTER.md.tmp must not be followed, or root would overwrite an
	// arbitrary file. O_CREATE|O_TRUNC|O_WRONLY for a normal write.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o664)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, ComputerMDPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename to %s: %w", ComputerMDPath, err)
	}
	fixComputerMDOwnership()
	return nil
}

// fixComputerMDOwnership ensures /home/vibecraft/COMPUTER.md is owned
// by the vibecraft user with mode 0664. Idempotent: no-op when
// already correct. Silently no-ops on hosts without a vibecraft user
// (dev workstations, unit tests) or when the daemon isn't running as
// root.
//
// This exists because the daemon runs as root on the host while the
// agent's bash tool runs in a bwrap sandbox as the vibecraft user
// with a separate user namespace. A file created by root looks like
// nobody:nogroup from inside the sandbox, which makes it unwritable
// even though the sandbox user is "supposed to" own /home/vibecraft.
// Chown to vibecraft:vibecraft + group-writable mode solves both
// problems.
func fixComputerMDOwnership() {
	u, err := user.Lookup("vibecraft")
	if err != nil {
		// No vibecraft user — almost certainly a dev box running the
		// daemon as the developer. Leave the file alone.
		return
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return
	}
	// Lchown not Chown — never follow a symlink at this path.
	if err := os.Lchown(ComputerMDPath, uid, gid); err != nil {
		// EPERM is expected on dev machines where the daemon isn't
		// root. Anything else is worth a warning.
		if !os.IsPermission(err) {
			fmt.Fprintf(os.Stderr, "warning: chown %s failed: %v\n", ComputerMDPath, err)
		}
		return
	}
	// O_NOFOLLOW: never chmod through a symlink. If the vibecraft user
	// plants a symlink at COMPUTER.md pointing at a 0600 protected file
	// (e.g. /etc/vibecraft/*), a plain os.Chmod would follow it and
	// weaken the target's permissions. Open the path itself (failing if
	// it's a symlink) and fchmod the fd.
	f, err := os.OpenFile(ComputerMDPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if !os.IsPermission(err) {
			fmt.Fprintf(os.Stderr, "warning: open %s for chmod failed: %v\n", ComputerMDPath, err)
		}
		return
	}
	defer f.Close()
	if err := f.Chmod(0o664); err != nil {
		fmt.Fprintf(os.Stderr, "warning: chmod %s failed: %v\n", ComputerMDPath, err)
	}
}
