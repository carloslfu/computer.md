// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/memory"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// ActivityCategory is the memory category used for post-task activity notes.
const ActivityCategory = "activity"

// activitySummaryType is the Message.Type used for the parallel activity-summary
// message row written into the conversation's messages table. This is what makes
// the chat pill survive page reload — SSE is live-only, the message is durable.
const activitySummaryType = "activity_summary"

// summarizerModel is the manager model used for activity summarization. Launch
// keeps this on the same tested manager model as the main loop.
const summarizerModel = managerclient.DefaultModelID

// summarizerMaxTokens caps output tokens for the summarizer call.
const summarizerMaxTokens = 1200

// summarizerValueCharCap caps the stored summary markdown to prevent a single
// pathological task from bloating the system prompt on every future turn.
// ~4800 chars ≈ 1200 tokens.
const summarizerValueCharCap = 4800

// summarizerTrivialAuditThreshold is the minimum number of audit entries
// required to run the detailed summarizer. Below this, the stub is kept.
const summarizerTrivialAuditThreshold = 3

// summarizerTrivialDuration is the minimum task duration required to run
// the detailed summarizer. Below this, the stub is kept.
const summarizerTrivialDuration = 5 * time.Second

// Summarizer generates post-task activity notes for durable agent memory.
// It runs asynchronously after a task terminates: the caller first writes a
// synchronous stub (so the next task always sees something for this one), then
// fires SummarizeTask in a goroutine to replace the stub with a detailed note.
type Summarizer struct {
	client *managerclient.Client
	tasks  *TaskStore
	audit  *audit.Logger
	memory *memory.Store
	mask   *vault.Masker
	broker EventBroker
	usage  UsageRecorder // nil = usage not recorded (tests)
}

// SetUsageRecorder wires the per-machine usage accumulator for
// summarizer calls. Called once at startup wiring.
func (s *Summarizer) SetUsageRecorder(u UsageRecorder) {
	s.usage = u
}

// NewSummarizer wires the summarizer with direct dependencies. The manager
// client is passed in as a concrete pointer rather than through an interface
// to keep the engine's ManagerAPI test-double narrow.
func NewSummarizer(
	client *managerclient.Client,
	tasks *TaskStore,
	audit *audit.Logger,
	memory *memory.Store,
	mask *vault.Masker,
	broker EventBroker,
) *Summarizer {
	return &Summarizer{
		client: client,
		tasks:  tasks,
		audit:  audit,
		memory: memory,
		mask:   mask,
		broker: broker,
	}
}

// ActivityStubMarkdown returns the minimal stub activity value used for both
// the synchronous post-task write and the daemon-restart FailRunningTasks path.
// The stub is always written so downstream readers (agent context, UI Activity
// tab) have something anchored to this task ID even if the detailed summarizer
// never runs. No Notes line by default — "Detailed summary pending" was
// developer-speak that made users think something was still loading.
func ActivityStubMarkdown(instruction string, status TaskStatus, elapsed time.Duration, extraNotes string) string {
	var b strings.Builder
	b.WriteString("Task: ")
	b.WriteString(oneLine(instruction))
	b.WriteString("\n")
	b.WriteString("Status: ")
	b.WriteString(string(status))
	b.WriteString("\n")
	b.WriteString("Duration: ")
	b.WriteString(humanizeDuration(elapsed))
	b.WriteString("\n")
	if extraNotes != "" {
		b.WriteString("\nNotes: ")
		b.WriteString(extraNotes)
		b.WriteString("\n")
	}
	return b.String()
}

// ActivityProgressMarkdown is the live, in-chat version of an activity stub.
// It is emitted while a task is still running so the customer sees the current
// phase and the latest observable tool action instead of a bare typing spinner.
func ActivityProgressMarkdown(instruction string, elapsed time.Duration, current, evidence string) string {
	var b strings.Builder
	b.WriteString("Task: ")
	b.WriteString(oneLine(instruction))
	b.WriteString("\n")
	b.WriteString("Status: ")
	b.WriteString(string(TaskRunning))
	b.WriteString("\n")
	b.WriteString("Duration: ")
	b.WriteString(humanizeDuration(elapsed))
	b.WriteString("\n")
	if current != "" {
		b.WriteString("\nCurrent: ")
		b.WriteString(oneLine(current))
		b.WriteString("\n")
	}
	if evidence != "" {
		b.WriteString("Last activity: ")
		b.WriteString(oneLine(evidence))
		b.WriteString("\n")
	}
	return b.String()
}

