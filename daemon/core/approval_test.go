// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/computer"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// recordingBroker captures every SSE event emitted by the engine so
// tests can assert on them without standing up a full HTTP stream.
type recordingBroker struct {
	mu     sync.Mutex
	events []brokerEvent
}

type brokerEvent struct {
	Type string
	Data interface{}
}

func (r *recordingBroker) Emit(eventType string, data interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, brokerEvent{Type: eventType, Data: data})
}

func (r *recordingBroker) Types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	types := make([]string, len(r.events))
	for i, e := range r.events {
		types[i] = e.Type
	}
	return types
}

func (r *recordingBroker) Has(eventType string) bool {
	for _, t := range r.Types() {
		if t == eventType {
			return true
		}
	}
	return false
}

// engineWithRealShell is testEngine but wires a real Shell so the
// Confirm → approve path can actually execute a tool. The command it
// runs is chosen by the test to be trivially safe.
func engineWithRealShell(t *testing.T, broker EventBroker) (*Engine, *TaskStore) {
	t.Helper()
	db := testDB(t)
	tasks := NewTaskStore(db)
	base := testEngine(t, db, nil)
	// Swap in a recording broker if supplied, and a real shell.
	base.broker = broker
	base.shell = computer.NewShell(t.TempDir(), "")
	base.tasks = tasks
	return base, tasks
}

func pollTaskStatus(t *testing.T, tasks *TaskStore, id string, want TaskStatus, timeout time.Duration) *Task {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			got, _ := tasks.GetTask(id)
			if got != nil {
				t.Fatalf("task %s did not reach %s within %s (current status: %s)", id, want, timeout, got.Status)
			} else {
				t.Fatalf("task %s did not reach %s within %s (task missing)", id, want, timeout)
			}
			return nil
		default:
		}
		got, err := tasks.GetTask(id)
		if err == nil && got.Status == want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestConfirmPath_Denial exercises the full guardrail Confirm flow with
