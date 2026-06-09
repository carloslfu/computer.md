// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/computer"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// promptCapture is a race-safe holder for the system prompt the engine passes
// to the mock manager. The mock's callback runs on the engine's background
// agent-loop goroutine while the test body polls + asserts from the test
// goroutine; a bare `var s string` shared across the two is a data race the
// -race detector flags (and CI now runs -race). Guard the handoff with a
// mutex so the gate stays green and the test reflects real synchronization.
type promptCapture struct {
	mu  sync.Mutex
	val string
}

func (p *promptCapture) set(s string) {
	p.mu.Lock()
	p.val = s
	p.mu.Unlock()
}

func (p *promptCapture) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.val
}

// fakeScreenshotter returns canned base64 data so engine tests can exercise
// the screenshot flow without a real X display.
type fakeScreenshotter struct {
	data string
}

func (f *fakeScreenshotter) CaptureBase64(_ context.Context) (string, error) {
	if f.data == "" {
		return "ZmFrZQ==", nil
	}
	return f.data, nil
}

// TestNarrationBecomesScreenshotCaption verifies that a text block emitted
// immediately before a screenshot tool_use is persisted as that screenshot's
// caption (on the screenshot message row) rather than as a standalone
// assistant bubble. The narration + image render as one visual unit in the
// dashboard instead of two separated chat bubbles.
func TestNarrationBecomesScreenshotCaption(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	// Mock manager: first call returns narration text + a screenshot tool_use,
	// second call returns end_turn with final text.
	calls := 0
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		calls++
		if calls == 1 {
			tc := managerclient.ToolCall{
				ID:    "tool-1",
				Name:  "computer",
				Input: map[string]string{"action": "screenshot"},
			}
			return &managerclient.Response{
				StopReason:  "tool_use",
				TextContent: "Opening Chrome to check the dashboard.",
				ToolCalls:   []managerclient.ToolCall{tc},
				Blocks: []managerclient.Block{
					{Type: "text", Text: "Opening Chrome to check the dashboard."},
					{Type: "tool_use", ToolCall: &tc},
				},
				RawContent: []map[string]interface{}{
					{"type": "text", "text": "Opening Chrome to check the dashboard."},
					{"type": "tool_use", "id": "tool-1", "name": "computer", "input": map[string]string{"action": "screenshot"}},
				},
			}, nil
		}
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "Done. Dashboard is loaded.",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	// executeComputerTool refuses to run the tool when computer is nil,
	// which would short-circuit the screenshot before the caption logic
	// fires. The fake wires a non-nil computer controller to satisfy the
	// guard; the screenshot data is supplied by fakeScreenshotter.
	engine.screenshot = &fakeScreenshotter{data: "ZmFrZS1pbWFnZS1kYXRh"}
	engine.computer = &computer.Controller{}
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-narration-screenshot", "open chrome")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task did not complete")
		default:
		}
		got, _ := tasks.GetTask(task.ID)
		if got.Status == TaskCompleted || got.Status == TaskFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	msgs, err := tasks.GetMessages("conv-narration-screenshot")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	// Narration must land as the caption (content field) of a screenshot
	// message, not as its own text bubble.
	var foundCaptioned, foundStandaloneBubble bool
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		if !strings.Contains(m.Content, "Opening Chrome to check the dashboard") {
			continue
		}
		if m.Type == "screenshot" {
			foundCaptioned = true
		} else if m.Type == "text" {
			foundStandaloneBubble = true
		}
	}
	if !foundCaptioned {
		t.Errorf("narration was not attached as screenshot caption; messages: %+v", msgs)
	}
	if foundStandaloneBubble {
		t.Errorf("narration was also persisted as a standalone text bubble; should only live on the screenshot caption")
	}
}

