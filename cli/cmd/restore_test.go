// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRestoreSnapshot_RejectsPathTraversal verifies that 'backup restore
// --file' refuses any name that is not a bare basename inside the backups
// directory. A crafted --file ("../../etc/x", an absolute path, or a name
// with a slash) must NOT be read or copied over the live daemon DB.
func TestRestoreSnapshot_RejectsPathTraversal(t *testing.T) {
	backups := t.TempDir()
	data := t.TempDir()
	t.Setenv("VIBECRAFT_BACKUPS_DIR", backups)
	t.Setenv("VIBECRAFT_DATA_DIR", data)

	// A legit snapshot inside the backups dir (proves the guard rejects
	// for the traversal reason, not because the file is missing).
	if err := os.WriteFile(filepath.Join(backups, "good.db"), []byte("snapshot-bytes"), 0600); err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}
	// A secret file OUTSIDE the backups dir that a traversal would target.
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "secret.db")
	if err := os.WriteFile(secretPath, []byte("do-not-copy-me"), 0600); err != nil {
		t.Fatalf("seeding secret: %v", err)
	}

	dst := filepath.Join(data, "vibecraft.db")

	traversals := []struct {
		name string
		arg  string
	}{
		{"parent-relative", filepath.Join("..", filepath.Base(secretDir), "secret.db")},
		{"absolute", secretPath},
		{"nested-slash", "sub/good.db"},
		{"dotdot", ".."},
		{"empty", ""},
	}

	for _, tc := range traversals {
		t.Run(tc.name, func(t *testing.T) {
			err := restoreSnapshot(tc.arg)
			if err == nil {
				t.Fatalf("restoreSnapshot(%q) = nil, want a rejection error", tc.arg)
			}
			// The DB must not have been created/overwritten.
			if _, statErr := os.Stat(dst); statErr == nil {
				t.Fatalf("restoreSnapshot(%q) wrote the daemon DB at %s; traversal was not blocked", tc.arg, dst)
			}
			// And the secret must be untouched (no read-then-copy).
			if b, _ := os.ReadFile(secretPath); string(b) != "do-not-copy-me" {
				t.Fatalf("secret file was modified by restoreSnapshot(%q)", tc.arg)
			}
		})
	}

	// Sanity: a bare, valid basename passes the guard and restores.
	if err := restoreSnapshot("good.db"); err != nil {
		t.Fatalf("restoreSnapshot(\"good.db\") = %v, want success", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading restored db: %v", err)
	}
	if string(got) != "snapshot-bytes" {
		t.Errorf("restored db content = %q, want %q", string(got), "snapshot-bytes")
	}
}

