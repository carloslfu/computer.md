// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// TestToolProgressMasksBeforeClipping is the regression guard for the
// progress-event secret-leak fix (daemon-core-3). The live progress narration
// path used to clip the tool input to a fixed length and *then* mask vault
// secrets. Because the masker only matches the full literal secret value, a
// secret straddling the clip boundary survived as an unmasked fragment in the
// SSE progress stream. Masking must happen BEFORE clipping so the secret is
// always replaced with its [NAME] marker before any truncation.
func TestToolProgressMasksBeforeClipping(t *testing.T) {
	db := testDB(t)
	engine := testEngine(t, db, &mockManager{fn: func(context.Context, string, []managerclient.Message) (*managerclient.Response, error) {
		return nil, context.Canceled
	}})

	// A secret long enough that, placed near the 180-char clip boundary of the
	// bash evidence string, the naive clip-then-mask order would slice it in
	// half. The unique, recognizable token lets us detect even a single-char
	// fragment leaking through.
	const secret = "SUPERSECRETVALUE_abcdefghijklmnopqrstuvwxyz_0123456789_THIS_MUST_NEVER_LEAK"
	if err := engine.vault.Set("LEAK_TOKEN", secret, "Leak token"); err != nil {
		t.Fatalf("vault set: %v", err)
	}

	// Build a bash command that pushes the secret across the 180-char clip
	// boundary: a long prefix of harmless padding, then the secret, then a
	// suffix. clippedOneLine clips at 180 chars (max-3 + "..."), so the secret
	// lands right on the cut line.
	prefix := strings.Repeat("a", 160)
	command := "echo " + prefix + " " + secret + " trailing-suffix"

	tc := managerclient.ToolCall{
		ID:    "tool-leak",
		Name:  "bash",
		Input: map[string]string{"command": command},
	}

	current, evidence := engine.toolProgressText(tc)
	if current == "" {
		t.Fatalf("expected a non-empty progress label")
	}

	// The full secret must be gone (it was masked before any clip).
	if strings.Contains(evidence, secret) {
		t.Fatalf("full secret leaked into progress evidence: %q", evidence)
	}
	// Critically: NO fragment of the secret may survive. The unique prefix of
	// the secret token is the tell-tale of a clip-then-mask fragment leak.
	for _, frag := range []string{"SUPERSECRETVALUE", "SUPERSE", "MUST_NEVER_LEAK"} {
		if strings.Contains(evidence, frag) {
			t.Fatalf("secret fragment %q leaked into progress evidence: %q", frag, evidence)
		}
	}
	// The masker's marker must be present — proof the value was recognized and
	// replaced (not merely clipped away by luck). The marker itself may be
	// truncated by the post-mask clip (e.g. "[LEAK_TOKEN..."), which is the
	// whole point: masking shrinks the secret to a short safe marker BEFORE
	// the clip, so only the harmless marker — never the secret — meets the
	// truncation boundary. We assert on the marker's leading prefix, which
	// survives the clip.
	if !strings.Contains(evidence, "[LEAK_TOKEN") {
		t.Fatalf("expected masked marker [LEAK_TOKEN in evidence; got: %q", evidence)
	}
}

// TestToolProgressMasksEditorAndDefaultBeforeClipping covers the two other
// progress paths that shared the clip-then-mask bug: the text-editor branch and
// the default tool branch. Both must mask before clipping for the same reason.
func TestToolProgressMasksEditorAndDefaultBeforeClipping(t *testing.T) {
	db := testDB(t)
	engine := testEngine(t, db, &mockManager{fn: func(context.Context, string, []managerclient.Message) (*managerclient.Response, error) {
		return nil, context.Canceled
	}})

	const secret = "EDITORSECRET_zyxwvutsrqponmlkjihgfedcba_9876543210_DO_NOT_LEAK_ME_EVER"
	if err := engine.vault.Set("EDITOR_TOKEN", secret, "Editor token"); err != nil {
		t.Fatalf("vault set: %v", err)
	}
	// Padding placed AFTER the secret so the secret sits near the front and its
	// [EDITOR_TOKEN] marker stays inside the 180-char clip window, while the
	// padding (not the secret) is what the clip truncates. With clip-then-mask,
	// the secret-bearing input clipped to 180 chars first would have leaked a
	// secret fragment; mask-then-clip replaces it with the marker first.
	suffix := strings.Repeat("z", 160)

	// Editor branch (str_replace_based_edit_tool): InputString returns the
	// "command" key if present, otherwise marshals the input map. We use a
	// non-command field so InputString marshals JSON containing the secret.
	editorCall := managerclient.ToolCall{
		ID:   "tool-editor",
		Name: "str_replace_based_edit_tool",
		Input: map[string]string{
			"new_text": secret + suffix,
		},
	}
	_, editorEvidence := engine.toolProgressText(editorCall)
	if strings.Contains(editorEvidence, secret) || strings.Contains(editorEvidence, "EDITORSECRET") {
		t.Fatalf("secret (or fragment) leaked into editor progress evidence: %q", editorEvidence)
	}
	if !strings.Contains(editorEvidence, "[EDITOR_TOKEN]") {
		t.Fatalf("expected masked marker in editor evidence; got: %q", editorEvidence)
	}

	// Default branch (unknown tool name): same expectation.
	defaultCall := managerclient.ToolCall{
		ID:   "tool-unknown",
		Name: "some_unknown_tool",
		Input: map[string]string{
			"arg": secret + suffix,
		},
	}
	_, defaultEvidence := engine.toolProgressText(defaultCall)
	if strings.Contains(defaultEvidence, secret) || strings.Contains(defaultEvidence, "EDITORSECRET") {
		t.Fatalf("secret (or fragment) leaked into default-tool progress evidence: %q", defaultEvidence)
	}
	if !strings.Contains(defaultEvidence, "[EDITOR_TOKEN]") {
		t.Fatalf("expected masked marker in default-tool evidence; got: %q", defaultEvidence)
	}
}

// TestTaskReachesTerminalStateWhenResultWriteFails is the regression guard for
// daemon-core-2: a successful agent loop whose result-persisting write errors
// must NOT strand the task in "running". The engine now falls back to failing
// the task (terminal state) and emits task:failed instead of task:completed.
//
// We force the SetResult write to fail by closing the DB during the final
// manager turn — after processTask has already transitioned the task to running
// (those writes succeed) but before the post-loop SetResult runs.
func TestTaskReachesTerminalStateWhenResultWriteFails(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	broker := &recordingBroker{}

	manager := &mockManager{fn: func(context.Context, string, []managerclient.Message) (*managerclient.Response, error) {
		// Close the DB as the side effect of producing the final response, so
		// the post-agentLoop result write fails while the earlier
		// running-transition writes already committed.
		_ = db.Close()
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "all done",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.broker = broker
	engine.TaskTimeout = 5 * time.Second

	task, err := tasks.CreateTask("conv-result-fail", "do a thing")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Drive the task directly (synchronous) so we don't race the engine's loop
	// goroutine against the closed DB. processTask transitions to running
	// (succeeds), runs the agent loop (mock closes the DB), then attempts the
	// result write (fails → fallback to failed).
	engine.processTask(context.Background(), task)

	// The authoritative assertion is the emitted lifecycle event: the task must
	// NOT report completed when the result write failed. (GetTask can't be used
	// here because the DB is intentionally closed.)
	if broker.Has("task:completed") {
		t.Fatalf("task emitted task:completed despite the result write failing; events: %v", broker.Types())
	}
	if !broker.Has("task:failed") {
		t.Fatalf("task did not reach a terminal failed state after result write failure; events: %v", broker.Types())
	}
}
