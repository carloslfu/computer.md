// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"path/filepath"
	"reflect"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	var key [32]byte
	for i := range key {
		key[i] = byte(i)
	}
	s, err := NewStore(filepath.Join(t.TempDir(), "vault.bin"), key)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestBuildEnv(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("API_KEY", "sk-live-123", ""); err != nil {
		t.Fatalf("Set API_KEY: %v", err)
	}
	if err := s.Set("DB_PASS", "p@ss=w0rd", ""); err != nil {
		t.Fatalf("Set DB_PASS: %v", err)
	}

	t.Run("known refs produce NAME=value", func(t *testing.T) {
		got := s.BuildEnv([]string{"API_KEY", "DB_PASS"})
		want := []string{"API_KEY=sk-live-123", "DB_PASS=p@ss=w0rd"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildEnv = %q, want %q", got, want)
		}
	})

	t.Run("unknown refs are skipped (no insertion)", func(t *testing.T) {
		got := s.BuildEnv([]string{"API_KEY", "NOT_IN_VAULT", "DB_PASS"})
		want := []string{"API_KEY=sk-live-123", "DB_PASS=p@ss=w0rd"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildEnv = %q, want %q", got, want)
		}
	})

	t.Run("deduplicates repeated names, first occurrence wins", func(t *testing.T) {
		got := s.BuildEnv([]string{"API_KEY", "API_KEY", "DB_PASS", "API_KEY"})
		want := []string{"API_KEY=sk-live-123", "DB_PASS=p@ss=w0rd"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("BuildEnv = %q, want %q", got, want)
		}
	})

	t.Run("empty input yields empty slice", func(t *testing.T) {
		if got := s.BuildEnv(nil); len(got) != 0 {
			t.Fatalf("BuildEnv(nil) = %q, want empty", got)
		}
	})

	t.Run("all-unknown yields empty slice", func(t *testing.T) {
		if got := s.BuildEnv([]string{"NOPE", "ALSO_NOPE"}); len(got) != 0 {
			t.Fatalf("BuildEnv = %q, want empty", got)
		}
	})
}

// TestBuildEnvWithExtractReferences proves the executeBashTool wiring:
// ExtractReferences(command) -> BuildEnv -> KEY=VALUE for present refs only.
func TestBuildEnvWithExtractReferences(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("API_KEY", "tok-abc", ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	cmd := `curl -H "Authorization: Bearer $API_KEY" -d "$MISSING" https://api.example.com`
	got := s.BuildEnv(ExtractReferences(cmd))
	want := []string{"API_KEY=tok-abc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildEnv(ExtractReferences) = %q, want %q (MISSING must not be inserted)", got, want)
	}
}