// TestRestoreSnapshot_BasenameWithDotComponentsRejected guards the
// specific "looks like a basename but contains a separator" inputs.
func TestRestoreSnapshot_RejectsDotDotBasename(t *testing.T) {
	backups := t.TempDir()
	data := t.TempDir()
	t.Setenv("VIBECRAFT_BACKUPS_DIR", backups)
	t.Setenv("VIBECRAFT_DATA_DIR", data)

	err := restoreSnapshot("../../etc/passwd")
	if err == nil {
		t.Fatal("restoreSnapshot(\"../../etc/passwd\") = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "bare snapshot name") && !strings.Contains(err.Error(), "outside the backups") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestDaemonRunning_DetectsViaSystemctl proves the safety gate is live: an
// "active" verdict from systemctl must mark the daemon as running so the
// restore refuses to overwrite the DB while the daemon holds it open. The
// old pidfile/socket-stat implementation could never observe this (Type=simple
// has no PIDFile) and always returned false, leaving the gate dead.
func TestDaemonRunning_DetectsViaSystemctl(t *testing.T) {
	// Neutralize the port probe so this test isolates the systemctl path and
	// never depends on whatever is (or isn't) bound on the host.
	prevPort := daemonPortListening
	daemonPortListening = func() bool { return false }
	t.Cleanup(func() { daemonPortListening = prevPort })

	cases := []struct {
		name    string
		state   string
		ok      bool
		running bool
	}{
		{"active", "active", true, true},
		{"activating", "activating", true, true},
		{"inactive", "inactive", true, false},
		{"failed", "failed", true, false},
		{"unknown-inconclusive", "unknown", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := systemctlIsActive
			systemctlIsActive = func(unit string) (bool, bool) {
				if unit != "vibecraft-daemon" {
					t.Fatalf("systemctlIsActive queried unexpected unit %q", unit)
				}
				switch tc.state {
				case "active", "activating", "reloading":
					return true, tc.ok
				default:
					return false, tc.ok
				}
			}
			t.Cleanup(func() { systemctlIsActive = prev })

			got, err := daemonRunning()
			if err != nil {
				t.Fatalf("daemonRunning() error = %v", err)
			}
			if got != tc.running {
				t.Fatalf("daemonRunning() = %v, want %v for systemctl state %q", got, tc.running, tc.state)
			}
		})
	}
}

// TestDaemonRunning_DetectsViaPortProbe proves the fallback: even when
// systemctl is inconclusive (no systemd, e.g. a dev/BYOM box running the
// daemon by hand), a process bound to the daemon port still counts as running.
func TestDaemonRunning_DetectsViaPortProbe(t *testing.T) {
	prevSys := systemctlIsActive
	systemctlIsActive = func(string) (bool, bool) { return false, false } // inconclusive
	t.Cleanup(func() { systemctlIsActive = prevSys })

	prevPort := daemonPortListening
	t.Cleanup(func() { daemonPortListening = prevPort })

	daemonPortListening = func() bool { return true }
	got, err := daemonRunning()
	if err != nil {
		t.Fatalf("daemonRunning() error = %v", err)
	}
	if !got {
		t.Fatal("daemonRunning() = false with daemon port listening, want true")
	}

	daemonPortListening = func() bool { return false }
	got, err = daemonRunning()
	if err != nil {
		t.Fatalf("daemonRunning() error = %v", err)
	}
	if got {
		t.Fatal("daemonRunning() = true with nothing listening and systemctl inconclusive, want false")
	}
}

// TestRestoreSnapshot_SafetyBackupNamedWithNow proves two restores of the SAME
// snapshot do not clobber each other's safety backup. The old code named the
// .bak file with the snapshot's mtime, so restoring the same snapshot twice
// produced an identical .bak name and overwrote the first safety copy — the
// live DB it was meant to preserve. The name must derive from "now", so the
// first safety backup survives the second restore.
func TestRestoreSnapshot_SafetyBackupNamedWithNow(t *testing.T) {
	backups := t.TempDir()
	data := t.TempDir()
	t.Setenv("VIBECRAFT_BACKUPS_DIR", backups)
	t.Setenv("VIBECRAFT_DATA_DIR", data)

	// One snapshot with a FIXED mtime; restoring it twice must still yield two
	// distinct safety backups because the name comes from now(), not the mtime.
	snap := filepath.Join(backups, "snap.db")
	if err := os.WriteFile(snap, []byte("snapshot-content"), 0600); err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}
	fixedMtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(snap, fixedMtime, fixedMtime); err != nil {
		t.Fatalf("setting snapshot mtime: %v", err)
	}

	// Prime an existing live DB so the FIRST restore already has something to
	// back up — this makes nowUTC drive the safety-backup name on restore #1.
	dst := filepath.Join(data, "vibecraft.db")
	if err := os.WriteFile(dst, []byte("live-state"), 0600); err != nil {
		t.Fatalf("priming live db: %v", err)
	}

	// Drive nowUTC so the two restores get clearly distinct timestamps that we
	// control — no sleeping, no flakiness.
	times := []time.Time{
		time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 9, 11, 0, 0, 0, time.UTC),
	}
	idx := 0
	prevNow := nowUTC
	nowUTC = func() time.Time {
		tm := times[idx%len(times)]
		idx++
		return tm
	}
	t.Cleanup(func() { nowUTC = prevNow })

	// Restore #1: backs up the primed live DB at now()=10:00.
	if err := restoreSnapshot("snap.db"); err != nil {
		t.Fatalf("first restoreSnapshot: %v", err)
	}
	// Restore #2: backs up the DB restore #1 wrote, at now()=11:00. Under the
	// buggy mtime-based naming both backups would share the snapshot's mtime
	// and the second would clobber the first.
	if err := restoreSnapshot("snap.db"); err != nil {
		t.Fatalf("second restoreSnapshot: %v", err)
	}

	entries, err := os.ReadDir(data)
	if err != nil {
		t.Fatalf("reading data dir: %v", err)
	}
	var baks []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak-") {
			baks = append(baks, e.Name())
		}
	}
	// Two restores, two distinct now()-stamped safety backups. The buggy code
	// would produce ONE (the snapshot mtime collided and clobbered).
	if len(baks) != 2 {
		t.Fatalf("expected 2 distinct safety backups, got %d: %v (mtime-naming clobbered one)", len(baks), baks)
	}
	for _, b := range baks {
		if strings.Contains(b, "2026-01-02T03-04-05") {
			t.Fatalf("safety backup named with snapshot mtime, not now: %q", b)
		}
	}
	names := strings.Join(baks, " ")
	if !strings.Contains(names, "2026-06-09T10-00-00") || !strings.Contains(names, "2026-06-09T11-00-00") {
		t.Fatalf("safety backups not named with now(); got %v, want both restore times", baks)
	}
}

