// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/carloslfu/computer.md/daemon/audit"
)

// Seams for tests. Default to the real `crontab` binary and the real
// marker location; tests override them to exercise the wipe-failure path
// without touching the host scheduler or /etc.
var (
	crontabMigrateMarker = "/etc/vibecraft/.crontab-migrated"

	// crontabList snapshots the host vibecraft crontab.
	crontabList = func() ([]byte, error) {
		return exec.Command("crontab", "-l", "-u", "vibecraft").Output()
	}

	// crontabWipe clears the host vibecraft crontab.
	crontabWipe = func() error {
		return exec.Command("crontab", "-r", "-u", "vibecraft").Run()
	}
)

// Paired delimiters bounding the migrated-host-crontab block inside the
// agent-shell crontab. They make the PROMOTE step IDEMPOTENT: each promotion
// strips any prior block (wherever it sits in the file) before re-appending a
// fresh one, so a retry rewrites the block in place instead of appending the
// host entries a second (and third, and Nth) time. Without this, a re-run of
// the promotion would multiply the schedule.
const (
	crontabMigrateBeginMarker = "# --- BEGIN migrated from the pre-sandboxing host crontab (managed by vibecraft; do not edit between markers) ---"
	crontabMigrateEndMarker   = "# --- END migrated from the pre-sandboxing host crontab ---"
)

// crontabMigrateStagingName is the durable side-file that holds the snapshot
// of the host crontab BETWEEN the host wipe and the moment the entries become
// live in supercronic. It is NOT read by supercronic (supercronic reads only
// <home>/crontab), so staging a snapshot here never schedules anything. Its
// sole job is to survive a crash in the wipe→promote window: if the daemon
// dies after the host wipe succeeded but before the entries were promoted into
// the live crontab, the next boot finds this file (host crontab is now empty)
// and finishes the promotion instead of silently losing the jobs.
const crontabMigrateStagingName = "crontab.migrating"

// stripMigrationBlock removes a previously-written migration block (the
// region between crontabMigrateBeginMarker and crontabMigrateEndMarker,
// inclusive) from an existing agent-shell crontab, returning the remaining
// operator/manager-authored content with trailing blank lines trimmed.
//
// It is tolerant of a malformed file: a BEGIN with no matching END strips
// from BEGIN to the end of the file (the block was always appended at the
// tail, so this is the only place a truncated block can be). A file with no
// BEGIN marker is returned unchanged. Only the FIRST block is stripped —
// there should only ever be one, and re-running converges to one.
func stripMigrationBlock(content string) string {
	lines := strings.Split(content, "\n")
	begin := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == crontabMigrateBeginMarker {
			begin = i
			break
		}
	}
	if begin == -1 {
		return strings.TrimRight(content, "\n")
	}
	end := len(lines) // default: strip to EOF (truncated block)
	for i := begin + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == crontabMigrateEndMarker {
			end = i + 1 // inclusive of the END marker line
			break
		}
	}
	kept := append([]string{}, lines[:begin]...)
	if end < len(lines) {
		kept = append(kept, lines[end:]...)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n")
}

// isNoCrontabError reports whether a non-zero `crontab -l` exit means the user
// simply has no crontab ("no crontab for <user>") rather than a real failure
// (missing binary, permission denied, transient error). Only the former is a
// safe-to-seal empty host; everything else must NOT seal the migration.
//
// `crontab -l` writes "no crontab for <user>" to STDERR. Our seam uses
// exec.Cmd.Output(), which on a non-zero exit returns an *exec.ExitError whose
// .Stderr holds that text. We also match the error string itself so the test
// seams (which return a plain errors.New("no crontab for vibecraft")) and any
// implementation that surfaces stderr through Error() are both recognized.
// Fail closed: an unrecognized error returns false (treated as a real error).
func isNoCrontabError(err error) bool {
	if err == nil {
		return false
	}
	const sentinel = "no crontab for"
	if strings.Contains(err.Error(), sentinel) {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), sentinel) {
		return true
	}
	return false
}

