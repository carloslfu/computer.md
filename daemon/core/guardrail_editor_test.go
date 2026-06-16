// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/guardrails"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// TestGuardrailCommand_EditorIncludesPath is the unit-level guard for the
// editor-tool guardrail bypass: ToolCall.InputString() returns only the verb
// for the editor tool family (the path lives in Input["path"]), so the path
// would never reach the path-matching policies. guardrailCommand must splice
// the path into the command string.
func TestGuardrailCommand_EditorIncludesPath(t *testing.T) {
	cases := []struct {
		name string
		tc   managerclient.ToolCall
		want string
	}{
		{
			name: "editor create includes path",
			tc: managerclient.ToolCall{
				Name:  "str_replace_based_edit_tool",
				Input: map[string]string{"command": "create", "path": "/home/vibecraft/.ssh/authorized_keys", "file_text": "ssh-rsa AAAA..."},
			},
			want: "create /home/vibecraft/.ssh/authorized_keys",
		},
		{
			name: "editor view includes path",
			tc: managerclient.ToolCall{
				Name:  "text_editor",
				Input: map[string]string{"command": "view", "path": "/etc/ssh/sshd_config"},
			},
			want: "view /etc/ssh/sshd_config",
		},
		{
			name: "bash unchanged (still the command)",
			tc: managerclient.ToolCall{
				Name:  "bash",
				Input: map[string]string{"command": "ls -la /tmp"},
			},
			want: "ls -la /tmp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := guardrailCommand(c.tc); got != c.want {
				t.Fatalf("guardrailCommand = %q, want %q", got, c.want)
			}
		})
	}
}

// TestEditorGuardrail_PathReachesPolicies asserts the real guardrail engine
// (DefaultPolicies) reaches a Confirm/Block decision for editor actions on
// sensitive paths, when fed the command string the engine actually builds.
//
// Against the buggy engine (Command: tc.InputString()), the command would be
// just "create" / "view" and every one of these returns Allow — the entire
// sensitive-file + credential guardrail was bypassed for the editor surface.
func TestEditorGuardrail_PathReachesPolicies(t *testing.T) {
	db := testDB(t)
	ge := guardrails.NewEngine(db)

	cases := []struct {
		name string
		tc   managerclient.ToolCall
		want guardrails.ActionType
	}{
		{
			name: "create on ~/.ssh/authorized_keys confirms (SSH backdoor)",
			tc: managerclient.ToolCall{
				Name:  "str_replace_based_edit_tool",
				Input: map[string]string{"command": "create", "path": "/home/vibecraft/.ssh/authorized_keys", "file_text": "x"},
			},
			want: guardrails.Confirm,
		},
		{
			// SSH private keys hard-Block, not Confirm: a Confirm is eligible
			// for the AI auto-review pass (which treats read-only filesystem
			// inspection as auto-approvable) and the key could be auto-approved
			// and returned to the manager unmasked. Block short-circuits before
			// the classifier and before execution. See guardrails.credentialFilePatterns.
			// Block still proves the editor path reached the policies — the
			// point of this test — same as the canonical-editor coverage in
			// guardrails/policies_test.go.
			name: "view on ~/.ssh/id_rsa blocks (private key read)",
			tc: managerclient.ToolCall{
				Name:  "text_editor",
				Input: map[string]string{"command": "view", "path": "/home/vibecraft/.ssh/id_rsa"},
			},
			want: guardrails.Block,
		},
		{
			name: "view on /etc/ssh/sshd_config confirms",
			tc: managerclient.ToolCall{
				Name:  "str_replace_editor",
				Input: map[string]string{"command": "view", "path": "/etc/ssh/sshd_config"},
			},
			want: guardrails.Confirm,
		},
		{
			name: "create on vault.enc is blocked",
			tc: managerclient.ToolCall{
				Name:  "str_replace_based_edit_tool",
				Input: map[string]string{"command": "create", "path": "/var/lib/vibecraft/vault.enc", "file_text": "x"},
			},
			want: guardrails.Block,
		},
		{
			name: "create over daemon OpenAI key is blocked",
			tc: managerclient.ToolCall{
				Name:  "str_replace_based_edit_tool",
				Input: map[string]string{"command": "create", "path": "/etc/vibecraft/openai.key", "file_text": "x"},
			},
			want: guardrails.Block,
		},
		{
			name: "create on an ordinary workspace file is allowed",
			tc: managerclient.ToolCall{
				Name:  "str_replace_based_edit_tool",
				Input: map[string]string{"command": "create", "path": "/home/vibecraft/notes.md", "file_text": "hi"},
			},
			want: guardrails.Allow,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := ge.Evaluate(guardrails.Action{Type: c.tc.Name, Command: guardrailCommand(c.tc)})
			if d.Action != c.want {
				t.Fatalf("decision for %s on path: got %s (rule=%s), want %s", c.tc.Name, d.Action, d.Rule, c.want)
			}
		})
	}
}

