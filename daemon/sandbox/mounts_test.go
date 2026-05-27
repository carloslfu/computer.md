// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendExistingReadOnlyMounts(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	existing := filepath.Join(dir, "google")
	if err := os.MkdirAll(existing, 0755); err != nil {
		t.Fatal(err)
	}

	mounts := appendExistingReadOnlyMounts([]Mount{{HostPath: "/home/vibecraft", SandboxPath: "/home/vibecraft"}}, missing, existing)
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(mounts), mounts)
	}
	got := mounts[1]
	if got.HostPath != existing || got.SandboxPath != existing || !got.ReadOnly {
		t.Fatalf("unexpected optional mount: %+v", got)
	}
}
