// SPDX-License-Identifier: Apache-2.0

package core

import (
	"path/filepath"
	"testing"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

func newTestTaskStore(t *testing.T) *TaskStore {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "tasks.db"), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewTaskStore(db)
}

func TestTaskStoreTerminalTransitionsAreMonotonic(t *testing.T) {
	tasks := newTestTaskStore(t)
	task, err := tasks.CreateTask("conv-1", "do the work")
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.UpdateStatus(task.ID, TaskCancelled); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetResult(task.ID, "late success"); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetError(task.ID, "late failure"); err != nil {
		t.Fatal(err)
	}
	if err := tasks.UpdateStatus(task.ID, TaskRunning); err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskCancelled {
		t.Fatalf("terminal cancelled task was overwritten: %+v", got)
	}
	if got.Result != nil {
		t.Fatalf("late result should not be stored on cancelled task: %+v", got.Result)
	}
	if got.ErrorMessage != nil {
		t.Fatalf("late error should not be stored on cancelled task: %+v", got.ErrorMessage)
	}
}

func TestClaimNextQueuedSkipsCancelledRows(t *testing.T) {
	tasks := newTestTaskStore(t)
	first, err := tasks.CreateTask("conv-1", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := tasks.CreateTask("conv-1", "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.UpdateStatus(first.ID, TaskCancelled); err != nil {
		t.Fatal(err)
	}

	claimed, err := tasks.ClaimNextQueued()
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		t.Fatal("expected second queued task to be claimed")
	}
	if claimed.ID != second.ID || claimed.Status != TaskRunning {
		t.Fatalf("claimed %+v, want second task running", claimed)
	}
	freshFirst, err := tasks.GetTask(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if freshFirst.Status != TaskCancelled {
		t.Fatalf("cancelled first task changed status: %+v", freshFirst)
	}
}
