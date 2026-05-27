// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/guardrails"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/memory"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// --- Mock implementations ---

// mockManager is a controllable manager API mock for testing.
type mockManager struct {
	fn func(ctx context.Context, systemPrompt string, messages []managerclient.Message) (*managerclient.Response, error)
}

func (m *mockManager) SendComputerUse(ctx context.Context, systemPrompt string, messages []managerclient.Message) (*managerclient.Response, error) {
	return m.fn(ctx, systemPrompt, messages)
}

// noopBroker satisfies EventBroker but discards all events.
type noopBroker struct{}

func (noopBroker) Emit(string, interface{}) {}

// testDB creates a temporary encrypted SQLite database for testing.
func testDB(t *testing.T) *persistence.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := persistence.Open(dbPath, "test-key")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// testEngine creates a minimal Engine suitable for testing task lifecycle.
// It uses a mock manager client and real DB but nil for computer/screenshot/shell
// since tests that need those can set them separately.
func testEngine(t *testing.T, db *persistence.DB, manager ManagerAPI) *Engine {
	t.Helper()
	dir := t.TempDir()
	tasks := NewTaskStore(db)
	ge := guardrails.NewEngine(db)
	mem := memory.NewStore(db)

	vaultPath := filepath.Join(dir, "vault.enc")
	var vaultKey [32]byte
	copy(vaultKey[:], []byte("test-vault-key-0123456789abcdef"))
	v, err := vault.NewStore(vaultPath, vaultKey)
	if err != nil {
		t.Fatalf("vault.NewStore: %v", err)
	}

	vm := vault.NewMasker(v)
	al := audit.NewLogger(db)

	return NewEngine(tasks, manager, nil, nil, nil, ge, mem, v, vm, al, "test prompt", noopBroker{})
}

// --- Tests ---

