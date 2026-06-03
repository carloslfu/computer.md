// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

func TestLogSanitizesLocalDetails(t *testing.T) {
	db, err := persistence.Open(filepath.Join(t.TempDir(), "audit.db"), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	l := NewLogger(db)
	rawKey := "vc_machine_abcdefghijklmnopqrstuvwxyz0123456789"
	l.Log(Entry{
		Action:   "daemon_task",
		Category: "chat",
		Details:  "password=hunter2 token=" + rawKey + " Authorization: Bearer should-not-store " + strings.Repeat("x", maxAuditDetailsLen),
	})

	entries, err := l.Query(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one audit entry, got %d", len(entries))
	}
	got := entries[0].Details
	for _, leak := range []string{"hunter2", rawKey, "should-not-store"} {
		if strings.Contains(got, leak) {
			t.Fatalf("audit details leaked %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("expected long details to be truncated, got len=%d", len(got))
	}
}