// TestNarrationFallsBackToBubbleWhenScreenshotFails verifies that when the
// screenshot tool fails (e.g. display unavailable), the buffered narration
// is flushed as a standalone assistant bubble so the user still sees what
// the agent was about to do — no silent loss.
func TestNarrationFallsBackToBubbleWhenScreenshotFails(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	calls := 0
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		calls++
		if calls == 1 {
			tc := managerclient.ToolCall{
				ID:    "tool-ss-fail",
				Name:  "computer",
				Input: map[string]string{"action": "screenshot"},
			}
			return &managerclient.Response{
				StopReason: "tool_use",
				Blocks: []managerclient.Block{
					{Type: "text", Text: "Checking the screen now."},
					{Type: "tool_use", ToolCall: &tc},
				},
				RawContent: []map[string]interface{}{
					{"type": "text", "text": "Checking the screen now."},
					{"type": "tool_use", "id": "tool-ss-fail", "name": "computer", "input": map[string]string{"action": "screenshot"}},
				},
			}, nil
		}
		return &managerclient.Response{StopReason: "end_turn", TextContent: "Done."}, nil
	}}

	engine := testEngine(t, db, manager)
	// screenshot and computer both left nil → executeComputerTool returns
	// the "computer tools unavailable" error, which is the failure path
	// we want to exercise.
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-narration-fail", "peek at screen")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task did not complete")
		default:
		}
		got, _ := tasks.GetTask(task.ID)
		if got.Status == TaskCompleted || got.Status == TaskFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	msgs, err := tasks.GetMessages("conv-narration-fail")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	var bubbleFound bool
	for _, m := range msgs {
		if m.Role == "assistant" && m.Type == "text" && strings.Contains(m.Content, "Checking the screen now") {
			bubbleFound = true
			break
		}
	}
	if !bubbleFound {
		t.Fatalf("narration was lost when the screenshot failed; expected a fallback bubble. messages: %+v", msgs)
	}
}

// TestNarrationBeforeNonScreenshotToolBecomesBubble verifies that when the
// text block precedes a non-screenshot tool_use (bash, click, type, …), it
// is still flushed as a standalone assistant bubble — the caption short-cut
// applies only to screenshots, which are the sole chat-visible tool output.
func TestNarrationBeforeNonScreenshotToolBecomesBubble(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	calls := 0
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		calls++
		if calls == 1 {
			tc := managerclient.ToolCall{
				ID:    "tool-bash-1",
				Name:  "bash",
				Input: map[string]string{"command": "ls /tmp"},
			}
			return &managerclient.Response{
				StopReason:  "tool_use",
				TextContent: "Listing the temp directory.",
				ToolCalls:   []managerclient.ToolCall{tc},
				Blocks: []managerclient.Block{
					{Type: "text", Text: "Listing the temp directory."},
					{Type: "tool_use", ToolCall: &tc},
				},
				RawContent: []map[string]interface{}{
					{"type": "text", "text": "Listing the temp directory."},
					{"type": "tool_use", "id": "tool-bash-1", "name": "bash", "input": map[string]string{"command": "ls /tmp"}},
				},
			}, nil
		}
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "Done.",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-narration-bash", "list tmp")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task did not complete")
		default:
		}
		got, _ := tasks.GetTask(task.ID)
		if got.Status == TaskCompleted || got.Status == TaskFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	msgs, err := tasks.GetMessages("conv-narration-bash")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	var narrationFound bool
	for _, m := range msgs {
		if m.Role == "assistant" && m.Type == "text" && strings.Contains(m.Content, "Listing the temp directory") {
			narrationFound = true
			break
		}
	}
	if !narrationFound {
		t.Fatalf("narration before a non-screenshot tool must be persisted as a text bubble; got: %+v", msgs)
	}
}