// a user denial. Verifies that:
//   - the approval prompt is persisted as a message of type "approval"
//   - the task transitions to waiting_for_input with the prompt in result
//   - task:waiting is emitted
//   - audit entries action_confirmation_requested + action_denied are written
//   - denying returns a ToolResult error to the manager, which can then complete
func TestConfirmPath_Denial(t *testing.T) {
	broker := &recordingBroker{}
	engine, tasks := engineWithRealShell(t, broker)

	// First manager call: issue a kill command that triggers ProcessPolicy Confirm.
	// Second call: after denial, produce a final text answer.
	callCount := 0
	engine.manager = &mockManager{fn: func(ctx context.Context, _ string, msgs []managerclient.Message) (*managerclient.Response, error) {
		callCount++
		switch callCount {
		case 1:
			return &managerclient.Response{
				StopReason: "tool_use",
				Blocks: []managerclient.Block{
					{Type: "tool_use", ToolCall: &managerclient.ToolCall{
						ID:    "toolu_test1",
						Name:  "bash",
						Input: map[string]string{"command": "kill 99999"},
					}},
				},
				ToolCalls: []managerclient.ToolCall{{
					ID: "toolu_test1", Name: "bash",
					Input: map[string]string{"command": "kill 99999"},
				}},
				RawContent: []interface{}{},
			}, nil
		default:
			// After denial, respond with final text.
			return &managerclient.Response{
				StopReason:  "end_turn",
				TextContent: "Understood, I will not run that.",
			}, nil
		}
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-approval-deny", "try killing some pid")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Wait until the task enters waiting_for_input.
	got := pollTaskStatus(t, tasks, task.ID, TaskWaitingForInput, 3*time.Second)
	if got.Result == nil || !strings.Contains(*got.Result, "kill 99999") {
		t.Fatalf("expected approval prompt with command, got result=%v", got.Result)
	}

	// Approval prompt must be persisted as a message of type "approval".
	msgs, err := tasks.GetMessages("conv-approval-deny")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	var foundApproval bool
	for _, m := range msgs {
		if m.Type == "approval" && strings.Contains(m.Content, "kill 99999") {
			foundApproval = true
			break
		}
	}
	if !foundApproval {
		t.Fatalf("no persisted message of type=approval with the command; messages: %+v", msgs)
	}

	// task:waiting must have been broadcast.
	if !broker.Has("task:waiting") {
		t.Fatalf("no task:waiting event; events: %v", broker.Types())
	}

	// Audit: action_confirmation_requested must be present.
	entries, err := engine.audit.QueryByTask(task.ID)
	if err != nil {
		t.Fatalf("QueryByTask: %v", err)
	}
	if !hasAction(entries, "action_confirmation_requested") {
		t.Fatalf("missing audit action_confirmation_requested; have: %v", actionNames(entries))
	}

	// Deny.
	if err := engine.SubmitInput(task.ID, "no"); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Task should complete (second manager call returns end_turn text).
	final := pollTaskStatus(t, tasks, task.ID, TaskCompleted, 3*time.Second)
	if final.Result == nil || !strings.Contains(*final.Result, "will not run") {
		t.Fatalf("expected completion with text mentioning denial, got: %v", final.Result)
	}

	// Audit: action_denied must be present. The "Denied" user message
	// should also be in the conversation.
	entries, _ = engine.audit.QueryByTask(task.ID)
	if !hasAction(entries, "action_denied") {
		t.Fatalf("missing audit action_denied; have: %v", actionNames(entries))
	}

	msgs, _ = tasks.GetMessages("conv-approval-deny")
	var foundDenied bool
	for _, m := range msgs {
		if m.Role == "user" && m.Content == "Denied" {
			foundDenied = true
			break
		}
	}
	if !foundDenied {
		t.Fatalf("expected user message 'Denied' after denial; messages: %+v", msgs)
	}
}

// TestConfirmPath_Approval verifies that approving a confirm-prompted
// command results in tool execution, an action_approved audit entry,
// and a "Approved" user message in the conversation.
func TestConfirmPath_Approval(t *testing.T) {
	broker := &recordingBroker{}
	engine, tasks := engineWithRealShell(t, broker)

	// `kill 0 || true` triggers ProcessPolicy Confirm (kill command) and
	// never actually errors out when approved, so the tool succeeds and
	// the loop can reach end_turn on the second manager call.
	callCount := 0
	engine.manager = &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		callCount++
		if callCount == 1 {
			return &managerclient.Response{
				StopReason: "tool_use",
				Blocks: []managerclient.Block{
					{Type: "tool_use", ToolCall: &managerclient.ToolCall{
						ID:    "toolu_test3",
						Name:  "bash",
						Input: map[string]string{"command": "kill 0 || true"},
					}},
				},
				ToolCalls: []managerclient.ToolCall{{
					ID: "toolu_test3", Name: "bash",
					Input: map[string]string{"command": "kill 0 || true"},
				}},
				RawContent: []interface{}{},
			}, nil
		}
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "All done.",
		}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-approval-yes", "do the confirm-requiring thing")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_ = pollTaskStatus(t, tasks, task.ID, TaskWaitingForInput, 3*time.Second)
	if err := engine.SubmitInput(task.ID, "yes"); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	final := pollTaskStatus(t, tasks, task.ID, TaskCompleted, 5*time.Second)
	if final.Result == nil || !strings.Contains(*final.Result, "All done") {
		t.Fatalf("unexpected result: %v", final.Result)
	}

	// Audit should have action_approved AND tool_executed.
	entries, _ := engine.audit.QueryByTask(task.ID)
	if !hasAction(entries, "action_approved") {
		t.Fatalf("missing action_approved; have: %v", actionNames(entries))
	}
	if !hasAction(entries, "tool_executed") {
		t.Fatalf("missing tool_executed (approved tool should run); have: %v", actionNames(entries))
	}

	// Conversation should include the "Approved" user message.
	msgs, _ := tasks.GetMessages("conv-approval-yes")
	var foundApproved bool
	for _, m := range msgs {
		if m.Role == "user" && m.Content == "Approved" {
			foundApproved = true
			break
		}
	}
	if !foundApproved {
		t.Fatalf("expected user message 'Approved'; messages: %+v", msgs)
	}
}

// TestEmptyManagerResponseIsFailure asserts that when the manager returns a
// final end_turn with empty text (and no tool calls), the task is
// marked failed rather than silently completing with an empty result.
func TestEmptyManagerResponseIsFailure(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	engine := testEngine(t, db, &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		return &managerclient.Response{StopReason: "end_turn", TextContent: ""}, nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-empty", "say nothing")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got := pollTaskStatus(t, tasks, task.ID, TaskFailed, 3*time.Second)
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "empty response") {
		t.Fatalf("expected empty-response error, got: %v", got.ErrorMessage)
	}
}

// TestListWaitingForInput verifies the new tasks-store method returns
// only tasks currently in waiting_for_input state — used by the SSE
// stream handler to replay pending approvals on connect.
func TestListWaitingForInput(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	// Three tasks: one waiting, one running, one completed.
	a, _ := tasks.CreateTask("conv-a", "a")
	b, _ := tasks.CreateTask("conv-b", "b")
	c, _ := tasks.CreateTask("conv-c", "c")

	tasks.UpdateStatus(a.ID, TaskRunning)
	tasks.SetWaitingForInput(a.ID, "approve a?")
	tasks.UpdateStatus(b.ID, TaskRunning)
	tasks.SetResult(c.ID, "done")

	waiting, err := tasks.ListWaitingForInput()
	if err != nil {
		t.Fatalf("ListWaitingForInput: %v", err)
	}
	if len(waiting) != 1 {
		t.Fatalf("expected 1 waiting task, got %d: %+v", len(waiting), waiting)
	}
	if waiting[0].ID != a.ID {
		t.Fatalf("wrong task: %s != %s", waiting[0].ID, a.ID)
	}
	if waiting[0].Result == nil || *waiting[0].Result != "approve a?" {
		t.Fatalf("expected result to hold the question, got: %v", waiting[0].Result)
	}
}

func hasAction(entries []audit.Entry, action string) bool {
	for _, e := range entries {
		if e.Action == action {
			return true
		}
	}
	return false
}

func actionNames(entries []audit.Entry) string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Action
	}
	return fmt.Sprintf("%v", names)
}
