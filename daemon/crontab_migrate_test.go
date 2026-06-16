// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCrontabSeams swaps the package-level test seams (marker path + the
// crontab list/wipe shims) for the duration of a test and restores them
// after, regardless of how the test exits.
func withCrontabSeams(t *testing.T, markerPath string, list func() ([]byte, error), wipe func() error) {
	t.Helper()
	origMarker, origList, origWipe := crontabMigrateMarker, crontabList, crontabWipe
	crontabMigrateMarker = markerPath
	crontabList = list
	crontabWipe = wipe
	t.Cleanup(func() {
		crontabMigrateMarker = origMarker
		crontabList = origList
		crontabWipe = origWipe
	})
}

func markerExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("unexpected stat error on marker %s: %v", path, err)
	return false
}

// TestMigrateHostCrontab_WipeFailureDoesNotMark is the regression guard for
// daemon-services-2: when the host wipe FAILS, the migration marker must NOT be
// written. Writing it on that path would seal in the host copy as live forever.
// Leaving the marker absent lets the next boot retry cleanly.
//
// With the exactly-once-total fix the entries on the wipe-failure path are
// STAGED (a side-file supercronic does not read), NOT made live in the
// agent-shell crontab — so the jobs run ONLY in the still-live host crontab,
// never in both at once.
func TestMigrateHostCrontab_WipeFailureDoesNotMark(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	const entries = "*/5 * * * * /home/vibecraft/loop.sh\n"
	withCrontabSeams(t, marker,
		func() ([]byte, error) { return []byte(entries), nil },
		func() error { return errors.New("crontab: permission denied") },
	)

	migrateHostCrontab(home, nil)

	if markerExists(t, marker) {
		t.Fatalf("marker was written on the wipe-failure path; this re-introduces double-execution (daemon-services-2)")
	}

	// The entries must NOT be live in the agent-shell crontab supercronic reads
	// while the host copy is still live — that would be the double-run.
	if supercronicLive(t, home, "/home/vibecraft/loop.sh") {
		t.Fatalf("entries went live in supercronic while the host wipe failed (host copy still live) — double-run")
	}

	// They must be staged durably so the next boot has the data to retry.
	staged, err := os.ReadFile(filepath.Join(home, crontabMigrateStagingName))
	if err != nil {
		t.Fatalf("expected host crontab to be staged for retry: %v", err)
	}
	if !strings.Contains(string(staged), "/home/vibecraft/loop.sh") {
		t.Fatalf("staged entries missing: %q", staged)
	}
}

// countSubstr counts non-overlapping occurrences of sub in s.
func countSubstr(s, sub string) int {
	return strings.Count(s, sub)
}

// hostCron is a tiny in-memory model of the host vibecraft crontab so a test
// can observe whether a job is simultaneously live in BOTH the host scheduler
// and supercronic. The real seams talk to /etc; this lets a test wire
// crontabList/crontabWipe to mutable state and check the exactly-once-TOTAL
// invariant the migration must hold.
type hostCron struct {
	body     string // current host crontab contents ("" == no/empty host crontab)
	wipeFail bool   // when true, crontabWipe returns an error and does NOT clear body
	wipes    int    // number of wipe attempts
}

func (h *hostCron) list() ([]byte, error) {
	if strings.TrimSpace(h.body) == "" {
		return nil, errors.New("no crontab for vibecraft")
	}
	return []byte(h.body), nil
}

func (h *hostCron) wipe() error {
	h.wipes++
	if h.wipeFail {
		return errors.New("crontab: permission denied")
	}
	h.body = ""
	return nil
}

// supercronicLive reports whether the named job is currently live in the
// agent-shell crontab supercronic reads (i.e. present in the live file, not a
// staging side-file).
func supercronicLive(t *testing.T, home, job string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "crontab"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), job)
}

