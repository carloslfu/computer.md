// SPDX-License-Identifier: Apache-2.0

package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/memory"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// testStores constructs the four dependencies WriteRunningStub needs
// (TaskStore, memory.Store, audit.Logger, vault.Masker) on top of a
// fresh in-temp encrypted DB. Mirrors the inline setup in testEngine
// but exposes them individually so tests can call summarizer methods
// directly without spinning up the full engine loop.
//
// The recordingBroker type used by these tests already exists in
// approval_test.go in this package — share it rather than redeclaring.
func testStores(t *testing.T, db *persistence.DB) (*TaskStore, *memory.Store, *audit.Logger, *vault.Masker) {
	t.Helper()
	dir := t.TempDir()
	tasks := NewTaskStore(db)
	mem := memory.NewStore(db)
	al := audit.NewLogger(db)

	vaultPath := filepath.Join(dir, "vault.enc")
	var vaultKey [32]byte
	copy(vaultKey[:], []byte("test-vault-key-0123456789abcdef"))
	v, err := vault.NewStore(vaultPath, vaultKey)
	if err != nil {
		t.Fatalf("vault.NewStore: %v", err)
	}
	return tasks, mem, al, vault.NewMasker(v)
}

// TestWriteRunningStubPersistsAndEmits is the core regression for the
// "delay before the task shows up in History" UX bug. Before this fix
// the activity row was only written at task completion, so a long
// task was invisible to History for its entire duration. The
// placeholder write must:
//  1. Persist a memory row with status=running and step_count=0
//  2. Emit a task:activity SSE event so live clients can render it
//     without polling
//  3. Be upsert-compatible — the same key gets overwritten by
//     WriteStub at task end (no duplicate rows)
func TestWriteRunningStubPersistsAndEmits(t *testing.T) {
	db := testDB(t)
	tasks, mem, audit, masker := testStores(t, db)

	broker := &recordingBroker{}
	summarizer := NewSummarizer(nil, tasks, audit, mem, masker, broker)

	task, err := tasks.CreateTask("conv-running", "research the bigger question")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := summarizer.WriteRunningStub(task); err != nil {
		t.Fatalf("WriteRunningStub: %v", err)
	}

	// Memory row should exist with running status.
	item, err := mem.GetByKey(ActivityCategory, task.ID)
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if !strings.Contains(item.Value, "Status: running") {
		t.Errorf("expected 'Status: running' in placeholder, got:\n%s", item.Value)
	}
	if !strings.Contains(item.Value, "research the bigger question") {
		t.Errorf("expected the original instruction in placeholder, got:\n%s", item.Value)
	}

	// SSE event should have fired with status=running.
	broker.mu.Lock()
	defer broker.mu.Unlock()
	var sawActivity bool
	for _, e := range broker.events {
		if e.Type != "task:activity" {
			continue
		}
		sawActivity = true
		data, ok := e.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("task:activity payload not a map: %T", e.Data)
		}
		if data["status"] != "running" {
			t.Errorf("expected status=running in SSE payload, got %v", data["status"])
		}
		if data["task_id"] != task.ID {
			t.Errorf("expected task_id=%s, got %v", task.ID, data["task_id"])
		}
		if data["step_count"] != 0 {
			t.Errorf("expected step_count=0 on placeholder, got %v", data["step_count"])
		}
	}
	if !sawActivity {
		t.Errorf("no task:activity event emitted; got %d events total", len(broker.events))
	}
}

// TestWriteRunningStubIsUpsertedByCompletionStub verifies that calling
// WriteRunningStub followed by WriteStub leaves exactly ONE row in
// memory (the completion stub wins, last-write-wins via UPSERT). This
// is critical: a bug here would cause the History view to show two
// rows per task (one Running, one Done), permanently.
func TestWriteRunningStubIsUpsertedByCompletionStub(t *testing.T) {
	db := testDB(t)
	tasks, mem, audit, masker := testStores(t, db)
	summarizer := NewSummarizer(nil, tasks, audit, mem, masker, &recordingBroker{})

	task, err := tasks.CreateTask("conv-upsert", "do something")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Placeholder at start.
	if err := summarizer.WriteRunningStub(task); err != nil {
		t.Fatalf("WriteRunningStub: %v", err)
	}

	// Mark completed and write the completion stub.
	now := time.Now()
	task.Status = TaskCompleted
	task.CompletedAt = &now
	if err := summarizer.WriteStub(task, ""); err != nil {
		t.Fatalf("WriteStub: %v", err)
	}

	// Exactly one row should exist for this task.
	items, err := mem.List(ActivityCategory)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	count := 0
	for _, it := range items {
		if it.Key == task.ID {
			count++
			if !strings.Contains(it.Value, "Status: completed") {
				t.Errorf("expected the completion stub to win, got:\n%s", it.Value)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 memory row for task %s, got %d", task.ID, count)
	}
}

// TestActivityStubMarkdownSupportsRunningStatus pins that
// ActivityStubMarkdown round-trips the TaskRunning status into the
// markdown. The frontend reads "Status: running" to render the
// spinner — change the casing or the field and the spinner
// disappears.
func TestActivityStubMarkdownSupportsRunningStatus(t *testing.T) {
	md := ActivityStubMarkdown("login to gmail", TaskRunning, 0, "")
	if !strings.Contains(md, "Status: running") {
		t.Errorf("expected 'Status: running' in markdown, got:\n%s", md)
	}
	if !strings.Contains(md, "login to gmail") {
		t.Errorf("expected the instruction text, got:\n%s", md)
	}
}

func TestActivityProgressMarkdownIncludesCurrentAndEvidence(t *testing.T) {
	md := ActivityProgressMarkdown(
		"open codex",
		3*time.Second,
		"Opening Codex in a terminal.",
		"bash: xterm -T worker-1 -e codex &",
	)
	for _, want := range []string{
		"Task: open codex",
		"Status: running",
		"Duration: 3s",
		"Current: Opening Codex in a terminal.",
		"Last activity: bash: xterm -T worker-1 -e codex &",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("progress markdown missing %q:\n%s", want, md)
		}
	}
}