// WriteStub writes a minimal activity entry for the given task. Used both as
// the synchronous pre-summarizer write in processTask and by FailRunningTasks
// on daemon startup for zombie tasks. Safe to call with either path — the
// memory Set is an upsert, so a later detailed summary replaces the stub.
func (s *Summarizer) WriteStub(task *Task, extraNotes string) error {
	elapsed := taskElapsed(task)
	markdown := ActivityStubMarkdown(task.Instruction, task.Status, elapsed, extraNotes)
	meta := buildMetadata(task, 0)
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)
	_, err := s.memory.Set(ActivityCategory, task.ID, markdown, &metaStr)
	return err
}

// WriteRunningStub writes a placeholder activity entry the moment a task
// STARTS running, so the History UI shows it immediately with status
// "running" instead of being invisible until the task ends. The same
// memory key is later overwritten by WriteStub (at task completion) and
// then again by SummarizeTask (the detailed async pass). All three writes
// upsert on (category, key) — last one wins.
//
// Also emits task:activity over SSE so live clients can render the new
// "Running…" row without polling.
func (s *Summarizer) WriteRunningStub(task *Task) error {
	// Always pass TaskRunning explicitly regardless of task.Status — the
	// caller invokes us right after marking the task running, but the
	// status field on the in-memory task struct may still be stale (the
	// UpdateStatus call only writes to the DB). This enforces the
	// "placeholder means in-flight" contract at the markdown level.
	markdown := ActivityStubMarkdown(task.Instruction, TaskRunning, 0, "")
	meta := buildMetadata(task, 0)
	// Override the status in metadata too — buildMetadata reads
	// task.Status which is also potentially stale here.
	meta["status"] = string(TaskRunning)
	meta["duration_ms"] = 0
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)
	if _, err := s.memory.Set(ActivityCategory, task.ID, markdown, &metaStr); err != nil {
		return err
	}
	s.broker.Emit("task:activity", map[string]interface{}{
		"task_id":          task.ID,
		"conversation_id":  task.ConversationID,
		"status":           string(TaskRunning),
		"duration_ms":      0,
		"step_count":       0,
		"result_one_line":  "",
		"summary_markdown": markdown,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// SummarizeTask produces a detailed activity summary by calling the manager with
// the task's audit log + message window. It is meant to run in a goroutine
// launched from processTask. The returned error is logged by the caller but
// never propagates to the user — on failure, the synchronous stub (already
// written by WriteStub) remains in place as the authoritative activity entry.
func (s *Summarizer) SummarizeTask(ctx context.Context, task *Task) error {
	auditEntries, err := s.audit.QueryByTask(task.ID)
	if err != nil {
		return fmt.Errorf("audit query: %w", err)
	}

	elapsed := taskElapsed(task)

	// Skip the detailed call only when the task is trivial by BOTH measures:
	// few audit entries AND short duration. Using OR produced false negatives —
	// e.g., a 7-step crafting task that completed in 4s got stub-only treatment
	// even though it clearly warranted a summary.
	if len(auditEntries) < summarizerTrivialAuditThreshold && elapsed < summarizerTrivialDuration {
		// Still emit the activity SSE event and parallel message using the stub,
		// so the UI pill appears even for trivial tasks.
		return s.emitStubAsActivity(ctx, task, len(auditEntries))
	}

	msgs, err := s.tasks.GetMessages(task.ConversationID)
	if err != nil {
		return fmt.Errorf("get messages: %w", err)
	}
	windowStart := taskWindowStart(task)
	windowEnd := taskWindowEnd(task)
	var windowMsgs []*Message
	for _, m := range msgs {
		if (m.CreatedAt.Equal(windowStart) || m.CreatedAt.After(windowStart)) &&
			(m.CreatedAt.Equal(windowEnd) || m.CreatedAt.Before(windowEnd)) {
			windowMsgs = append(windowMsgs, m)
		}
	}

	systemPrompt := summarizerSystemPrompt
	userPrompt := s.buildUserPrompt(task, elapsed, auditEntries, windowMsgs)

	release, reserveErr := reserveUsageCall(s.usage, summarizerModel, task.ConversationID)
	if reserveErr != nil {
		return nil
	}
	resp, err := s.client.SendText(ctx, summarizerModel, systemPrompt, userPrompt, summarizerMaxTokens)
	if err != nil {
		release()
		return fmt.Errorf("summarizer API call: %w", err)
	}

	// Attribute the summarizer's manager tokens to the originating chat so
	// the Usage panel's per-conversation breakdown is honest about the
	// full cost of running that task.
	if s.usage != nil {
		if uerr := s.usage.Record(summarizerModel, task.ConversationID, resp.Usage); uerr != nil {
			log.Printf("summarizer: usage.Record: %v", uerr)
		}
	}
	release()

	raw := strings.TrimSpace(resp.TextContent)
	if raw == "" {
		return fmt.Errorf("empty summarizer output")
	}

	masked := s.mask.Mask(raw)
	if len(masked) > summarizerValueCharCap {
		masked = masked[:summarizerValueCharCap] + "\n\n... (truncated)"
	}

	// Prepend the step-count marker so the client-side parser is synchronous
	// on reload — no need to fetch metadata separately.
	stepCount := len(auditEntries)
	storedMarkdown := fmt.Sprintf("<!-- steps:%d -->\n%s", stepCount, masked)

	meta := buildMetadata(task, stepCount)
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)

	if _, err := s.memory.Set(ActivityCategory, task.ID, storedMarkdown, &metaStr); err != nil {
		return fmt.Errorf("memory set: %w", err)
	}

	// Parallel message row so the pill survives page reload.
	if err := s.tasks.AddTypedMessage(task.ConversationID, "assistant", storedMarkdown, activitySummaryType, nil); err != nil {
		log.Printf("summarizer: AddTypedMessage failed for task %s: %v", task.ID, err)
	}

	s.emitActivityEvent(task, storedMarkdown, stepCount)
	return nil
}

// emitStubAsActivity emits the task:activity SSE event + parallel message row
// using the already-written stub, for trivial tasks that skip the detailed
// manager call.
func (s *Summarizer) emitStubAsActivity(ctx context.Context, task *Task, stepCount int) error {
	item, err := s.memory.GetByKey(ActivityCategory, task.ID)
	if err != nil {
		return fmt.Errorf("get stub: %w", err)
	}
	stub := item.Value
	marked := fmt.Sprintf("<!-- steps:%d -->\n%s", stepCount, stub)
	if err := s.tasks.AddTypedMessage(task.ConversationID, "assistant", marked, activitySummaryType, nil); err != nil {
		log.Printf("summarizer: AddTypedMessage failed for trivial task %s: %v", task.ID, err)
	}
	s.emitActivityEvent(task, marked, stepCount)
	return nil
}

// emitActivityEvent publishes the task:activity SSE event with the summary
// payload the UI needs to render the chat pill.
func (s *Summarizer) emitActivityEvent(task *Task, markdown string, stepCount int) {
	payload := map[string]interface{}{
		"task_id":          task.ID,
		"conversation_id":  task.ConversationID,
		"status":           string(task.Status),
		"duration_ms":      taskElapsed(task).Milliseconds(),
		"step_count":       stepCount,
		"result_one_line":  extractResultOneLine(markdown),
		"summary_markdown": markdown,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	}
	s.broker.Emit("task:activity", payload)
}

// buildUserPrompt assembles the vault-masked audit + message input for the manager.
// Image data is dropped from messages before serialization to keep input tokens
// bounded — the audit log already records every tool action.
func (s *Summarizer) buildUserPrompt(task *Task, elapsed time.Duration, auditEntries []audit.Entry, msgs []*Message) string {
	var b strings.Builder
	b.WriteString("Task instruction: ")
	b.WriteString(oneLine(task.Instruction))
	b.WriteString("\nStatus: ")
	b.WriteString(string(task.Status))
	b.WriteString("\nDuration: ")
	b.WriteString(humanizeDuration(elapsed))
	b.WriteString("\n\nAudit entries (chronological, tool calls):\n")
	for _, e := range auditEntries {
		masked := s.mask.Mask(e.Details)
		fmt.Fprintf(&b, "[%s] %s: %s\n", e.Timestamp.Format("15:04:05"), e.Action, oneLine(masked))
	}
	b.WriteString("\nMessages (agent text between tool calls, vault-masked, image_data excluded):\n")
	for _, m := range msgs {
		// Drop image_data from the summarizer input entirely.
		content := m.Content
		if m.Type == "screenshot" {
			if content == "" {
				content = "(screenshot captured)"
			} else {
				content = "(screenshot: " + content + ")"
			}
		}
		maskedContent := s.mask.Mask(content)
		fmt.Fprintf(&b, "[%s] %s: %s\n", m.CreatedAt.Format("15:04:05"), m.Role, oneLine(maskedContent))
	}
	return b.String()
}

// summarizerSystemPrompt is the full system prompt for the summarizer. It is
// tuned for the VibeCraft ICP (non-technical operators) and ruthlessly
// enforces grounding — the agent will read its own output back in future turns
// to answer "what did you do?" so fabrication must be eliminated.
const summarizerSystemPrompt = `You distill an agent's work on a single task into a short markdown note for
a non-technical business operator. The note is the agent's own memory — it
will be read by the agent on future turns to answer questions like "what did
you do yesterday?"

Rules:
- Lead with the user's original ask (one line, unmodified).
- Status: one of "completed", "failed", "cancelled".
- Duration: use the provided elapsed time.
- "What I did": 3-8 bullets, plain past-tense language, specific apps/URLs/files touched.
- "Result": one sentence, concrete outcome (file path, URL, numerical result, or error).
- "Notes": only if something non-obvious happened (retries, errors handled, partial success). Omit if not applicable.
- No marketing language. No "successfully". No "I was able to". Direct verbs only.
- If the audit log is thin (short task, no meaningful steps), write two lines and stop.
- Never fabricate. If you don't know a file path or outcome, say so.

Output format (markdown, no preamble):

Task: {user's original ask}
Status: {completed | failed | cancelled}
Duration: {elapsed}

What I did:
- {bullet}
- {bullet}

Result: {one sentence}

Notes: {optional}
`

// taskElapsed computes the elapsed wall-clock time for the task, defending
// against nil StartedAt / CompletedAt pointers with CreatedAt / UpdatedAt
// fallbacks. Terminal states should always have both set but we don't crash
// if a future code path leaves one nil.
func taskElapsed(task *Task) time.Duration {
	start := taskWindowStart(task)
	end := taskWindowEnd(task)
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func taskWindowStart(task *Task) time.Time {
	if task.StartedAt != nil {
		return *task.StartedAt
	}
	return task.CreatedAt
}

func taskWindowEnd(task *Task) time.Time {
	if task.CompletedAt != nil {
		return *task.CompletedAt
	}
	return task.UpdatedAt
}

// humanizeDuration formats a duration like "3m 28s" or "45s".
func humanizeDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	}
	if d < time.Hour {
		mins := int(d / time.Minute)
		secs := int((d % time.Minute) / time.Second)
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	hours := int(d / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %dm", hours, mins)
}

// oneLine collapses whitespace + trims to a single line representation of text.
// Used for task instructions and short audit details in the stub / prompt.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	// Collapse consecutive spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// buildMetadata assembles the metadata JSON persisted alongside the activity
// entry in the memory store. Keeps started/ended/status/duration/step_count
// addressable without reparsing the markdown.
func buildMetadata(task *Task, stepCount int) map[string]interface{} {
	start := taskWindowStart(task)
	end := taskWindowEnd(task)
	return map[string]interface{}{
		"conversation_id": task.ConversationID,
		"started_at":      start.UTC().Format(time.RFC3339),
		"ended_at":        end.UTC().Format(time.RFC3339),
		"status":          string(task.Status),
		"duration_ms":     taskElapsed(task).Milliseconds(),
		"step_count":      stepCount,
	}
}

// extractResultOneLine pulls the first `Result:` line out of the markdown
// for the SSE payload, falling back to the first non-blank line.
func extractResultOneLine(markdown string) string {
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Result:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "Result:"))
		}
	}
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		return trimmed
	}
	return ""
}