// TestRestoreSnapshot_TwoRestoresKeepBothSafetyBackups is the direct
// regression for the clobber: prime an EXISTING live DB, run two restores of
// the same snapshot, and confirm both pre-restore states are preserved as
// separate .bak files. Under the buggy mtime-based naming the second backup
// would overwrite the first.
func TestRestoreSnapshot_TwoRestoresKeepBothSafetyBackups(t *testing.T) {
	backups := t.TempDir()
	data := t.TempDir()
	t.Setenv("VIBECRAFT_BACKUPS_DIR", backups)
	t.Setenv("VIBECRAFT_DATA_DIR", data)

	snap := filepath.Join(backups, "snap.db")
	if err := os.WriteFile(snap, []byte("snapshot-content"), 0600); err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}
	fixedMtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(snap, fixedMtime, fixedMtime); err != nil {
		t.Fatalf("setting snapshot mtime: %v", err)
	}

	dst := filepath.Join(data, "vibecraft.db")
	// Prime a live DB with distinct content so we can tell the first safety
	// backup apart from anything else.
	if err := os.WriteFile(dst, []byte("live-state-A"), 0600); err != nil {
		t.Fatalf("priming live db: %v", err)
	}

	times := []time.Time{
		time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 9, 11, 0, 0, 0, time.UTC),
	}
	idx := 0
	prevNow := nowUTC
	nowUTC = func() time.Time {
		tm := times[idx%len(times)]
		idx++
		return tm
	}
	t.Cleanup(func() { nowUTC = prevNow })

	// Restore #1: backs up "live-state-A".
	if err := restoreSnapshot("snap.db"); err != nil {
		t.Fatalf("first restoreSnapshot: %v", err)
	}
	// Mutate the live DB so restore #2 backs up a DIFFERENT pre-restore state.
	if err := os.WriteFile(dst, []byte("live-state-B"), 0600); err != nil {
		t.Fatalf("mutating live db: %v", err)
	}
	// Restore #2: backs up "live-state-B".
	if err := restoreSnapshot("snap.db"); err != nil {
		t.Fatalf("second restoreSnapshot: %v", err)
	}

	entries, err := os.ReadDir(data)
	if err != nil {
		t.Fatalf("reading data dir: %v", err)
	}
	contents := map[string]bool{}
	bakCount := 0
	for _, e := range entries {
		if !strings.Contains(e.Name(), ".bak-") {
			continue
		}
		bakCount++
		b, err := os.ReadFile(filepath.Join(data, e.Name()))
		if err != nil {
			t.Fatalf("reading backup %s: %v", e.Name(), err)
		}
		contents[string(b)] = true
	}
	if bakCount != 2 {
		t.Fatalf("expected 2 distinct safety backups, got %d (the second clobbered the first)", bakCount)
	}
	if !contents["live-state-A"] || !contents["live-state-B"] {
		t.Fatalf("both pre-restore states must survive; got backups with contents %v", contents)
	}
}

// TestRestoreSnapshot_RemovesStaleWALSidecars is the regression for the
// WAL-corruption hazard: the live DB runs in WAL mode and leaves -wal/-shm
// sidecars. A snapshot is a clean single-file VACUUM INTO, so the OLD DB's
// sidecars MUST be removed during restore — otherwise the daemon's next
// open checkpoints a stale WAL onto the freshly restored main file and
// corrupts (or reverts) it.
func TestRestoreSnapshot_RemovesStaleWALSidecars(t *testing.T) {
	backups := t.TempDir()
	data := t.TempDir()
	t.Setenv("VIBECRAFT_BACKUPS_DIR", backups)
	t.Setenv("VIBECRAFT_DATA_DIR", data)

	snap := filepath.Join(backups, "snap.db")
	if err := os.WriteFile(snap, []byte("snapshot-content"), 0600); err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}

	dst := filepath.Join(data, "vibecraft.db")
	if err := os.WriteFile(dst, []byte("live-state"), 0600); err != nil {
		t.Fatalf("priming live db: %v", err)
	}
	// Stale WAL/SHM sidecars from the previous live DB.
	wal := dst + "-wal"
	shm := dst + "-shm"
	if err := os.WriteFile(wal, []byte("stale-wal"), 0600); err != nil {
		t.Fatalf("priming wal: %v", err)
	}
	if err := os.WriteFile(shm, []byte("stale-shm"), 0600); err != nil {
		t.Fatalf("priming shm: %v", err)
	}

	if err := restoreSnapshot("snap.db"); err != nil {
		t.Fatalf("restoreSnapshot: %v", err)
	}

	if _, err := os.Stat(wal); !os.IsNotExist(err) {
		t.Fatalf("stale -wal sidecar still present after restore (err=%v); the daemon would checkpoint it onto the restored DB", err)
	}
	if _, err := os.Stat(shm); !os.IsNotExist(err) {
		t.Fatalf("stale -shm sidecar still present after restore (err=%v)", err)
	}
	// The main file is the restored snapshot.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading restored db: %v", err)
	}
	if string(got) != "snapshot-content" {
		t.Fatalf("restored db = %q, want the snapshot content", string(got))
	}
}