// TestEditorGuardrail_EndToEndConfirm exercises the full runToolBlock path:
// the manager issues an editor `create` on ~/.ssh/authorized_keys, and the
// engine must surface an approval card (task → waiting_for_input) instead of
// silently writing the file. This is the real-behavior anchor for the
// bypass — against the buggy engine the editor action returned Allow, the
// file was created, and the task ran to completion without ever pausing.
func TestEditorGuardrail_EndToEndConfirm(t *testing.T) {
	broker := &recordingBroker{}
	engine, tasks := engineWithRealShell(t, broker)

	const sensitivePath = "/home/vibecraft/.ssh/authorized_keys"

	callCount := 0
	engine.manager = &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		callCount++
		if callCount == 1 {
			tc := managerclient.ToolCall{
				ID:    "toolu_editor_ssh",
				Name:  "str_replace_based_edit_tool",
				Input: map[string]string{"command": "create", "path": sensitivePath, "file_text": "ssh-rsa AAAA backdoor"},
			}
			return &managerclient.Response{
				StopReason: "tool_use",
				Blocks: []managerclient.Block{
					{Type: "tool_use", ToolCall: &tc},
				},
				ToolCalls:  []managerclient.ToolCall{tc},
				RawContent: []interface{}{},
			}, nil
		}
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "Understood, not writing that.",
		}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-editor-ssh", "add my ssh key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// The action must pause for approval. Against the buggy engine this
	// never happens — the editor write returns Allow and the task runs to
	// completion without ever pausing. Reaching waiting_for_input is the
	// core proof the editor tool's path now reaches the guardrail policies.
	_ = pollTaskStatus(t, tasks, task.ID, TaskWaitingForInput, 3*time.Second)

	// task:waiting must have been broadcast — the engine surfaced an
	// approval card for an editor action it previously executed silently.
	if !broker.Has("task:waiting") {
		t.Fatalf("no task:waiting event for editor write to a sensitive path; events: %v", broker.Types())
	}

	// The audit Details for the confirmation must carry the target PATH, not
	// just the verb, so an operator auditing a partial-write / persistence
	// incident can see which file was guarded. Against the buggy engine the
	// Details read "text_editor: create" with no path at all.
	entries, err := engine.audit.QueryByTask(task.ID)
	if err != nil {
		t.Fatalf("QueryByTask: %v", err)
	}
	var sawPathInAudit bool
	for _, e := range entries {
		if e.Action == "action_confirmation_requested" && strings.Contains(e.Details, ".ssh/authorized_keys") {
			sawPathInAudit = true
		}
	}
	if !sawPathInAudit {
		t.Fatalf("expected action_confirmation_requested audit entry containing the target path; entries=%v", actionNames(entries))
	}

	// Deny so the file is never written, and the task winds down cleanly.
	if err := engine.SubmitInput(task.ID, "no"); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	final := pollTaskStatus(t, tasks, task.ID, TaskCompleted, 3*time.Second)
	if final.Result == nil || !strings.Contains(*final.Result, "not writing") {
		t.Fatalf("expected completion after denial, got: %v", final.Result)
	}
}
