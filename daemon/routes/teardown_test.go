// SPDX-License-Identifier: Apache-2.0

package routes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnregisterTearsDownAppService pins the fix for the high-severity
// teardown leak: removing a hosted app must NOT leave its systemd-user
// unit running or its 0600 <name>.env secret sidecar on disk.
//
// Before the fix, Manager.Unregister only deleted the Caddy route — the
// backend process kept running (Restart=on-failure + linger) and the
// env file holding resolved vault secrets + the injected
// VIBECRAFT_AI_CREDITS_TOKEN persisted indefinitely. This test asserts
// the real teardown behavior:
//   - `systemctl --user disable --now <name>.service` is invoked (stops
//     AND disables the unit in one call), and
//   - the on-disk unit file AND the secret env sidecar are both removed.
//
// It records the actual systemctl invocations and exercises real
// os.Remove against temp files so it asserts behavior tied to the bug,
// not a tautology over a mock.
func TestUnregisterTearsDownAppService(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	const name = "myapp"

	// Lay down the real on-disk artifacts install-app-service would have
	// written: a unit file and its 0600 secret sidecar, in a redirected
	// systemd dir so we never touch a real one.
	dir := t.TempDir()
	restoreDir := systemdUserDir
	systemdUserDir = dir
	t.Cleanup(func() { systemdUserDir = restoreDir })

	unitPath := filepath.Join(dir, name+".service")
	envPath := filepath.Join(dir, name+".env")
	if err := os.WriteFile(unitPath, []byte("[Unit]\nDescription=myapp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 0600 sidecar carrying a live secret remnant — exactly what must not
	// survive a removal.
	if err := os.WriteFile(envPath, []byte("VIBECRAFT_AI_CREDITS_TOKEN=live-secret\nAPI_KEY=sk-live-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Capture systemctl invocations instead of running them. Keep the
	// real os.Remove so the file-removal assertions test actual behavior.
	var gotCmds [][]string
	restoreRun := runUserSystemctlFn
	runUserSystemctlFn = func(_ context.Context, args ...string) (string, error) {
		gotCmds = append(gotCmds, append([]string(nil), args...))
		return "", nil
	}
	t.Cleanup(func() { runUserSystemctlFn = restoreRun })

	// Stub Caddy reload out — buildCaddyfile + reload touch /etc/caddy and
	// a real caddy binary that aren't present under test. We assert
	// teardown happens regardless of the (best-effort) reload result.
	restoreReload := regenReload
	regenReload = func(*Manager) error { return nil }
	t.Cleanup(func() { regenReload = restoreReload })

	// Register the route in the DB so DeleteRoute succeeds (Unregister
	// returns the DeleteRoute error otherwise).
	if err := db.CreateRoute(name, 3000); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Unregister(name); err != nil {
		t.Fatalf("Unregister returned error: %v", err)
	}

	// 1. The unit must have been disabled-and-stopped. A bare `disable`
	//    (no --now) would remove the symlink but leave the process
	//    running, so assert --now is present.
	var sawDisableNow bool
	for _, c := range gotCmds {
		joined := strings.Join(c, " ")
		if strings.HasPrefix(joined, "disable") && contains(joined, "--now") && contains(joined, name+".service") {
			sawDisableNow = true
		}
	}
	if !sawDisableNow {
		t.Errorf("teardown did not run `systemctl --user disable --now %s.service`; got invocations: %v", name, gotCmds)
	}

	// 2. The unit file must be gone.
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file %s still exists after Unregister (stat err=%v)", unitPath, err)
	}

	// 3. The 0600 secret sidecar must be gone — this is the leak the
	//    finding flagged. Its survival is the actual harm.
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Errorf("secret env sidecar %s still exists after Unregister (stat err=%v) — resolved vault secrets + AI-credits token leaked on disk", envPath, err)
	}
}

// TestUnregisterTeardownToleratesNoUnit pins the idempotency contract: a
// plain port route (no install-app-service unit/env behind it) must
// Unregister cleanly. Teardown removal is best-effort over files that
// may not exist, and a missing unit must not be reported as an error to
// the caller.
func TestUnregisterTeardownToleratesNoUnit(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	dir := t.TempDir()
	restoreDir := systemdUserDir
	systemdUserDir = dir
	t.Cleanup(func() { systemdUserDir = restoreDir })

	// No unit / env files on disk — a plain port route.
	restoreRun := runUserSystemctlFn
	runUserSystemctlFn = func(context.Context, ...string) (string, error) { return "", nil }
	t.Cleanup(func() { runUserSystemctlFn = restoreRun })

	restoreReload := regenReload
	regenReload = func(*Manager) error { return nil }
	t.Cleanup(func() { regenReload = restoreReload })

	if err := db.CreateRoute("plain", 4000); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Unregister("plain"); err != nil {
		t.Fatalf("Unregister of a plain port route (no backing unit) errored: %v", err)
	}
}