// TestMigrateHostCrontab_NeverLiveInBothSchedulers is the regression guard for
// the HIGH bug the prior fix did NOT actually fix: when the host wipe
// persistently fails, the entries must never be live in BOTH the host cron and
// supercronic at the same time. The strip-then-append idempotency capped the
// schedule at 2x (instead of unbounded N-fold), but a permanent 2x is still a
// double-run on every boot the wipe fails — the goal is exactly-once TOTAL.
//
// The fix is to never promote the entries into the live supercronic crontab
// until the host crontab is confirmed clear. So across a persistent-wipe-
// failure loop the job stays live ONLY in the host (one place); once the wipe
// finally succeeds it flips to ONLY supercronic (one place). It is never live
// in both.
func TestMigrateHostCrontab_NeverLiveInBothSchedulers(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	const job = "/home/vibecraft/loop.sh"
	h := &hostCron{body: "*/5 * * * * " + job + "\n", wipeFail: true}
	withCrontabSeams(t, marker, h.list, h.wipe)

	// Several boots, all with a failing wipe.
	for i := 0; i < 3; i++ {
		migrateHostCrontab(home, nil)

		hostLive := strings.Contains(h.body, job)
		scLive := supercronicLive(t, home, job)
		if hostLive && scLive {
			t.Fatalf("boot %d: job live in BOTH host cron AND supercronic — permanent double-run (the HIGH bug)", i)
		}
		if !hostLive && !scLive {
			t.Fatalf("boot %d: job vanished from BOTH schedulers — lost a scheduled job", i)
		}
		if markerExists(t, marker) {
			t.Fatalf("boot %d: marker written despite the host wipe failing", i)
		}
	}

	// Now the wipe succeeds. The migration completes: job flips to supercronic
	// ONLY, host is clear, marker written.
	h.wipeFail = false
	migrateHostCrontab(home, nil)

	if strings.Contains(h.body, job) {
		t.Fatalf("after a successful wipe the job is still live in host cron:\n%q", h.body)
	}
	if !supercronicLive(t, home, job) {
		t.Fatalf("after migration the job must be live in supercronic; it is missing")
	}
	if !markerExists(t, marker) {
		t.Fatalf("marker not written after the wipe finally succeeded")
	}
	if n := countSubstr(mustRead(t, filepath.Join(home, "crontab")), job); n != 1 {
		t.Fatalf("job scheduled %d times in supercronic after recovery (want exactly 1)", n)
	}
}

