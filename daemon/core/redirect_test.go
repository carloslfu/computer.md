// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// TestNotifyNewTaskInConversation_CrossConvExpiresApproval is the
// foreground-attention guarantee at the heart of the cross-chat fix.
//
// Setup: task A in conv-a issues a bash command that trips the guardrail
// confirm rule. Engine parks on an approval card (waiting_for_input).
//
// Trigger: a fresh task B is created in conv-b. The HTTP handler signals
// the engine via NotifyNewTaskInConversation.
//
// Expected: A's card is auto-expired (ExpiredReason mentions
// "redirected"), A wraps up via its second manager call, the engine loop
// frees, and B runs to completion.
func TestNotifyNewTaskInConversation_CrossConvExpiresApproval(t *testing.T) {
	broker := &recordingBroker{}
	engine, tasks := engineWithRealShell(t, broker)

	// Track which task we're answering for so the same mock can drive
	// both task A and task B. We key off the user-instruction content
	// embedded in the message stream.
	engine.manager = &mockManager{fn: func(ctx context.Context, _ string, msgs []managerclient.Message) (*managerclient.Response, error) {
		instruction := firstUserText(msgs)
		switch {
		case strings.Contains(instruction, "kill the process"):
			// Task A's first call: emit a kill command that triggers the
			// process-confirm guardrail. After that call the engine is
			// parked on an approval card; we won't be re-entered for A
			// until something resolves it (in this test, the cross-conv
			// redirect from task B).
			if !hasToolResults(msgs) {
				// First call: emit the tool_use that trips the guardrail.
				return &managerclient.Response{
					StopReason: "tool_use",
					Blocks: []managerclient.Block{
						{Type: "tool_use", ToolCall: &managerclient.ToolCall{
							ID:    "redir_kill",
							Name:  "bash",
							Input: map[string]string{"command": "kill 99999"},
						}},
					},
					ToolCalls: []managerclient.ToolCall{{
						ID: "redir_kill", Name: "bash",
						Input: map[string]string{"command": "kill 99999"},
					}},
					RawContent: []interface{}{},
				}, nil
			}
			// Task A's second call (after the redirect-expired card came
			// back as a tool_error): wrap up with a short message.
			return &managerclient.Response{
				StopReason:  "end_turn",
				TextContent: "Switching focus — abandoning the kill.",
			}, nil
		case strings.Contains(instruction, "say hi"):
			// Task B: just end_turn cleanly so we can observe it ran.
			return &managerclient.Response{
				StopReason:  "end_turn",
				TextContent: "hi",
			}, nil
		}
		return nil, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	taskA, err := tasks.CreateTask("conv-a", "kill the process please")
	if err != nil {
		t.Fatalf("CreateTask A: %v", err)
	}

	// Wait until A parks on its approval card.
	_ = pollTaskStatus(t, tasks, taskA.ID, TaskWaitingForInput, 3*time.Second)

	// Snapshot the approval message id so we can assert ExpiredAt later.
	approvalMsgID := findApprovalMessageID(t, tasks, "conv-a")

	// Now create task B in a DIFFERENT conversation. The daemon's
	// handleTask calls NotifyNewTaskInConversation right after CreateTask;
	// the test simulates that call sequence by invoking the method directly.
	taskB, err := tasks.CreateTask("conv-b", "say hi")
	if err != nil {
		t.Fatalf("CreateTask B: %v", err)
	}
	engine.NotifyNewTaskInConversation("conv-b")

	// Task A should terminate as Cancelled — the redirect arm cancels
	// the task ctx directly so the engine loop frees up without waiting
	// for the manager to honour a "withdraw" prompt (which it sometimes
	// reinterprets as "try a different path", pinning the engine to the
	// abandoned conversation).
	_ = pollTaskStatus(t, tasks, taskA.ID, TaskCancelled, 5*time.Second)

	// Task B should run end-to-end now that the loop is free.
	_ = pollTaskStatus(t, tasks, taskB.ID, TaskCompleted, 5*time.Second)

	// Approval card on A must be expired with the redirect reason.
	if approvalMsgID == "" {
		t.Fatal("no approval message id captured from conv-a")
	}
	msgs, _ := tasks.GetMessages("conv-a")
	var card ApprovalPayload
	for _, m := range msgs {
		if m.ID == approvalMsgID {
			a, ok := DecodeApprovalJSON(m.Content)
			if !ok {
				t.Fatalf("approval message content did not decode as ApprovalPayload: %q", m.Content)
			}
			card = a
			break
		}
	}
	if card.ExpiredAt == "" {
		t.Fatalf("approval card was not marked expired; payload=%+v", card)
	}
	if !strings.Contains(strings.ToLower(card.ExpiredReason), "redirect") {
		t.Fatalf("expected ExpiredReason to mention redirect, got %q", card.ExpiredReason)
	}

	// Broker should have fired task:approval_expired with redirect reason.
	if !brokerHasReason(broker, "task:approval_expired", "redirected") {
		t.Fatalf("expected task:approval_expired with reason=redirected; events: %v", broker.Types())
	}

	// Audit should record the redirect for the guardrail category.
	entries, _ := engine.audit.QueryByTask(taskA.ID)
	if !hasAction(entries, "action_confirmation_redirected") {
		t.Fatalf("expected audit action_confirmation_redirected on task A; have: %v", actionNames(entries))
	}

	// The redirect arm posts a parting assistant message into conv-a so
	// the customer sees what happened if they return to that chat. The
	// generic "Task stopped" system_notice alone would be too thin —
	// they didn't stop it, they redirected.
	convMsgs, _ := tasks.GetMessages("conv-a")
	var foundParting bool
	for _, m := range convMsgs {
		if m.Role == "assistant" && strings.Contains(strings.ToLower(m.Content), "new conversation") {
			foundParting = true
			break
		}
	}
	if !foundParting {
		t.Fatalf("expected a parting assistant message in conv-a explaining the redirect; messages: %+v", convMsgs)
	}
}

// TestNotifyNewTaskInConversation_SameConvIsNoOp verifies the same-conv
// path leaves the open card alone. The user is plausibly still working
// inside this very conversation (e.g. realised they need to type some
// context before filling the card); we never auto-expire on same-conv
// activity.
func TestNotifyNewTaskInConversation_SameConvIsNoOp(t *testing.T) {
	broker := &recordingBroker{}
	engine, tasks := engineWithRealShell(t, broker)

	engine.manager = &mockManager{fn: func(ctx context.Context, _ string, msgs []managerclient.Message) (*managerclient.Response, error) {
		if !hasToolResults(msgs) {
			return &managerclient.Response{
				StopReason: "tool_use",
				Blocks: []managerclient.Block{
					{Type: "tool_use", ToolCall: &managerclient.ToolCall{
						ID:    "same_kill",
						Name:  "bash",
						Input: map[string]string{"command": "kill 99999"},
					}},
				},
				ToolCalls: []managerclient.ToolCall{{
					ID: "same_kill", Name: "bash",
					Input: map[string]string{"command": "kill 99999"},
				}},
				RawContent: []interface{}{},
			}, nil
		}
		return &managerclient.Response{StopReason: "end_turn", TextContent: "done"}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	taskA, _ := tasks.CreateTask("conv-same", "kill the process please")
	_ = pollTaskStatus(t, tasks, taskA.ID, TaskWaitingForInput, 3*time.Second)

	// Same-conv notification: must NOT expire the card.
	engine.NotifyNewTaskInConversation("conv-same")

	// Give the engine a moment in case it would (incorrectly) react.
	time.Sleep(150 * time.Millisecond)

	fresh, _ := tasks.GetTask(taskA.ID)
	if fresh.Status != TaskWaitingForInput {
		t.Fatalf("same-conv notify must not advance status; got %s", fresh.Status)
	}
	approvalMsgID := findApprovalMessageID(t, tasks, "conv-same")
	msgs, _ := tasks.GetMessages("conv-same")
	for _, m := range msgs {
		if m.ID == approvalMsgID {
			a, _ := DecodeApprovalJSON(m.Content)
			if a.ExpiredAt != "" {
				t.Fatalf("same-conv notify must not expire the card; got ExpiredAt=%q", a.ExpiredAt)
			}
			break
		}
	}

	// Now clean up so the test doesn't hang the goroutine.
	if err := engine.SubmitInput(taskA.ID, "no"); err != nil {
		t.Fatalf("SubmitInput cleanup: %v", err)
	}
	_ = pollTaskStatus(t, tasks, taskA.ID, TaskCompleted, 3*time.Second)
}

// TestNotifyNewTaskInConversation_NoActiveWaitIsNoOp verifies that
// calling Notify when the engine is idle or running normally (no card
// open) does nothing observable. This is the common case — every
// CreateTask calls Notify; only the rare parked-card-in-other-conv case
// should react.
func TestNotifyNewTaskInConversation_NoActiveWaitIsNoOp(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	var calls atomic.Int32
	engine := testEngine(t, db, &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		calls.Add(1)
		return &managerclient.Response{StopReason: "end_turn", TextContent: "ok"}, nil
	}})
	engine.tasks = tasks

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// Engine is idle. Notify is a no-op.
	engine.NotifyNewTaskInConversation("conv-anything")

	taskA, _ := tasks.CreateTask("conv-x", "a")
	// Notify in a different conv while task A is running normally (not
	// parked). Must NOT cancel A — running work is sacred; only
	// waiting_for_input gets the redirect treatment.
	engine.NotifyNewTaskInConversation("conv-y")
	_ = pollTaskStatus(t, tasks, taskA.ID, TaskCompleted, 3*time.Second)
}

// --- helpers ---

// messageContentString flattens a managerclient.Message's Content
// (which is interface{} to accommodate both plain text and structured
// content blocks) into a lowercased searchable string for the mock
// manager's routing logic.
func messageContentString(m managerclient.Message) string {
	switch c := m.Content.(type) {
	case string:
		return strings.ToLower(c)
	case nil:
		return ""
	}
	blob, err := json.Marshal(m.Content)
	if err != nil {
		return ""
	}
	return strings.ToLower(string(blob))
}

// firstUserText returns the lower-cased content of the first user
// message in the slice — i.e. the task instruction. The agent loop later
// appends tool_result messages with role=user, so the LAST user message
// is no longer the instruction once the loop has gone around once. Mock
// managers route on the instruction, so they want the first.
func firstUserText(msgs []managerclient.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return messageContentString(m)
		}
	}
	return ""
}

// hasToolResults reports whether the conversation already contains a
// user message carrying ToolResults — i.e. the agent loop has come back
// around with the result of a previous tool_use. Mock managers use this
// to branch on "first call vs. follow-up after tool_result." Robust to
// RawContent vs Content shape; doesn't depend on the assistant message
// content surface (which the engine fills via RawContent, not Content).
func hasToolResults(msgs []managerclient.Message) bool {
	for _, m := range msgs {
		if len(m.ToolResults) > 0 {
			return true
		}
	}
	return false
}

func findApprovalMessageID(t *testing.T, tasks *TaskStore, convID string) string {
	t.Helper()
	msgs, err := tasks.GetMessages(convID)
	if err != nil {
		t.Fatalf("GetMessages(%s): %v", convID, err)
	}
	for _, m := range msgs {
		if m.Type == "approval" {
			return m.ID
		}
	}
	return ""
}

func brokerHasReason(b *recordingBroker, eventType, reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.events {
		if e.Type != eventType {
			continue
		}
		blob, err := json.Marshal(e.Data)
		if err != nil {
			continue
		}
		if strings.Contains(string(blob), reason) {
			return true
		}
	}
	return false
}
