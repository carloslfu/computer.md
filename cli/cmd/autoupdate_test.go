// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAutoUpdateDue(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), autoUpdateStampName)

	if !autoUpdateDue(stamp, autoUpdateInterval) {
		t.Error("a missing stamp must count as due")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(stamp, []byte(now), 0600); err != nil {
		t.Fatal(err)
	}
	if autoUpdateDue(stamp, autoUpdateInterval) {
		t.Error("a stamp written just now must not be due")
	}

	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(stamp, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if !autoUpdateDue(stamp, autoUpdateInterval) {
		t.Error("a 48h-old stamp must be due against a 24h interval")
	}

	if err := os.WriteFile(stamp, []byte("not-a-timestamp"), 0600); err != nil {
		t.Fatal(err)
	}
	if !autoUpdateDue(stamp, autoUpdateInterval) {
		t.Error("an unparseable stamp must fail open to due")
	}
}

func TestWriteAutoUpdateStamp(t *testing.T) {
	// The parent directory does not exist yet — writeAutoUpdateStamp must
	// create it (mirrors a fresh install that has never run `auth login`).
	stamp := filepath.Join(t.TempDir(), "config", autoUpdateStampName)

	writeAutoUpdateStamp(stamp)

	if autoUpdateDue(stamp, autoUpdateInterval) {
		t.Error("after writeAutoUpdateStamp the check must not be due")
	}
	info, err := os.Stat(stamp)
	if err != nil {
		t.Fatalf("stamp not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("stamp mode = %o, want 600", perm)
	}
}