func TestZombieTaskCleanupOnStart(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	// Create a task and manually set it to "running" (simulates a daemon crash).
	task, err := tasks.CreateTask("conv-1", "do something")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := tasks.UpdateStatus(task.ID, TaskRunning); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// Also create a waiting_for_input task.
	task2, err := tasks.CreateTask("conv-2", "waiting task")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := tasks.UpdateStatus(task2.ID, TaskRunning); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if err := tasks.SetWaitingForInput(task2.ID, "approve?"); err != nil {
		t.Fatalf("SetWaitingForInput: %v", err)
	}

	// Start engine — it should clean up zombies.
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	engine := testEngine(t, db, manager)

	ctx, cancel := context.WithCancel(context.Background())
	engine.Start(ctx)
	time.Sleep(200 * time.Millisecond) // Let cleanup run.
	cancel()

	// Verify both tasks are now failed.
	t1, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if t1.Status != TaskFailed {
		t.Errorf("zombie task status = %s, want %s", t1.Status, TaskFailed)
	}
	if t1.ErrorMessage == nil || *t1.ErrorMessage != "daemon restarted while task was running" {
		t.Errorf("zombie task error = %v, want 'daemon restarted while task was running'", t1.ErrorMessage)
	}

	t2, err := tasks.GetTask(task2.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if t2.Status != TaskFailed {
		t.Errorf("waiting task status = %s, want %s", t2.Status, TaskFailed)
	}
}

func TestTaskTimeoutFailsCleanly(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("timing-sensitive test may flake in CI")
	}

	db := testDB(t)
	tasks := NewTaskStore(db)

	// Mock manager that blocks until context is cancelled.
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 500 * time.Millisecond // Short timeout for testing.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// Submit a task.
	task, err := tasks.CreateTask("conv-timeout", "timeout test")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Wait for the task to time out.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task did not time out within 5 seconds")
		default:
		}

		got, err := tasks.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status == TaskFailed {
			if got.ErrorMessage == nil || *got.ErrorMessage == "" {
				t.Error("task failed but error message is empty")
			}
			return // Success.
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestPanicRecoveryInAgentLoop(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	// Mock manager that panics.
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		panic("test panic in manager API call")
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// Submit a task that will trigger the panic.
	task, err := tasks.CreateTask("conv-panic", "panic test")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Wait for it to be marked failed.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("panicking task was not marked failed within 5 seconds")
		default:
		}

		got, err := tasks.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status == TaskFailed {
			if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "panic") {
				t.Errorf("error message should mention panic, got: %v", got.ErrorMessage)
			}

			// Verify engine is still alive by submitting a second task.
			task2, err := tasks.CreateTask("conv-panic-2", "after panic")
			if err != nil {
				t.Fatalf("CreateTask after panic: %v", err)
			}

			// Give the engine time to pick up and fail the second task (it'll also panic).
			time.Sleep(500 * time.Millisecond)
			got2, _ := tasks.GetTask(task2.ID)
			if got2.Status == TaskQueued {
				// Engine might not have picked it up yet, wait more.
				time.Sleep(1 * time.Second)
				got2, _ = tasks.GetTask(task2.ID)
			}
			if got2.Status == TaskQueued {
				t.Error("engine did not pick up second task after panic — engine loop is dead")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestEngineProcessesAfterTimeout(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("timing-sensitive test may flake in CI")
	}

	db := testDB(t)
	tasks := NewTaskStore(db)

	callCount := 0
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		callCount++
		if callCount <= 1 {
			// First task: block until timeout.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		// Second task: return immediately with final text.
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "done",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// First task — will time out.
	task1, _ := tasks.CreateTask("conv-recovery-1", "will timeout")

	// Wait for first task to fail.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("first task did not time out")
		default:
		}
		got, _ := tasks.GetTask(task1.ID)
		if got.Status == TaskFailed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Second task — should complete successfully.
	task2, _ := tasks.CreateTask("conv-recovery-2", "should work")

	deadline = time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("second task did not complete after first timeout")
		default:
		}
		got, _ := tasks.GetTask(task2.ID)
		if got.Status == TaskCompleted {
			return // Success — engine recovered.
		}
		if got.Status == TaskFailed {
			t.Fatalf("second task failed unexpectedly: %v", got.ErrorMessage)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestAgentLoop_DropsTrailingAssistantFromConcurrentTask is the regression
// test for the v0.40.4 prefill-rejection fix.
//
// The production failure mode it pins down:
//
//	The manager API rejects any request whose `messages` array ends on
//	role=assistant (some models explicitly do not support
//	assistant prefill — they return `400 invalid_request_error: This
//	model does not support assistant message prefill. The conversation
//	must end with a user message.`). When that happens mid-conversation,
//	every subsequent task in the same conversation hits the same 400 and
//	the chat appears to loop "running 0s" → API error → "running 0s" →
//	API error until the user manually starts a fresh conversation.
//
// The construction race that produces a trailing assistant in the
// messages table — and therefore in the engine's apiMessages on first
// load — is:
//
//  1. Task A is running. Engine is in agentLoop persisting narration
//     via AddMessage(assistant, …) at line 1140 each turn.
//  2. The user (or a system) submits Task B while Task A is mid-loop.
//     CreateTask adds a user-role message for Task B at time T1.
//  3. Task A persists more assistant narration at T2 > T1. Then Task A
//     ends, persisting its final assistant result at T3 > T2.
//  4. Engine picks up Task B from the queue and calls agentLoop.
//     agentLoop's GetMessages returns messages ordered by created_at:
//     … user(A) … assistant(A.done) user(B) assistant(A.late) assistant(A.final)
//     Last role is "assistant" — the API rejects the request.
//
// This test reproduces the post-race conversation state directly,
// fires the engine on Task B, and asserts the FIRST API call shape:
// the last message MUST be role=user. The fix is in agentLoop, which
// strips trailing assistant turns from the initial conversation
// history before sending — they break the contract the API enforces
// and they were never part of "this task's" conversation in any
// logical sense (they're another task's late writes that happened to
// land after this task's instruction in the timeline).
func TestAgentLoop_DropsTrailingAssistantFromConcurrentTask(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	convID := "conv-race"

	// Seed: prior Task A ran successfully in this conversation. The
	// engine would have persisted its narration + final result; we
	// short-circuit by writing the final state directly and marking
	// the task complete so the engine doesn't try to run it.
	firstTask, err := tasks.CreateTask(convID, "do task A")
	if err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	if err := tasks.SetResult(firstTask.ID, "task A complete"); err != nil {
		t.Fatalf("SetResult A: %v", err)
	}
	if err := tasks.AddMessage(convID, "assistant", "task A complete"); err != nil {
		t.Fatalf("AddMessage A.done: %v", err)
	}

	// User submits Task B. CreateTask adds the user message for B.
	taskB, err := tasks.CreateTask(convID, "do task B")
	if err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}

	// THE RACE: Task A's late narration lands AFTER Task B's user
	// instruction in the messages table. In production this happens
	// when Task A is still mid-loop and persists more assistant turns
	// after the user already submitted Task B.
	if err := tasks.AddMessage(convID, "assistant", "task A late narration"); err != nil {
		t.Fatalf("AddMessage A.late: %v", err)
	}

	// Capture exactly what the engine sends to the manager API on the
	// FIRST call for Task B. Subsequent calls (if the agent loop iterates)
	// are in-memory-controlled by the engine itself and not what's under
	// test here.
	var firstCall []managerclient.Message
	var mu sync.Mutex
	var called int32
	manager := &mockManager{fn: func(ctx context.Context, _ string, msgs []managerclient.Message) (*managerclient.Response, error) {
		if atomic.AddInt32(&called, 1) == 1 {
			mu.Lock()
			firstCall = append(firstCall, msgs...)
			mu.Unlock()
		}
		// Short-circuit the loop: return end_turn with a non-empty
		// answer so processTask completes cleanly.
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "done",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// Wait for Task B to reach a terminal state.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Task B did not complete within 5 seconds")
		default:
		}
		got, gerr := tasks.GetTask(taskB.ID)
		if gerr != nil {
			t.Fatalf("GetTask B: %v", gerr)
		}
		if got.Status == TaskCompleted {
			break
		}
		if got.Status == TaskFailed {
			// A FAIL here is also a real failure mode worth diagnosing —
			// could be the original prefill bug if the test were running
			// against the real API. With the mock it should never fire.
			t.Fatalf("Task B failed unexpectedly: %v", got.ErrorMessage)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if firstCall == nil {
		t.Fatalf("mockManager was never called for Task B")
	}
	if len(firstCall) == 0 {
		t.Fatalf("first API call had an empty messages slice")
	}
	last := firstCall[len(firstCall)-1]
	if last.Role != "user" {
		// Print the full slice to aid debugging.
		t.Logf("messages slice (%d entries):", len(firstCall))
		for i, m := range firstCall {
			t.Logf("  [%d] role=%s content=%v", i, m.Role, m.Content)
		}
		t.Errorf("FIRST API call ends on role=%q; want %q.\n"+
			"The trailing assistant turn from concurrent Task A's late narration should be stripped from the initial history before the request is sent. "+
			"Otherwise the manager API rejects with 400 invalid_request_error: 'This model does not support assistant message prefill. The conversation must end with a user message.'",
			last.Role, "user")
	}
}

// TestAgentLoop_StripsMultipleTrailingAssistants is the "more than one
// late write" variant of the prefill regression — Task A could persist
// both a late narration AND its final result after Task B's user
// instruction landed. Both must be stripped, leaving the user
// instruction as the last message in the first API call.
func TestAgentLoop_StripsMultipleTrailingAssistants(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	convID := "conv-multi-trailing"

	priorTask, err := tasks.CreateTask(convID, "do task A")
	if err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	if err := tasks.SetResult(priorTask.ID, "task A done"); err != nil {
		t.Fatalf("SetResult: %v", err)
	}

	taskB, err := tasks.CreateTask(convID, "do task B")
	if err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}

	// Task A's TWO late writes land after user(B).
	if err := tasks.AddMessage(convID, "assistant", "task A late narration 1"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := tasks.AddMessage(convID, "assistant", "task A final result"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	var firstCall []managerclient.Message
	var mu sync.Mutex
	var called int32
	manager := &mockManager{fn: func(ctx context.Context, _ string, msgs []managerclient.Message) (*managerclient.Response, error) {
		if atomic.AddInt32(&called, 1) == 1 {
			mu.Lock()
			firstCall = append(firstCall, msgs...)
			mu.Unlock()
		}
		return &managerclient.Response{StopReason: "end_turn", TextContent: "done"}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Task B did not complete in 5s")
		default:
		}
		got, _ := tasks.GetTask(taskB.ID)
		if got.Status == TaskCompleted {
			break
		}
		if got.Status == TaskFailed {
			t.Fatalf("Task B failed: %v", got.ErrorMessage)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if firstCall == nil {
		t.Fatalf("mockManager never called")
	}
	last := firstCall[len(firstCall)-1]
	if last.Role != "user" {
		t.Errorf("last role = %q, want %q (both trailing assistants should be stripped)", last.Role, "user")
	}
	// And the user instruction should be Task B's, not Task A's.
	if last.Content != "do task B" {
		t.Errorf("last user content = %v, want %q (stripping went too far)", last.Content, "do task B")
	}
}

// TestAgentLoop_NoStripWhenAlreadyEndingOnUser is the no-op pin: the
// strip must NOT touch a history that already ends correctly on a
// user turn, because that would silently drop the actual current
// task's instruction and the agent would either error or respond to
// the wrong question.
func TestAgentLoop_NoStripWhenAlreadyEndingOnUser(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	convID := "conv-clean-tail"

	// Standard happy path: prior task completed, new task's user message
	// is the very last entry. Nothing should be stripped.
	priorTask, err := tasks.CreateTask(convID, "do task A")
	if err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}
	if err := tasks.SetResult(priorTask.ID, "task A done"); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	if err := tasks.AddMessage(convID, "assistant", "task A done"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	taskB, err := tasks.CreateTask(convID, "do task B")
	if err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}

	var firstCall []managerclient.Message
	var mu sync.Mutex
	var called int32
	manager := &mockManager{fn: func(ctx context.Context, _ string, msgs []managerclient.Message) (*managerclient.Response, error) {
		if atomic.AddInt32(&called, 1) == 1 {
			mu.Lock()
			firstCall = append(firstCall, msgs...)
			mu.Unlock()
		}
		return &managerclient.Response{StopReason: "end_turn", TextContent: "done"}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Task B did not complete in 5s")
		default:
		}
		got, _ := tasks.GetTask(taskB.ID)
		if got.Status == TaskCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if firstCall == nil {
		t.Fatalf("mockManager never called")
	}
	// History should have user(A), assistant(A done), user(B). No strip.
	if len(firstCall) != 3 {
		t.Logf("messages slice (%d entries):", len(firstCall))
		for i, m := range firstCall {
			t.Logf("  [%d] role=%s content=%v", i, m.Role, m.Content)
		}
		t.Errorf("expected 3 messages, got %d (strip should be a no-op here)", len(firstCall))
	}
	if firstCall[len(firstCall)-1].Role != "user" {
		t.Errorf("last role = %q, want %q", firstCall[len(firstCall)-1].Role, "user")
	}
	if firstCall[len(firstCall)-1].Content != "do task B" {
		t.Errorf("last user content = %v, want %q (the current task's instruction was wrongly dropped)",
			firstCall[len(firstCall)-1].Content, "do task B")
	}
}

// TestCreateTask_TaskAndMessageAtomicallyVisible pins the second half
// of the v0.40.4 fix: CreateTask must commit the task row AND the
// user message in a single transaction. Otherwise the engine's
// NextQueued (a Query on the same single SQLite connection) can
// interleave between them, see the task, dequeue it, and load
// messages BEFORE the user message is committed — leaving the
// agentLoop's first API request ending on whatever was previously
// the conversation's last persisted message (typically assistant).
//
// This test asserts the post-condition: when CreateTask returns
// successfully, BOTH the task and the message are visible. The
// regression we're guarding against is a refactor that splits them
// back into separate auto-committed Execs.
func TestCreateTask_TaskAndMessageAtomicallyVisible(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	task, err := tasks.CreateTask("conv-atomic", "do something")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Task row visible.
	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask immediately after CreateTask: %v", err)
	}
	if got.Status != TaskQueued {
		t.Errorf("status = %s, want %s", got.Status, TaskQueued)
	}

	// User message visible.
	msgs, err := tasks.GetMessages("conv-atomic")
	if err != nil {
		t.Fatalf("GetMessages immediately after CreateTask: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message immediately after CreateTask, got %d (atomic-write race regression)", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("first message role = %q, want %q", msgs[0].Role, "user")
	}
	if msgs[0].Content != "do something" {
		t.Errorf("first message content = %q, want %q", msgs[0].Content, "do something")
	}
}
