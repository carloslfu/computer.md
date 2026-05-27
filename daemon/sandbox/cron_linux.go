// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

// cron_linux.go is the Phase 3 per-sandbox scheduler. Each PERSISTENT
// sandbox that hosts scheduled work (the agent-shell catch-all and every
// system sandbox) runs its own scheduler INSIDE its namespaces, reading
// a per-sandbox crontab FILE that lives in the sandbox's identity-mapped
// HOME (so it persists across daemon restarts / sandbox recreation with
// no extra mount, and the host has no vibecraft crontab — CLAUDE.md "no
// cron primitive": a standard cron, just bounded to each sandbox).
//
// The scheduler is `supercronic`, NOT Debian/Vixie `cron`. This is a
// security decision, recorded in spawn.go and daemon/SECURITY.md and the
// plan: every sandbox is spawned with `--unshare-user` (bubblewrap
// writes setgroups=deny for the new user namespace, unconditionally,
// even when bwrap runs as root). Vixie cron's per-job child does
// setgid()+initgroups()+setuid() before exec; setgroups() returns EPERM
// under setgroups=deny, so the job dies BEFORE exec — proven on a real
// kernel. The ONLY way to make Vixie cron fire is to drop
// `--unshare-user`, which would make a compromised sandbox real host
// root behind a hand-maintained capability blacklist — gutting the
// cornerstone of the isolation model. Rejected. supercronic is the
// standard container-grade cron: a single static binary that runs jobs
// as the current identity via /bin/sh with NO privilege-drop / no
// setgroups, so it works under full user-ns isolation. Same precedent
// as choosing `tini` for PID-1 (D5).

const supercronicBin = "/usr/local/bin/supercronic"

// CrontabPath is the per-sandbox crontab file: a plain file in the
// identity-mapped HOME (already bound at the same path), so no spool
// dir, no /var/spool bind, no setgid `crontab` helper.
func CrontabPath(homeDir string) string {
	return filepath.Join(homeDir, "crontab")
}

// ensureCrontab makes sure the crontab file exists (supercronic errors
// on a missing file). Seeded with a header so it is a valid, non-empty
// crontab the manager edits like any other file. Chowns to the sandbox
// identity so the inside-namespace view is owner-writable; without
// this the daemon (running as root) leaves the file root-owned, which
// inside the sandbox's user namespace appears as nobody:nogroup and
// the manager cannot edit it — observed live on a BYOM box where the
// manager's "remove this scheduled system" attempt couldn't touch
// ~/crontab to strip the entry.
func ensureCrontab(homeDir string) error {
	p := CrontabPath(homeDir)
	if _, err := os.Stat(p); err == nil {
		EnsureCrontabOwnership(homeDir) // repair drift on existing files
		return nil
	}
	if err := os.WriteFile(p,
		[]byte("# VibeCraft per-sandbox crontab. Standard 5-field lines:\n"+
			"#   * * * * * /path/to/command\n"+
			"# Edited by the manager; runs inside THIS sandbox only.\n"), 0600); err != nil {
		return err
	}
	chownAgent(p)
	return nil
}

// EnsureCrontabOwnership re-chowns the crontab file at homeDir to the
// sandbox identity if it exists. Exported so the daemon can run a
// defensive pass at every startup — fleet machines created before the
// ensureCrontab chown fix have root-owned crontabs that the manager
// cannot edit from inside the sandbox; one daemon restart after
// upgrading repairs ownership in place. Idempotent; no-op when the
// file already has the correct ownership, when the agent user doesn't
// exist (dev), or when the daemon lacks privilege.
func EnsureCrontabOwnership(homeDir string) {
	chownAgent(CrontabPath(homeDir))
}

// chownAgent looks up the "vibecraft" user and chowns the file to it.
// No-op in dev (user absent) or when the daemon isn't root. Mirrors
// the chownToAgent helper in the main package (different package, so
// duplicated rather than imported — sandbox/ has no main dependency).
func chownAgent(path string) {
	u, err := user.Lookup("vibecraft")
	if err != nil {
		return
	}
	uid, uerr := strconv.Atoi(u.Uid)
	gid, gerr := strconv.Atoi(u.Gid)
	if uerr != nil || gerr != nil {
		return
	}
	if err := os.Chown(path, uid, gid); err != nil && !os.IsPermission(err) {
		log.Printf("cron: chown %s: %v", path, err)
	}
}

func crontabMtime(p string) time.Time {
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// superviseCron runs supercronic against the sandbox's crontab file for
// the sandbox's lifetime. It restarts supercronic (bounded backoff) if
// it exits while ctx is live, and — since supercronic reads the crontab
// once at startup — also restarts it when the crontab file changes, so
// the manager's edits take effect without a daemon bounce. Stops
// cleanly when ctx is cancelled (sandbox Destroy).
func superviseCron(ctx context.Context, sb *Sandbox, homeDir, label string) {
	crontab := CrontabPath(homeDir)
	for {
		if ctx.Err() != nil {
			return
		}
		startMtime := crontabMtime(crontab)
		runCtx, cancel := context.WithCancel(ctx)
		// Reload-on-change: poll the crontab mtime; cancel this run when
		// it changes so the loop relaunches supercronic with the new
		// schedule. Cheap (one stat / 15s) and reload is the only path
		// — supercronic does not self-watch.
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					if crontabMtime(crontab) != startMtime {
						cancel()
						return
					}
				}
			}
		}()
		// Absolute path: bwrap runs --clearenv so there is no PATH.
		// -passthrough-logs keeps job stdout/stderr; -quiet drops
		// supercronic's own banner. supercronic runs each command via
		// /bin/sh as THIS (sandbox) identity — no setgroups/setuid.
		_, err := sb.Run(runCtx, []string{supercronicBin,
			"-passthrough-logs", "-quiet", crontab}, nil)
		cancel()
		if ctx.Err() != nil {
			return
		}
		// A crontab-change cancel is normal (relaunch immediately);
		// a real exit is not (back off so a broken crontab doesn't spin).
		if crontabMtime(crontab) != startMtime {
			continue
		}
		log.Printf("cron(%s): supercronic exited (%v) — restarting in 3s", label, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// discoveryWindow is how long a discovery system runs permissive+logged
// before a manifest is proposed. Plan default 7d; env-overridable so the
// real-kernel verification can exercise it in seconds.
func discoveryWindow() time.Duration {
	if v := os.Getenv("VIBECRAFT_DISCOVERY_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 7 * 24 * time.Hour
}

// watchDiscovery waits one discovery window then, if the hook is set,
// hands it the observed out-of-policy FQDNs for this system so the
// customer gets a concrete "lock egress to these" proposal.
func watchDiscovery(ctx context.Context, sb *Sandbox, system string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(discoveryWindow()):
	}
	if SystemProposalHook != nil {
		SystemProposalHook(system, sb.ObservedFQDNs())
	}
}
