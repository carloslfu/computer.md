// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