// TestMigrateHostCrontab_RecoversAfterWipeOkButCrashBeforePromote covers the
// crash window: the host wipe succeeds, but the daemon dies before the entries
// are made live in supercronic and before the marker is written. On the next
// boot the host crontab is already empty, so a naive "empty host -> mark and
// do nothing" would silently LOSE the jobs. The migration must instead
// recover the staged snapshot and finish the promotion.
func TestMigrateHostCrontab_RecoversAfterWipeOkButCrashBeforePromote(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	const job = "/home/vibecraft/nightly.sh"
	h := &hostCron{body: "@daily " + job + "\n"}

	// crashAfterWipe forces a return immediately after the wipe succeeds, before
	// the entries are promoted into the live crontab / the marker is set —
	// simulating a daemon crash in that window.
	crashAfterWipe := true
	withCrontabSeams(t, marker, h.list, func() error {
		if err := h.wipe(); err != nil {
			return err
		}
		if crashAfterWipe {
			crashAfterWipe = false
			panic("simulated crash immediately after host wipe")
		}
		return nil
	})

	// First boot: wipe succeeds then we crash before promoting. Recover the
	// panic the way a fresh process boot would (the state on disk is what
	// matters across the boundary).
	func() {
		defer func() { _ = recover() }()
		migrateHostCrontab(home, nil)
	}()

	if strings.Contains(h.body, job) {
		t.Fatalf("host crontab should already be wiped after the first (crashing) boot")
	}
	if markerExists(t, marker) {
		t.Fatalf("marker must not be set when we crashed before completing the promotion")
	}

	// Second boot: host is empty, but the staged snapshot must let the
	// migration finish — the job lands in supercronic and the marker is set.
	migrateHostCrontab(home, nil)

	if !supercronicLive(t, home, job) {
		t.Fatalf("job lost after wipe-then-crash recovery: not live in supercronic")
	}
	if !markerExists(t, marker) {
		t.Fatalf("marker not set after recovery completed the migration")
	}
	if n := countSubstr(mustRead(t, filepath.Join(home, "crontab")), job); n != 1 {
		t.Fatalf("recovered job scheduled %d times (want exactly 1)", n)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestMigrateHostCrontab_RetryDoesNotDuplicateEntries is the regression guard
// for the multi-execution bug: when the host wipe keeps failing across multiple
// boots, each re-run of the migration must NOT accumulate copies. With the
// exactly-once-total fix the entries on the wipe-failure path are STAGED (not
// live in supercronic) and the staging write is an overwrite, so the staged
// snapshot holds exactly one copy of the job no matter how many times the
// migration retries — and crucially the job is never live in supercronic while
// the host copy is still live.
func TestMigrateHostCrontab_RetryDoesNotDuplicateEntries(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	const job = "/home/vibecraft/loop.sh"
	const entries = "*/5 * * * * " + job + "\n"
	withCrontabSeams(t, marker,
		// crontabList keeps returning the host entries because the wipe never
		// succeeds — exactly the real-world condition that triggered the bug.
		func() ([]byte, error) { return []byte(entries), nil },
		func() error { return errors.New("crontab: persistent permission denied") },
	)

	// Three boots, all with a failing wipe (the daemon restarts re-run the
	// migration because the marker is never written on the wipe-failure path).
	for i := 0; i < 3; i++ {
		migrateHostCrontab(home, nil)
		if markerExists(t, marker) {
			t.Fatalf("boot %d: marker written despite persistent wipe failure", i)
		}
		// The job must never go live in supercronic while the host copy is live.
		if supercronicLive(t, home, job) {
			t.Fatalf("boot %d: job live in supercronic while host wipe failing — double-run", i)
		}
	}

	staged, err := os.ReadFile(filepath.Join(home, crontabMigrateStagingName))
	if err != nil {
		t.Fatalf("read staged snapshot: %v", err)
	}
	if n := countSubstr(string(staged), job); n != 1 {
		t.Fatalf("staged snapshot holds %d copies after 3 wipe-failing boots (want exactly 1):\n%s", n, staged)
	}
}

// TestMigrateHostCrontab_PromotePreservesManagerAuthoredContent proves the
// idempotent strip-then-append promotion preserves crontab lines the manager
// authored OUTSIDE the migration block, places the migrated host entries below
// them, and does not duplicate either across a re-run of the promotion (the
// crash-between-promote-and-staging-removal window).
func TestMigrateHostCrontab_PromotePreservesManagerAuthoredContent(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	// The manager already authored a job before the migration ever runs.
	const managerJob = "0 8 * * * /home/vibecraft/systems/brief/run.sh\n"
	if err := os.WriteFile(filepath.Join(home, "crontab"), []byte(managerJob), 0600); err != nil {
		t.Fatalf("seed manager crontab: %v", err)
	}

	const hostEntries = "*/10 * * * * /home/vibecraft/legacy.sh\n"
	h := &hostCron{body: hostEntries}
	withCrontabSeams(t, marker, h.list, h.wipe)

	// First migration: wipe succeeds, entries promoted into the live crontab.
	migrateHostCrontab(home, nil)
	if !markerExists(t, marker) {
		t.Fatalf("marker not written after a successful migration")
	}

	// Simulate a crash that happened AFTER the live crontab was written but
	// BEFORE the staging file was removed and the marker set: re-stage the
	// snapshot, drop the marker, and re-run. The promotion must be idempotent.
	if err := os.WriteFile(filepath.Join(home, crontabMigrateStagingName), []byte(hostEntries), 0600); err != nil {
		t.Fatalf("re-stage snapshot: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	migrateHostCrontab(home, nil)

	got, err := os.ReadFile(filepath.Join(home, "crontab"))
	if err != nil {
		t.Fatalf("read agent-shell crontab: %v", err)
	}
	gs := string(got)
	if n := countSubstr(gs, "/home/vibecraft/systems/brief/run.sh"); n != 1 {
		t.Fatalf("manager-authored job count = %d, want 1 (idempotent strip must preserve it exactly once):\n%s", n, gs)
	}
	if n := countSubstr(gs, "/home/vibecraft/legacy.sh"); n != 1 {
		t.Fatalf("migrated host job count = %d, want 1 (no duplication across a re-promotion):\n%s", n, gs)
	}
	if n := countSubstr(gs, crontabMigrateBeginMarker); n != 1 {
		t.Fatalf("expected exactly 1 migration block, got %d:\n%s", n, gs)
	}
	// The manager content must remain ABOVE the migration block.
	if mgrIdx, blkIdx := strings.Index(gs, "brief/run.sh"), strings.Index(gs, crontabMigrateBeginMarker); mgrIdx < 0 || blkIdx < 0 || mgrIdx > blkIdx {
		t.Fatalf("manager content should sit above the migration block; mgrIdx=%d blkIdx=%d:\n%s", mgrIdx, blkIdx, gs)
	}
	// The staging file must be cleaned up once promotion completes.
	if _, err := os.Stat(filepath.Join(home, crontabMigrateStagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file should be removed after a completed promotion; stat err=%v", err)
	}
}

// TestMigrateHostCrontab_RetryAfterWipeFailureSucceeds proves the recovery
// behavior: because the first (wipe-failing) run did not mark migrated, a
// second run actually re-attempts the migration, and once the wipe succeeds
// the marker is finally written.
func TestMigrateHostCrontab_RetryAfterWipeFailureSucceeds(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	const entries = "0 9 * * 1 /home/vibecraft/weekly.sh\n"

	wipeShouldFail := true
	wipeCalls := 0
	withCrontabSeams(t, marker,
		func() ([]byte, error) { return []byte(entries), nil },
		func() error {
			wipeCalls++
			if wipeShouldFail {
				return errors.New("crontab: transient failure")
			}
			return nil
		},
	)

	// First boot: wipe fails -> no marker.
	migrateHostCrontab(home, nil)
	if markerExists(t, marker) {
		t.Fatalf("marker written despite wipe failure on first run")
	}

	// Second boot: wipe now succeeds -> migration proceeds and marker is set.
	wipeShouldFail = false
	migrateHostCrontab(home, nil)
	if !markerExists(t, marker) {
		t.Fatalf("marker not written after a successful retry; migration would never complete")
	}
	if wipeCalls != 2 {
		t.Fatalf("expected wipe to be retried (2 calls), got %d", wipeCalls)
	}

	// The schedule locked in by the marker must contain the job exactly once —
	// the prior (wipe-failing) boot must not have left a duplicate that the
	// successful run then preserves and seals in permanently.
	got, err := os.ReadFile(filepath.Join(home, "crontab"))
	if err != nil {
		t.Fatalf("read agent-shell crontab: %v", err)
	}
	if n := countSubstr(string(got), "/home/vibecraft/weekly.sh"); n != 1 {
		t.Fatalf("after recovery the job is scheduled %d times (want exactly 1); a duplicate was sealed in:\n%s", n, got)
	}
}

// TestMigrateHostCrontab_ClearFailureThenCleanRetry_NoHostDuplication is the
// regression guard for the NEW-BUG fast-follow: the claim was that writing the
// migration marker only after the host crontab is cleared made the RETRY path
// duplicate host crontab entries on every boot. This test models the host
// crontab as MUTABLE state (the real seam talks to /etc) and walks the exact
// scenario the bug describes — a clear FAILURE on the first boot followed by a
// clean RETRY that succeeds — asserting:
//
//   - the host crontab itself never accumulates duplicate entries across the
//     failure+retry boots (a naive "append into supercronic, then wipe" or a
//     "re-stack onto the host" path would multiply it),
//   - the job ends up scheduled EXACTLY ONCE TOTAL across host cron +
//     supercronic (not 0, not 2, not N),
//   - it is never live in BOTH schedulers simultaneously on any boot.
//
// This is independent of the other retry tests: those use a wipe shim that
// always fails or a fixed list shim; this one threads real host state through
// list+wipe so a duplicate appended onto the HOST would be observable.
func TestMigrateHostCrontab_ClearFailureThenCleanRetry_NoHostDuplication(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	const job = "/home/vibecraft/loop.sh"
	const line = "*/5 * * * * " + job

	h := &hostCron{body: line + "\n", wipeFail: true}
	withCrontabSeams(t, marker, h.list, h.wipe)

	// Boot 1: the clear (wipe) FAILS. The job must remain live ONLY in the host
	// crontab, with exactly one copy — no duplicate appended onto the host, none
	// promoted into supercronic, no marker.
	migrateHostCrontab(home, nil)

	if n := countSubstr(h.body, line); n != 1 {
		t.Fatalf("boot 1 (clear-failure): host crontab holds %d copies of the job (want exactly 1 — no duplication):\n%q", n, h.body)
	}
	if supercronicLive(t, home, job) {
		t.Fatalf("boot 1 (clear-failure): job promoted into supercronic while the host copy is still live — double-run")
	}
	if markerExists(t, marker) {
		t.Fatalf("boot 1 (clear-failure): marker written despite the clear failing")
	}

	// Boot 2: same persistent failure (a reboot before the operator fixes perms).
	// Still exactly one copy on the host, still nothing in supercronic.
	migrateHostCrontab(home, nil)

	if n := countSubstr(h.body, line); n != 1 {
		t.Fatalf("boot 2 (clear-failure again): host crontab holds %d copies of the job (want exactly 1 — retry must not append onto the host):\n%q", n, h.body)
	}
	if supercronicLive(t, home, job) {
		t.Fatalf("boot 2: job in supercronic while host copy still live — double-run")
	}

	// Boot 3: clean RETRY — the clear now succeeds. The job flips to supercronic
	// ONLY: host is empty, supercronic holds exactly one copy, marker set.
	h.wipeFail = false
	migrateHostCrontab(home, nil)

	if strings.TrimSpace(h.body) != "" {
		t.Fatalf("after the clean retry the host crontab should be empty; got:\n%q", h.body)
	}
	scCopies := countSubstr(mustRead(t, filepath.Join(home, "crontab")), job)
	if scCopies != 1 {
		t.Fatalf("after the clean retry the job is scheduled %d times in supercronic (want exactly 1 TOTAL):\n%s", scCopies, mustRead(t, filepath.Join(home, "crontab")))
	}
	if !markerExists(t, marker) {
		t.Fatalf("marker not written after the clean retry completed the migration")
	}

	// Boot 4: a reboot AFTER completion must short-circuit on the marker — no
	// re-migration, no second wipe attempt, supercronic copy stays at one.
	wipesBefore := h.wipes
	migrateHostCrontab(home, nil)
	if h.wipes != wipesBefore {
		t.Fatalf("migration re-ran after the marker was set; extra wipe attempts: %d", h.wipes-wipesBefore)
	}
	if n := countSubstr(mustRead(t, filepath.Join(home, "crontab")), job); n != 1 {
		t.Fatalf("post-completion reboot changed the supercronic schedule to %d copies (want 1)", n)
	}
}

// TestMigrateHostCrontab_WipeSuccessMarks confirms the happy path still marks
// migrated exactly once so reboots do not re-migrate.
func TestMigrateHostCrontab_WipeSuccessMarks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	wipeCalls := 0
	withCrontabSeams(t, marker,
		func() ([]byte, error) { return []byte("@daily /home/vibecraft/nightly.sh\n"), nil },
		func() error { wipeCalls++; return nil },
	)

	migrateHostCrontab(home, nil)
	if !markerExists(t, marker) {
		t.Fatalf("marker not written on successful migration")
	}

	// A subsequent boot must short-circuit on the existing marker (no second
	// wipe, no re-migration).
	migrateHostCrontab(home, nil)
	if wipeCalls != 1 {
		t.Fatalf("migration re-ran after marker present; wipe called %d times (want 1)", wipeCalls)
	}
}

// TestMigrateHostCrontab_NoHostCrontabMarks covers the common clean case:
// `crontab -l` exits non-zero ("no crontab for vibecraft"). Nothing is
// copied, so marking is safe and required (otherwise every boot re-checks).
func TestMigrateHostCrontab_NoHostCrontabMarks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	wipeCalls := 0
	withCrontabSeams(t, marker,
		func() ([]byte, error) { return nil, errors.New("no crontab for vibecraft") },
		func() error { wipeCalls++; return nil },
	)

	migrateHostCrontab(home, nil)
	if !markerExists(t, marker) {
		t.Fatalf("marker not written on the no-host-crontab path")
	}
	if wipeCalls != 0 {
		t.Fatalf("wipe should not run when there is no host crontab; called %d times", wipeCalls)
	}
	if _, err := os.Stat(filepath.Join(home, "crontab")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent-shell crontab should not be created when there is nothing to migrate")
	}
}

// TestMigrateHostCrontab_EmptyHostCrontabMarks covers the empty-but-present
// host crontab: nothing to migrate, marker written, no wipe.
func TestMigrateHostCrontab_EmptyHostCrontabMarks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".crontab-migrated")
	home := t.TempDir()

	wipeCalls := 0
	withCrontabSeams(t, marker,
		func() ([]byte, error) { return []byte("   \n\t\n"), nil },
		func() error { wipeCalls++; return nil },
	)

	migrateHostCrontab(home, nil)
	if !markerExists(t, marker) {
		t.Fatalf("marker not written on the empty-host-crontab path")
	}
	if wipeCalls != 0 {
		t.Fatalf("wipe should not run for an empty host crontab; called %d times", wipeCalls)
	}
}
