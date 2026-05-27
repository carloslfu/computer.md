// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

// testDB opens a fresh encrypted SQLite database in the test's temp dir.
// Each test gets its own DB so triggers from one test can't contaminate
// another.
func testDB(t *testing.T) *persistence.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	db, err := persistence.Open(dbPath, "test-key")
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestAuditLogIsAppendOnly verifies the SQL trigger defended in
// persistence/db.go. The contract: INSERT works, UPDATE and DELETE
// fail with the trigger's RAISE(ABORT) message. This is the
// integrity-of-history guarantee the rest of the system depends on.
func TestAuditLogIsAppendOnly(t *testing.T) {
	db := testDB(t)
	logger := NewLogger(db)

	logger.Log(Entry{
		Action:   "test_action",
		Category: "guardrail",
		Details:  "original details",
	})

	entries, err := logger.Query(10, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after Log, got %d", len(entries))
	}
	id := entries[0].ID

	// UPDATE should be rejected by the trigger.
	_, err = db.Conn().Exec(`UPDATE audit_log SET details = ? WHERE id = ?`, "rewritten details", id)
	if err == nil {
		t.Fatal("UPDATE on audit_log succeeded — append-only trigger is missing or broken")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE error doesn't mention append-only, got: %v", err)
	}

	// DELETE should be rejected by the trigger.
	_, err = db.Conn().Exec(`DELETE FROM audit_log WHERE id = ?`, id)
	if err == nil {
		t.Fatal("DELETE on audit_log succeeded — append-only trigger is missing or broken")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE error doesn't mention append-only, got: %v", err)
	}

	// Confirm the entry still has its original details after the rejected ops.
	after, err := logger.Query(10, 0)
	if err != nil {
		t.Fatalf("Query after rejected ops: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("entry count changed after rejected ops: got %d", len(after))
	}
	if after[0].Details != "original details" {
		t.Errorf("entry details mutated despite rejected UPDATE: %q", after[0].Details)
	}

	// Sanity: subsequent INSERTs still work — the trigger is per-row,
	// not a table lock.
	logger.Log(Entry{Action: "test_action_2", Category: "guardrail"})
	after2, _ := logger.Query(10, 0)
	if len(after2) != 2 {
		t.Errorf("INSERT after rejected ops should still work; got %d entries", len(after2))
	}
}