// migrateHostCrontab is the Phase 3 one-shot host-crontab migration. The
// pre-sandboxing host crontab is the agent's own loops (the system
// concept didn't formally exist before Phase 3, so entries aren't tagged
// by system). On first boot of a Phase-3 daemon we move every host entry
// into the AGENT-SHELL sandbox's persisted crontab (the catch-all —
// entries that don't belong to a specific system run as the manager's own
// scheduled tasks; the manager reclassifies into per-system crontabs over
// time).
//
// The ordering is what makes this safe. The entries must run EXACTLY ONCE
// TOTAL across the host cron and supercronic, so they must never be live in
// both at once:
//
//  1. snapshot `crontab -l -u vibecraft`
//  2. STAGE the snapshot to a side-file supercronic does not read (durable,
//     schedules nothing — jobs still live only in the host crontab)
//  3. WIPE the host crontab. If the wipe fails, the jobs are live ONLY in the
//     host (one place), nothing is marked, and the next boot retries cleanly.
//  4. only AFTER the host is clear, PROMOTE the staged entries into the live
//     agent-shell crontab supercronic reads (one place), remove the staging
//     file, and write the marker.
//
// A naive "copy into supercronic, then wipe the host" ordering re-introduces
// the double-run: when the wipe persistently fails, the jobs stay live in BOTH
// schedulers on every boot. Wiping first, promoting second avoids that.
//
// Crash-safe: if the daemon dies after the wipe but before the promotion, the
// next boot finds the staging file (the host crontab is already empty) and
// finishes the promotion instead of losing the jobs. The promotion is
// idempotent (strip-then-append inside a delimited block), so a re-run never
// multiplies the schedule.
//
// Idempotent via the marker so a reboot/restart never re-migrates (which would
// resurrect entries the manager has since moved). Best-effort: every failure
// is logged, the daemon continues — a crontab migration hiccup must never block
// startup. On the macOS dev host there is no `vibecraft` crontab; the marker is
// still written so it's a clean no-op.
func migrateHostCrontab(homeHost string, auditLog *audit.Logger) {
	marker := crontabMigrateMarker
	if _, err := os.Stat(marker); err == nil {
		return // already done
	}

	crontabFile := filepath.Join(homeHost, "crontab")
	stagingFile := filepath.Join(homeHost, crontabMigrateStagingName)

	// markMigrated persists the one-shot marker so reboots never re-migrate.
	markMigrated := func() {
		if err := os.WriteFile(marker, []byte("done\n"), 0600); err != nil {
			log.Printf("crontab-migrate: could not write marker: %v", err)
		}
	}

	// promote makes the staged host entries LIVE in the agent-shell crontab
	// supercronic reads, then removes the staging file and marks migrated. It
	// is only ever called AFTER the host crontab is confirmed clear, so when it
	// runs the entries are nowhere else — they go live in exactly one place.
	//
	// IDEMPOTENT: strip any prior migration block from the live crontab first,
	// then re-append a single fresh one inside the BEGIN/END markers, preserving
	// any crontab the manager authored OUTSIDE that block. A re-run (e.g. a
	// crash between writing the live file and removing the staging file) rewrites
	// the same single block rather than appending the entries again — so the
	// schedule never multiplies.
	promote := func(body string) bool {
		prefix := ""
		if existing, rerr := os.ReadFile(crontabFile); rerr == nil && len(existing) > 0 {
			if kept := stripMigrationBlock(string(existing)); kept != "" {
				prefix = kept + "\n"
			}
		}
		merged := prefix + crontabMigrateBeginMarker + "\n" + body + "\n" + crontabMigrateEndMarker + "\n"
		if err := os.WriteFile(crontabFile, []byte(merged), 0600); err != nil {
			// Could not make the entries live. The host crontab is already
			// wiped, so the jobs are currently live NOWHERE — but the staging
			// file still holds the snapshot, and the marker is NOT set, so the
			// next boot recovers and finishes the promotion. Do not lose data.
			log.Printf("crontab-migrate: write agent-shell crontab: %v (staged snapshot kept for retry)", err)
			return false
		}
		// Owned by the sandbox identity. chownToAgent is the same helper
		// LoadConfig uses for the inbox; a no-op when the vibecraft user
		// doesn't exist (dev).
		chownToAgent(crontabFile)
		// Remove the staging file last: if we crash before this, the next boot
		// re-promotes idempotently (strip-then-append) — harmless.
		if err := os.Remove(stagingFile); err != nil && !os.IsNotExist(err) {
			log.Printf("crontab-migrate: could not remove staging file %s: %v", stagingFile, err)
		}
		markMigrated()
		log.Printf("crontab-migrate: moved host crontab into the agent-shell sandbox; host crontab wiped")
		if auditLog != nil {
			auditLog.Log(audit.Entry{
				Action: "host_crontab_migrated", Category: "security",
				Details: "host vibecraft crontab moved into agent-shell sandbox spool", RiskLevel: "low",
			})
		}
		return true
	}

	// Read the durable staged snapshot from a prior interrupted boot, if any.
	// Whether we act on it depends on the LIVE host-crontab state, decided
	// below — the staging file alone cannot tell a "wipe succeeded, crashed
	// before promote" from a "wipe failed, staged for retry."
	stagedBody := ""
	if staged, rerr := os.ReadFile(stagingFile); rerr == nil {
		stagedBody = strings.TrimSpace(string(staged))
		if stagedBody == "" {
			_ = os.Remove(stagingFile) // empty staging file is meaningless
		}
	}

	out, err := crontabList()
	hostEmpty := false
	if err != nil {
		// `crontab -l` exits non-zero in two very different cases that we MUST
		// NOT conflate:
		//   1. There genuinely is no crontab for the user — the message is
		//      "no crontab for <user>". The host is empty; marking is safe.
		//   2. A real failure (crontab binary missing, permission denied, a
		//      transient error). The host crontab may still hold live jobs we
		//      have never migrated. Sealing here (markMigrated) would lock those
		//      jobs into the host forever and never migrate them. FAIL CLOSED:
		//      treat anything we cannot positively identify as "no crontab" as a
		//      real error, log it, and return so the next boot retries cleanly.
		if isNoCrontabError(err) {
			hostEmpty = true
		} else {
			log.Printf("crontab-migrate: could not read host crontab (NOT sealing migration; will retry on next boot): %v", err)
			return
		}
	} else if strings.TrimSpace(string(out)) == "" {
		hostEmpty = true
	}

	if hostEmpty {
		// The host crontab is clear. If a prior boot staged a snapshot but died
		// before promoting it (the wipe→promote crash window), finish the
		// promotion now — otherwise we would silently LOSE those jobs. With no
		// staged snapshot there is genuinely nothing to migrate; mark and stop.
		if stagedBody != "" {
			log.Printf("crontab-migrate: host crontab clear with a staged snapshot present — recovering interrupted migration")
			promote(stagedBody)
			return
		}
		log.Printf("crontab-migrate: no host crontab to migrate")
		markMigrated()
		return
	}

	// Host crontab is non-empty: the jobs are live there and ONLY there
	// (we have not promoted anything into supercronic). A staged snapshot at
	// this point is a leftover from a previous wipe-FAILURE, not a post-wipe
	// crash, so we must NOT promote from it — that would make the jobs live in
	// both places. Re-run the full stage→wipe→promote sequence on the current
	// host contents.
	body := strings.TrimSpace(string(out))

	// Stage the snapshot to a durable side-file that supercronic does NOT read.
	// This persists the host entries across the wipe so a crash between the wipe
	// and the promotion can recover. It does NOT schedule anything — the jobs
	// remain live ONLY in the host crontab at this point.
	if err := os.WriteFile(stagingFile, []byte(body+"\n"), 0600); err != nil {
		// Could not stage. The host crontab is still intact + untouched, so the
		// jobs run in exactly one place. Leave the marker absent so the next
		// boot retries cleanly.
		log.Printf("crontab-migrate: stage host crontab: %v", err)
		return
	}
	chownToAgent(stagingFile)

	// Wipe the host crontab BEFORE promoting the entries into supercronic. This
	// is the invariant that fixes the HIGH bug: the entries are made live in
	// supercronic only once the host copy is gone, so they are never live in
	// BOTH schedulers at once.
	//
	//   - wipe FAILS  -> the jobs are live ONLY in the host crontab (one place).
	//                    The staged snapshot is kept, the marker is NOT written,
	//                    and the next boot retries cleanly. No double-run.
	//   - wipe OK     -> the jobs are live NOWHERE for an instant; promote then
	//                    makes them live ONLY in supercronic (one place).
	if err := crontabWipe(); err != nil {
		log.Printf("crontab-migrate: WARNING host crontab NOT wiped (jobs remain live ONLY in the host crontab; staged snapshot kept; will retry on next boot; manual `crontab -r -u vibecraft` advised): %v", err)
		if auditLog != nil {
			auditLog.Log(audit.Entry{
				Action: "host_crontab_migrate_partial", Category: "security",
				Details: "host wipe failed; jobs still run only in host cron, snapshot staged, migration NOT marked done, will retry", RiskLevel: "medium",
			})
		}
		return
	}

	promote(body)
}