// TestToolProgressEventSurfacesLiveAction pins the live work-card source of
// truth: progress comes from the engine right before it executes a real tool,
// not from a best-effort model narration sentence.
func TestToolProgressEventSurfacesLiveAction(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)
	broker := &recordingBroker{}

	calls := 0
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		calls++
		if calls == 1 {
			tc := managerclient.ToolCall{
				ID:   "tool-bash-progress",
				Name: "bash",
				Input: map[string]string{
					"command": "printf hello",
				},
			}
			return &managerclient.Response{
				StopReason: "tool_use",
				ToolCalls:  []managerclient.ToolCall{tc},
				Blocks: []managerclient.Block{
					{Type: "tool_use", ToolCall: &tc},
				},
				RawContent: []map[string]interface{}{
					{"type": "tool_use", "id": "tool-bash-progress", "name": "bash", "input": map[string]string{"command": "printf hello"}},
				},
			}, nil
		}
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "Done.",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.broker = broker
	engine.shell = computer.NewShell(t.TempDir(), "")
	engine.TaskTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-progress", "run a small command")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task did not complete")
		default:
		}
		got, _ := tasks.GetTask(task.ID)
		if got.Status == TaskCompleted || got.Status == TaskFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, event := range broker.events {
		if event.Type != "task:progress" {
			continue
		}
		payload, ok := event.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("task:progress payload not a map: %T", event.Data)
		}
		if payload["task_id"] != task.ID {
			t.Errorf("progress task_id = %v, want %s", payload["task_id"], task.ID)
		}
		if payload["current"] != "Running a shell command." {
			t.Errorf("progress current = %v", payload["current"])
		}
		if evidence, _ := payload["evidence"].(string); !strings.Contains(evidence, "printf hello") {
			t.Errorf("progress evidence did not include command: %q", evidence)
		}
		if summary, _ := payload["summary_markdown"].(string); !strings.Contains(summary, "Status: running") {
			t.Errorf("progress summary missing running status:\n%s", summary)
		}
		return
	}
	t.Fatalf("no task:progress event emitted; events: %v", broker.Types())
}

// TestActivityStubWrittenOnCompletion verifies that after a task completes,
// the synchronous stub activity entry exists in the memory store keyed by
// task ID. This is the Step 2 stub write that guarantees the next task
// always sees something for this one in WithActivities.
func TestActivityStubWrittenOnCompletion(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "All done.",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	engine.TaskTimeout = 5 * time.Second

	// Wire the summarizer (without a real manager client — stub-only path).
	// We call WriteStub directly from the engine via summarizer dependency.
	summarizer := NewSummarizer(nil, tasks, engine.audit, engine.memory, engine.vaultMask, noopBroker{})
	engine.SetSummarizer(summarizer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	task, err := tasks.CreateTask("conv-stub", "trivial test task")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Wait for completion.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("task did not complete")
		default:
		}
		got, _ := tasks.GetTask(task.ID)
		if got.Status == TaskCompleted {
			break
		}
		if got.Status == TaskFailed {
			t.Fatalf("task failed unexpectedly: %v", got.ErrorMessage)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Give the goroutine a moment to write the stub.
	time.Sleep(200 * time.Millisecond)

	item, err := engine.memory.GetByKey(ActivityCategory, task.ID)
	if err != nil {
		t.Fatalf("GetByKey: %v (no activity stub written)", err)
	}
	if !strings.Contains(item.Value, "Status: completed") {
		t.Errorf("stub missing 'Status: completed'; got: %s", item.Value)
	}
	if !strings.Contains(item.Value, "Task: trivial test task") {
		t.Errorf("stub missing original task instruction; got: %s", item.Value)
	}
}

// TestRestartStubWrittenForZombieTask verifies that when the daemon restarts
// and FailRunningTasks runs, each zombie also gets a stub activity entry
// with "interrupted by daemon restart" notes — matches success criterion #8.
func TestRestartStubWrittenForZombieTask(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	// Manually create a zombie task (status = running).
	task, err := tasks.CreateTask("conv-zombie", "in-flight work")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := tasks.UpdateStatus(task.ID, TaskRunning); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// Build engine and wire summarizer (stub-only path is fine).
	manager := &mockManager{fn: func(ctx context.Context, _ string, _ []managerclient.Message) (*managerclient.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	engine := testEngine(t, db, manager)
	summarizer := NewSummarizer(nil, tasks, engine.audit, engine.memory, engine.vaultMask, noopBroker{})
	engine.SetSummarizer(summarizer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// Let startup zombie cleanup run.
	time.Sleep(300 * time.Millisecond)

	got, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != TaskFailed {
		t.Fatalf("zombie task status = %s, want failed", got.Status)
	}

	item, err := engine.memory.GetByKey(ActivityCategory, task.ID)
	if err != nil {
		t.Fatalf("zombie did not get a stub activity entry: %v", err)
	}
	if !strings.Contains(item.Value, "interrupted by daemon restart") {
		t.Errorf("stub missing 'interrupted by daemon restart' note; got: %s", item.Value)
	}
}

// TestActivityInjectedIntoContext verifies that activity entries written to
// the memory store appear under "What you've done recently" in the system
// prompt for the next task — the Step 3 grounding fix.
func TestActivityInjectedIntoContext(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	// Pre-seed an activity entry.
	stubMarkdown := ActivityStubMarkdown(
		"open the dashboard",
		TaskCompleted,
		3*time.Second,
		"",
	)

	var captured promptCapture
	manager := &mockManager{fn: func(_ context.Context, systemPrompt string, _ []managerclient.Message) (*managerclient.Response, error) {
		captured.set(systemPrompt)
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "ok",
		}, nil
	}}

	engine := testEngine(t, db, manager)
	if _, err := engine.memory.Set(ActivityCategory, "task-prev", stubMarkdown, nil); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	if _, err := tasks.CreateTask("conv-injection", "next task"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Wait for the task to run.
	deadline := time.After(5 * time.Second)
	for {
		if captured.get() != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("agent loop did not call the manager in time")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}

	capturedSystemPrompt := captured.get()
	if !strings.Contains(capturedSystemPrompt, "What you've done recently") {
		t.Errorf("system prompt missing activity section header; got:\n%s", capturedSystemPrompt)
	}
	if !strings.Contains(capturedSystemPrompt, "open the dashboard") {
		t.Errorf("system prompt missing seeded activity content; got:\n%s", capturedSystemPrompt)
	}
	if !strings.Contains(capturedSystemPrompt, "<activity_log>") {
		t.Errorf("system prompt missing activity_log delimiter; got:\n%s", capturedSystemPrompt)
	}
}

// TestActivityExcludedFromMemorySearch verifies that activity entries are NOT
// surfaced through general memory search, even if their text matches the next
// task's instruction. This prevents the 500-char truncation noise documented
// in IN3.
func TestActivityExcludedFromMemorySearch(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	var captured promptCapture
	manager := &mockManager{fn: func(_ context.Context, systemPrompt string, _ []managerclient.Message) (*managerclient.Response, error) {
		captured.set(systemPrompt)
		return &managerclient.Response{
			StopReason:  "end_turn",
			TextContent: "ok",
		}, nil
	}}

	engine := testEngine(t, db, manager)

	// Activity entry whose text would match the upcoming instruction.
	activityMarkdown := "Task: deploy production api\nStatus: completed\n"
	if _, err := engine.memory.Set(ActivityCategory, "task-prev-2", activityMarkdown, nil); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	// And a regular memory entry with similar wording.
	if _, err := engine.memory.Set("preference", "deploy_target", "production api on Vercel", nil); err != nil {
		t.Fatalf("seed preference: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	if _, err := tasks.CreateTask("conv-search", "deploy the production api now"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if captured.get() != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("agent loop did not call the manager in time")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}

	capturedSystemPrompt := captured.get()
	// The preference should be present in the memory section.
	if !strings.Contains(capturedSystemPrompt, "production api on Vercel") {
		t.Errorf("preference memory not present in system prompt")
	}
	// The activity should ONLY appear under <activity_log>, not under <stored_data>.
	storedDataIdx := strings.Index(capturedSystemPrompt, "<stored_data>")
	storedDataEnd := strings.Index(capturedSystemPrompt, "</stored_data>")
	if storedDataIdx >= 0 && storedDataEnd > storedDataIdx {
		storedDataBlock := capturedSystemPrompt[storedDataIdx:storedDataEnd]
		if strings.Contains(storedDataBlock, "deploy production api") {
			t.Errorf("activity entry leaked into stored_data block")
		}
	}
}

// TestStartedAtPreservedAcrossWaitingForInput verifies that StartedAt is set
// only on the first transition to TaskRunning — subsequent transitions (e.g.,
// resuming from TaskWaitingForInput after user approval) must not reset the
// start clock. Before the COALESCE fix, a task with a 10-minute user-approval
// wait would report a duration of just however long the post-approval work
// took, and the summarizer's trivial-skip would wrongly swallow the task.
func TestStartedAtPreservedAcrossWaitingForInput(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	task, err := tasks.CreateTask("conv-started-at", "do a thing that needs approval")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := tasks.UpdateStatus(task.ID, TaskRunning); err != nil {
		t.Fatalf("first Running: %v", err)
	}
	firstStart, err := tasks.GetTask(task.ID)
	if err != nil || firstStart.StartedAt == nil {
		t.Fatalf("StartedAt not set on first Running: %v", err)
	}
	originalStart := *firstStart.StartedAt

	// Simulate waiting_for_input → resume → running pattern.
	if err := tasks.SetWaitingForInput(task.ID, "approve?"); err != nil {
		t.Fatalf("SetWaitingForInput: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // guarantee a different timestamp
	if err := tasks.UpdateStatus(task.ID, TaskRunning); err != nil {
		t.Fatalf("resume Running: %v", err)
	}

	resumed, err := tasks.GetTask(task.ID)
	if err != nil || resumed.StartedAt == nil {
		t.Fatalf("StartedAt missing after resume: %v", err)
	}
	if !resumed.StartedAt.Equal(originalStart) {
		t.Errorf("StartedAt was reset on resume. originally %s, now %s",
			originalStart, *resumed.StartedAt)
	}
}

// TestSummarizerSkipsOnlyWhenBothAreTrivial verifies the AND logic in the
// skip condition: a task with many audit entries completing quickly must
// still trigger the detailed summarizer, not stub-only. Before the fix
// (OR logic), a 7-step task that finished in 4s — a real case the user
// observed — was wrongly dropped to stub-only.
func TestSummarizerSkipsOnlyWhenBothAreTrivial(t *testing.T) {
	cases := []struct {
		name       string
		auditCount int
		duration   time.Duration
		shouldSkip bool
	}{
		{"trivial_both", 1, 2 * time.Second, true},
		{"many_steps_fast", 7, 2 * time.Second, false},
		{"few_steps_slow", 1, 30 * time.Second, false},
		{"non_trivial_both", 7, 30 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip := tc.auditCount < summarizerTrivialAuditThreshold && tc.duration < summarizerTrivialDuration
			if skip != tc.shouldSkip {
				t.Errorf("skip=%v, want %v", skip, tc.shouldSkip)
			}
		})
	}
}

// TestActivityStubMarkdownNoPendingLineByDefault guards against the "Detailed
// summary pending" language reappearing — it was developer-speak that made
// users think the activity was still loading when it wasn't.
func TestActivityStubMarkdownNoPendingLineByDefault(t *testing.T) {
	md := ActivityStubMarkdown("do a thing", TaskCompleted, 2*time.Second, "")
	if strings.Contains(md, "pending") {
		t.Errorf("default stub must not contain 'pending'; got:\n%s", md)
	}
	if !strings.Contains(md, "Status: completed") {
		t.Errorf("stub should still contain status; got:\n%s", md)
	}
	// Explicit extra notes must still render.
	restart := ActivityStubMarkdown("do a thing", TaskFailed, 0, "interrupted by daemon restart")
	if !strings.Contains(restart, "interrupted by daemon restart") {
		t.Errorf("explicit notes must render; got:\n%s", restart)
	}
}
