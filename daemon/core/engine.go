// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/computer"
	"github.com/carloslfu/computer.md/daemon/guardrails"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/memory"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// errTaskNoLongerActive is the surface we return to a client that tries to
// act on a credential or approval card whose underlying task has ended
// (timed out, cancelled, failed, or completed). Customer-facing wording —
// it gets rendered verbatim under the card.
var errTaskNoLongerActive = errors.New("This card is no longer active because the task that asked for it ended. Start a new message to continue.")

// Manager API limits on inlined images (base64-encoded). Raw byte limits
// chosen conservatively: base64 inflates size by ~4/3, so 3.5 MB raw
// stays safely under the 5 MB wire cap even after encoding overhead.
const (
	maxInlineImageBytes = 3_500_000
	maxImagesPerMessage = 4
)

// supportedImageMIMEs lists the image formats the manager accepts. Anything
// else is referenced by path only so the agent can still open / analyze
// it from bash.
var supportedImageMIMEs = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/jpg":  true,
	"image/gif":  true,
	"image/webp": true,
}

// ManagerAPI is the interface for sending messages to the manager model.
// Extracted for testability.
type ManagerAPI interface {
	SendComputerUse(ctx context.Context, systemPrompt string, messages []managerclient.Message) (*managerclient.Response, error)
}

// NoToolsManagerAPI is implemented by the production OpenAI manager client.
// Tests and alternate managers can omit it and keep the normal tool loop.
type NoToolsManagerAPI interface {
	SendNoTools(ctx context.Context, systemPrompt string, messages []managerclient.Message) (*managerclient.Response, error)
}

type ManagerModelProvider interface {
	Model() string
}

// Screenshotter captures the current display and returns a base64-encoded
// PNG. Extracted from *computer.ScreenshotService so tests can inject a
// fake that returns canned image data without requiring a real X display.
type Screenshotter interface {
	CaptureBase64(ctx context.Context) (string, error)
}

// DefaultTaskTimeout caps the *agent-active* time a single task may consume.
// Time spent in waiting_for_input (approval / credential card) is paused
// and does NOT count toward this — the agent isn't doing work while the
// human is thinking. So this is a budget against real agent computation,
// not wall clock from start. 2 hours of pure agent time is generous for
// any real task; the watchdog is defense-in-depth for runaway loops and
// stuck goroutines, not a tolerance for humans.
const DefaultTaskTimeout = 2 * time.Hour

// HumanWaitTimeout caps how long the agent will block on a single human
// reply (approval click, credential submit) before giving up and asking
// the manager model what to do next. The budget is paused for the whole wait, so
// this only bounds the agent's patience, not the task's lifetime.
// 24h means a customer can step away — to lunch, overnight, for the
// weekend — and come back without the card silently rotting.
const HumanWaitTimeout = 24 * time.Hour

// apiCallTimeout is the maximum time for a single manager API call within the
// agent loop. This is a safety net above the HTTP transport-level timeouts.
const apiCallTimeout = 3 * time.Minute

const directImageAttachmentPrompt = `
Current turn attachment rule:
The current user message includes user-uploaded image bytes inline. For this turn, the attached image is the object to inspect. Answer from the attached image itself. Do not take a desktop screenshot to answer what the attached image shows. Only use the live desktop if the customer explicitly asks you to interact with the computer.`

// Notifier pushes a high-priority cross-machine notification to the
// operator (forwards to the platform's notification ingest, dispatches
// bell + email per priority). Set via SetNotifier; nil = log only.
type Notifier interface {
	Notify(kind, title, body, priority string)
}

// DiskHealth thresholds. Hysteresis prevents thrashing when the agent
// hovers right at the line.
const (
	DiskPauseRatio   = 0.10 // <10% free → pause
	DiskResumeRatio  = 0.15 // >15% free → resume
	DiskCheckPath    = "/var/lib/vibecraft"
	DiskRetryBackoff = 30 * time.Second
)

// Engine is the agent core. It processes tasks from the queue, interacts
// with the manager model, executes computer actions, and manages the full agent loop.
// ShellExecutor is the bash surface the engine drives. *computer.Shell
// (host bash) satisfies it; so does the Phase 2 long-lived agent-shell
// sandbox (sandbox.AgentShell). Routing every agent bash + text_editor
// call through this interface is what lets the agent's whole world move
// inside a sandbox without touching the tool surface.
type ShellExecutor interface {
	Execute(ctx context.Context, command string) (string, error)
	ExecuteWithEnv(ctx context.Context, command string, extraEnv []string) (string, error)
	ExecuteInteractive(ctx context.Context, command, input string) (string, error)
}

type Engine struct {
	tasks      *TaskStore
	manager    ManagerAPI
	computer   *computer.Controller
	screenshot Screenshotter
	shell      *computer.Shell
	// agentShell, when set (Linux production via main.go), is the
	// long-lived agent-shell sandbox; the engine runs all bash +
	// text_editor through it instead of host bash (Phase 2). nil on the
	// macOS dev build → falls back to e.shell (host bash), unchanged.
	agentShell ShellExecutor
	guardrails *guardrails.Engine
	memory     *memory.Store
	vault      *vault.Store
	vaultMask  *vault.Masker
	audit      *audit.Logger
	prompt     string
	broker     EventBroker
	summarizer *Summarizer
	compactor  *Compactor                 // nil = no mid-task compaction; tasks grow until the model's hard limit
	classifier *guardrails.RiskClassifier // nil = every guardrail Confirm goes straight to a human approval card
	notifier   Notifier                   // nil = log only
	usage      UsageRecorder              // nil = usage not recorded (tests); production wires usage.Store
	// diskFreeRatio reports current free-space ratio at DiskCheckPath.
	// Override in tests; nil → DefaultDiskFreeRatio(DiskCheckPath).
	diskFreeRatio func() (float64, error)
	// diskPaused tracks whether the engine is currently paused on
	// low-disk. Edge-triggered notifications fire on transitions only.
	diskPaused bool

	TaskTimeout time.Duration // overridable for testing; defaults to DefaultTaskTimeout

	// IsPaused exposes the disk-pause state for /metrics and any other
	// read-only observer. Reads e.diskPaused under mu so the value is
	// consistent with the writer in shouldDequeue().
	mu                sync.Mutex
	currentTaskID     string
	currentTaskConvID string // conversation_id of currentTaskID; tracked so cross-conversation new tasks can redirect attention without a DB read
	cancel            context.CancelFunc
	currentBudget     *taskBudget // budget for the currently-running task; nil between tasks
	running           bool
	progressSteps     map[string]int
	inputCh           chan string
	// credentialsInputCh delivers the user's submission (or dismissal) of
	// a request_credentials card back to the suspended agent loop. Buffered
	// so the HTTP handler never blocks on a stale reader.
	credentialsInputCh chan CredentialResponse
	// redirectCh fires when the user posts a new task in a DIFFERENT
	// conversation while the current task is parked on an approval or
	// credential card. The select arms in the two wait sites consume it
	// and treat it like "the customer moved on" — expire the card, return
	// a tool_error, free the engine loop. Buffered cap-1 so the HTTP
	// handler that triggers it never blocks. See NotifyNewTaskInConversation.
	redirectCh chan struct{}
	// pendingCredMsgs maps a waiting task id to the persisted
	// credential_request bubble so SubmitCredentials can flip it to its
	// "stored" state, and the terminal-task paths can flip it to
	// "expired", without scanning the conversation for it.
	pendingCredMsgs map[string]pendingCredMsg
	// pendingApprovalMsgs maps a waiting task id to the persisted
	// approval bubble so terminal-state paths (timeout, ctx-cancel,
	// cross-conversation redirect) can stamp it expired symmetrically
	// with credential cards.
	pendingApprovalMsgs map[string]pendingApprovalMsg
}

// EventBroker is the interface the engine uses to emit SSE events.
type EventBroker interface {
	Emit(eventType string, data interface{})
}

// pendingCredMsg holds the message id of a persisted credential_request
// bubble and a snapshot of its payload, so terminal-state handlers can
// stamp it expired without a DB read for the message content.
type pendingCredMsg struct {
	MessageID string
	Payload   CredentialRequestPayload
}

// pendingApprovalMsg is the approval-card analog of pendingCredMsg.
type pendingApprovalMsg struct {
	MessageID string
	Payload   ApprovalPayload
}

// NewEngine creates a fully wired agent engine.
func NewEngine(
	tasks *TaskStore,
	manager ManagerAPI,
	ctrl *computer.Controller,
	ss Screenshotter,
	sh *computer.Shell,
	ge *guardrails.Engine,
	mem *memory.Store,
	v *vault.Store,
	vm *vault.Masker,
	al *audit.Logger,
	prompt string,
	broker EventBroker,
) *Engine {
	return &Engine{
		tasks:               tasks,
		manager:             manager,
		computer:            ctrl,
		screenshot:          ss,
		shell:               sh,
		guardrails:          ge,
		memory:              mem,
		vault:               v,
		vaultMask:           vm,
		audit:               al,
		prompt:              prompt,
		broker:              broker,
		progressSteps:       make(map[string]int),
		inputCh:             make(chan string, 1),
		credentialsInputCh:  make(chan CredentialResponse, 1),
		redirectCh:          make(chan struct{}, 1),
		pendingCredMsgs:     make(map[string]pendingCredMsg),
		pendingApprovalMsgs: make(map[string]pendingApprovalMsg),
		TaskTimeout:         DefaultTaskTimeout,
	}
}

// SetSummarizer wires the post-task activity summarizer. Called during
// startup wiring. Nil is acceptable (summaries are simply skipped).
func (e *Engine) SetSummarizer(s *Summarizer) {
	e.summarizer = s
}

// UsageRecorder accumulates manager API token consumption for the
// dashboard's Usage panel. The engine calls Record after every manager
// response. Nil means no recording — tests run without a usage store
// because they don't need to verify token accounting end-to-end (the
// usage package has its own dedicated tests for that).
type UsageRecorder interface {
	Record(model, conversationID string, usage managerclient.Usage) error
}

// SetUsageRecorder wires the per-machine usage accumulator. Called
// once at startup wiring. After this is set, every manager call the
// engine makes is attributed by (model, conversation_id, day) to a
// persistent total the dashboard reads back as a dollar amount.
func (e *Engine) SetUsageRecorder(u UsageRecorder) {
	e.usage = u
}

// SetCompactor wires the mid-task history compactor. Called during
// startup wiring. Nil is acceptable — without a compactor the agent
// loop appends every turn forever, and very long tasks will eventually
// push input size into expensive long-context territory.
func (e *Engine) SetCompactor(c *Compactor) {
	e.compactor = c
}

// SetRiskClassifier wires the auto-review pass that runs between the
// regex-based guardrail engine and the human approval card. When the
// classifier returns "approve", the action runs silently and an audit
// entry is logged; when it returns "escalate" (or is unset), the
// human card is shown as before. The classifier can never elevate an
// Allow or soften a Block — it only downgrades Confirm to Allow.
func (e *Engine) SetRiskClassifier(c *guardrails.RiskClassifier) {
	e.classifier = c
}

// SetNotifier wires the cross-machine push channel. nil disables push
// notifications; the engine still logs and audits.
func (e *Engine) SetNotifier(n Notifier) {
	e.notifier = n
}

// SetDiskFreeRatio overrides the default disk-space probe (used in
// tests). nil resets to DefaultDiskFreeRatio(DiskCheckPath).
// IsPaused returns whether the engine is currently in the low-disk
// paused state. Read-only; used by /metrics (Workstream K) to surface
// fleet-wide pause rate to the platform's health dashboard.
func (e *Engine) IsPaused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.diskPaused
}

func (e *Engine) SetDiskFreeRatio(f func() (float64, error)) {
	e.diskFreeRatio = f
}

// checkDiskHealthy returns true if the daemon should keep dequeuing
// tasks, false if the engine should pause on low disk. Emits notifier
// + audit on healthy↔paused transitions only — no per-iteration spam.
func (e *Engine) checkDiskHealthy() bool {
	probe := e.diskFreeRatio
	if probe == nil {
		probe = func() (float64, error) { return DefaultDiskFreeRatio(DiskCheckPath) }
	}
	ratio, err := probe()
	if err != nil {
		// Unable to probe → don't pause. Log and continue; we'd rather
		// process work than block the engine on a transient statfs hiccup.
		log.Printf("disk: probe failed: %v (continuing)", err)
		return true
	}

	if e.diskPaused {
		// Currently paused. Only resume above the hysteresis ceiling.
		if ratio >= DiskResumeRatio {
			e.diskPaused = false
			pct := int(ratio * 100)
			log.Printf("disk: free=%d%% — resuming engine loop", pct)
			e.audit.Log(audit.Entry{
				Action:   "disk_resumed",
				Category: "system",
				Details:  fmt.Sprintf("free=%d%% threshold=%d%%", pct, int(DiskResumeRatio*100)),
			})
			if e.notifier != nil {
				e.notifier.Notify(
					"system:disk_recovered",
					"Disk space recovered",
					fmt.Sprintf("Disk free space is back to %d%%. The agent has resumed task processing.", pct),
					"normal",
				)
			}
		}
		return !e.diskPaused
	}

	// Currently healthy. Pause if we cross the low threshold.
	if ratio < DiskPauseRatio {
		e.diskPaused = true
		pct := int(ratio * 100)
		log.Printf("disk: free=%d%% — pausing engine loop (threshold=%d%%)", pct, int(DiskPauseRatio*100))
		e.audit.Log(audit.Entry{
			Action:    "disk_paused",
			Category:  "system",
			Details:   fmt.Sprintf("free=%d%% threshold=%d%%", pct, int(DiskPauseRatio*100)),
			RiskLevel: "high",
		})
		if e.notifier != nil {
			e.notifier.Notify(
				"system:disk_low",
				"Machine disk almost full",
				fmt.Sprintf("Disk free space dropped to %d%%. The agent has paused all task processing to avoid corruption. The engine will resume automatically once free space exceeds %d%%.", pct, int(DiskResumeRatio*100)),
				"high",
			)
		}
		return false
	}
	return true
}

// fetchRecentActivities reads the last N activity notes from the memory store,
// ordered newest-first, and returns them as ActivityEntry values for context
// injection. Silently returns nil if the memory store is empty or errors.
func (e *Engine) fetchRecentActivities(limit int) []ActivityEntry {
	items, err := e.memory.GetRecentByCategory(ActivityCategory, limit)
	if err != nil || len(items) == 0 {
		return nil
	}
	// GetRecentByCategory returns newest-first. For context we want
	// chronological order (oldest → newest) so the agent reads history in
	// time order when recounting.
	out := make([]ActivityEntry, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		out = append(out, ActivityEntry{
			TaskID:    it.Key,
			Markdown:  it.Value,
			UpdatedAt: it.UpdatedAt,
		})
	}
	return out
}

// recentChatForClassifier returns a compact plain-text rendering of the
// most recent N messages in the conversation, oldest first. Used to give
// the risk classifier intent-alignment context — what was the customer
// asking for, what has the agent been doing — without bloating the
// classifier's input with screenshots or approval-card JSON.
//
// Filters applied:
//   - approval / credential_request payloads (JSON, not conversation)
//     are summarised to a one-liner so they don't clutter the prompt
//   - everything is plain text; no base64 image data
//   - very long messages are truncated at 600 chars per turn
//
// Returns "" if the conversation has no messages yet (the classifier
// will just judge on the action + task instruction).
func (e *Engine) recentChatForClassifier(conversationID string, limit int) string {
	if conversationID == "" || limit <= 0 {
		return ""
	}
	msgs, err := e.tasks.GetMessages(conversationID)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	var b strings.Builder
	for _, m := range msgs {
		if m == nil {
			continue
		}
		role := m.Role
		switch role {
		case "user", "agent", "system":
		default:
			role = "agent"
		}
		content := strings.TrimSpace(m.Content)
		switch m.Type {
		case "approval":
			content = "[approval card shown to customer]"
		case "credential_request":
			content = "[credential request shown to customer]"
		case "screenshot":
			// Screenshot messages carry the caption in .Content; keep
			// the caption and tag the line so the classifier knows.
			if content == "" {
				content = "[screenshot, no caption]"
			} else {
				content = "[screenshot] " + content
			}
		}
		if content == "" {
			continue
		}
		if len(content) > 600 {
			content = content[:600] + "…"
		}
		fmt.Fprintf(&b, "%s: %s\n", role, content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Start begins the task processing loop in the background.
func (e *Engine) Start(ctx context.Context) {
	go e.loop(ctx)
}

// SubmitInput sends user input to a task that is waiting for it.
func (e *Engine) SubmitInput(taskID, input string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.currentTaskID != taskID {
		return errTaskNoLongerActive
	}

	task, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if task.Status != TaskWaitingForInput {
		return errTaskNoLongerActive
	}

	// Store the user's response as a message. Normalise "yes"/"no" into a
	// human-readable label so the chat history reads naturally; anything
	// else is stored verbatim (for future free-form response flows).
	label := input
	switch input {
	case "yes", "y":
		label = "Approved"
	case "no", "n":
		label = "Denied"
	}
	e.tasks.AddMessage(task.ConversationID, "user", label)

	select {
	case e.inputCh <- input:
	default:
	}

	return nil
}

// SubmitCredentials writes each submitted value to the vault and unblocks
// the agent loop that's waiting inside handleCredentialRequest. Called
// from the HTTP handler after it has authenticated the request and
// verified the task is in waiting_for_input.
//
// On cancel (user dismissed the card), no vault writes happen and the
// agent receives a tool_error it can react to.
func (e *Engine) SubmitCredentials(taskID string, resp CredentialResponse) error {
	e.mu.Lock()
	if e.currentTaskID != taskID {
		e.mu.Unlock()
		return errTaskNoLongerActive
	}
	msgID := e.pendingCredMsgs[taskID].MessageID
	e.mu.Unlock()

	task, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if task.Status != TaskWaitingForInput {
		return errTaskNoLongerActive
	}

	if !resp.Cancelled {
		// Double-check every name against the original request before
		// writing — defence against a client that fabricates names the
		// agent never asked for.
		var payload CredentialRequestPayload
		if task.Result != nil {
			if p, ok := DecodeCredentialRequestJSON(*task.Result); ok {
				payload = p
			}
		}
		allowed := make(map[string]CredentialSpec, len(payload.Fields))
		for _, f := range payload.Fields {
			allowed[f.Name] = f
		}
		for _, v := range resp.Values {
			if _, ok := allowed[v.Name]; !ok {
				return fmt.Errorf("credential %q was not part of this request", v.Name)
			}
			if v.Value == "" {
				return fmt.Errorf("credential %q has an empty value", v.Name)
			}
		}

		// Snapshot which names already existed so a mid-write failure
		// only rolls back the NEW entries we created. Existing names
		// have values we can't restore (the agent doesn't hold them),
		// so we leave them alone on rollback — worst case, the user
		// retries and overwrites with the same value they just typed.
		preExisting := make(map[string]bool, len(resp.Values))
		for _, name := range e.vault.Names() {
			preExisting[name] = true
		}

		var created []string // names newly created by this submission
		for _, v := range resp.Values {
			fresh, err := e.tasks.GetTask(taskID)
			if err != nil {
				return err
			}
			if fresh.Status != TaskWaitingForInput {
				return errTaskNoLongerActive
			}
			label := v.Label
			if label == "" {
				label = allowed[v.Name].Label
			}
			if err := e.vault.Set(v.Name, v.Value, label); err != nil {
				// Roll back the new writes only. Audit the rollback so
				// an operator debugging a partial-write incident can
				// see exactly which names were touched.
				for _, name := range created {
					if delErr := e.vault.Delete(name); delErr != nil {
						log.Printf("credential_request: rollback delete for %s failed: %v", name, delErr)
					}
				}
				e.audit.Log(audit.Entry{
					Action:    "credentials_submit_partial_rollback",
					Category:  "vault",
					TaskID:    taskID,
					Details:   fmt.Sprintf("failed on %s; rolled back %d new entries", v.Name, len(created)),
					RiskLevel: "high",
				})
				return fmt.Errorf("vault write for %s failed (%w); partial writes rolled back", v.Name, err)
			}
			if !preExisting[v.Name] {
				created = append(created, v.Name)
			}
			e.audit.Log(audit.Entry{
				Action:    "vault_secret_set_from_credential_request",
				Category:  "vault",
				TaskID:    taskID,
				Details:   v.Name,
				RiskLevel: "medium",
			})
		}

		// Flip the persisted card to its "stored" state so page reloads
		// rehydrate the receipt, not the input form.
		storedNames := make([]string, len(resp.Values))
		for i, v := range resp.Values {
			storedNames[i] = v.Name
		}
		payload.Stored = &CredentialStored{
			At:    time.Now().UTC().Format(time.RFC3339),
			Names: storedNames,
		}
		updated := payload.JSON()
		if msgID != "" && updated != "" {
			if err := e.tasks.UpdateMessageContent(msgID, updated); err != nil {
				log.Printf("credential_request: failed to update persisted card for task %s: %v", taskID, err)
			}
		}

		e.broker.Emit("task:credentials_stored", map[string]interface{}{
			"task_id":    taskID,
			"message_id": msgID,
			"payload":    payload,
		})
	}

	select {
	case e.credentialsInputCh <- resp:
	default:
	}
	return nil
}

// CancelTask cancels the currently running task if it matches.
func (e *Engine) CancelTask(taskID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.currentTaskID == taskID && e.cancel != nil {
		e.cancel()
		return nil
	}

	// If it's queued, just mark it cancelled directly.
	task, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if task.Status == TaskQueued {
		return e.tasks.UpdateStatus(taskID, TaskCancelled)
	}

	return fmt.Errorf("task %s cannot be cancelled (status: %s)", taskID, task.Status)
}

// CurrentTaskID returns the ID of the currently running task, if any.
func (e *Engine) CurrentTaskID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentTaskID
}

// pauseBudget pauses the active-time budget for the currently-running task.
// No-op if no task is running. Always paired with resumeBudget in a defer
// or matching select case.
func (e *Engine) pauseBudget() {
	e.mu.Lock()
	b := e.currentBudget
	e.mu.Unlock()
	if b != nil {
		b.Pause()
	}
}

func (e *Engine) resumeBudget() {
	e.mu.Lock()
	b := e.currentBudget
	e.mu.Unlock()
	if b != nil {
		b.Resume()
	}
}

// markCredentialCardExpired stamps the persisted credential_request message
// with an ExpiredAt + ExpiredReason so a returning or reconnecting client
// renders the expired-receipt state instead of a live form. Idempotent —
// preserves Stored if it was already set (the card had been submitted) and
// preserves an earlier ExpiredAt if one exists.
func (e *Engine) markCredentialCardExpired(msgID string, payload CredentialRequestPayload, reason string) {
	if msgID == "" {
		return
	}
	if payload.Stored != nil {
		return
	}
	payload.MessageID = msgID
	payload.ExpiredAt = time.Now().UTC().Format(time.RFC3339)
	payload.ExpiredReason = reason
	updated := payload.JSON()
	if updated == "" {
		return
	}
	if err := e.tasks.UpdateMessageContent(msgID, updated); err != nil {
		log.Printf("credential_request: failed to mark message %s expired: %v", msgID, err)
	}
}

// markApprovalCardExpired stamps the persisted approval message with an
// ExpiredAt + ExpiredReason so a returning or reconnecting client renders
// the expired-receipt state instead of a live approval form. Mirror of
// markCredentialCardExpired. Idempotent — preserves Resolved if the user
// already answered, and an earlier ExpiredAt if one exists.
func (e *Engine) markApprovalCardExpired(msgID string, payload ApprovalPayload, reason string) {
	if msgID == "" {
		return
	}
	if payload.Resolved != "" {
		return
	}
	payload.MessageID = msgID
	payload.ExpiredAt = time.Now().UTC().Format(time.RFC3339)
	payload.ExpiredReason = reason
	updated := payload.JSON()
	if updated == "" {
		return
	}
	if err := e.tasks.UpdateMessageContent(msgID, updated); err != nil {
		log.Printf("approval: failed to mark message %s expired: %v", msgID, err)
	}
}

// markOpenInputsExpired stamps any open credential_request OR approval
// bubble for the given task as expired. Called from the task-terminal
// paths in processTask (timeout / cancel / failure / panic) so a card the
// user might still have open in the UI doesn't outlive the task it
// belonged to.
func (e *Engine) markOpenInputsExpired(task *Task) {
	if task == nil {
		return
	}
	e.mu.Lock()
	pendingCred, hasCred := e.pendingCredMsgs[task.ID]
	pendingApproval, hasApproval := e.pendingApprovalMsgs[task.ID]
	e.mu.Unlock()
	if hasCred && pendingCred.MessageID != "" {
		e.markCredentialCardExpired(pendingCred.MessageID, pendingCred.Payload, "task ended")
		e.broker.Emit("task:credentials_expired", map[string]interface{}{
			"task_id":    task.ID,
			"message_id": pendingCred.MessageID,
			"reason":     "task ended",
		})
	}
	if hasApproval && pendingApproval.MessageID != "" {
		e.markApprovalCardExpired(pendingApproval.MessageID, pendingApproval.Payload, "task ended")
		e.broker.Emit("task:approval_expired", map[string]interface{}{
			"task_id":    task.ID,
			"message_id": pendingApproval.MessageID,
			"reason":     "task ended",
		})
	}
}

// NotifyNewTaskInConversation tells the engine that the user has just
// queued a new task in a different conversation. If the currently-held
// task is parked on an approval or credential card (waiting_for_input),
// that card is auto-expired — the user has clearly moved on. The agent
// loop receives a tool_error, wraps up the old task, and the engine loop
// frees up to claim the new task.
//
// Same-conversation new tasks queue normally; this is the cross-chat
// redirect signal only. No-op when the engine is idle or the held task
// is actually running (we never auto-cancel work-in-flight — only parked
// human-wait state, where nothing is actually happening).
func (e *Engine) NotifyNewTaskInConversation(newConvID string) {
	if newConvID == "" {
		return
	}
	e.mu.Lock()
	currentID := e.currentTaskID
	currentConvID := e.currentTaskConvID
	e.mu.Unlock()
	if currentID == "" || currentConvID == "" || currentConvID == newConvID {
		return
	}
	task, err := e.tasks.GetTask(currentID)
	if err != nil || task == nil || task.Status != TaskWaitingForInput {
		return
	}
	select {
	case e.redirectCh <- struct{}{}:
	default:
		// Already pending — fine, one signal is enough.
	}
}

func (e *Engine) loop(ctx context.Context) {
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	// Clean up zombie tasks from a previous daemon run. Each zombie also
	// gets a stub activity entry so the epistemic prompt rule has an anchor
	// ("I was interrupted on that one") instead of the agent fabricating an
	// outcome for work that was abandoned mid-flight.
	zombies, err := e.tasks.ListZombieTasks()
	if err != nil {
		log.Printf("error listing zombie tasks: %v", err)
	}
	if n, err := e.tasks.FailRunningTasks("daemon restarted while task was running"); err != nil {
		log.Printf("error cleaning up zombie tasks: %v", err)
	} else if n > 0 {
		log.Printf("cleaned up %d zombie tasks from previous run", n)
		e.audit.Log(audit.Entry{
			Action:   "zombie_cleanup",
			Category: "system",
			Details:  fmt.Sprintf("marked %d running tasks as failed on startup", n),
		})
	}
	if e.summarizer != nil {
		for _, t := range zombies {
			// Re-fetch so Status/CompletedAt reflect the just-applied FailRunningTasks update.
			fresh, ferr := e.tasks.GetTask(t.ID)
			if ferr != nil {
				continue
			}
			if werr := e.summarizer.WriteStub(fresh, "interrupted by daemon restart"); werr != nil {
				log.Printf("error writing restart stub for task %s: %v", fresh.ID, werr)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Disk-full pre-warning: pause the entire engine loop when free
		// space drops below the low threshold. Resumes automatically once
		// space recovers above the hysteresis ceiling. We pause BEFORE
		// dequeuing — a paused engine doesn't spin manager API calls or
		// computer-control side effects, and zombie tasks from the prior
		// run have already been failed at startup so they aren't stuck.
		if !e.checkDiskHealthy() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(DiskRetryBackoff):
			}
			continue
		}

		task, err := e.tasks.ClaimNextQueued()
		if err != nil {
			log.Printf("error fetching next task: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if task == nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Run the task. There is no wall-clock cap at this layer — the
		// task's budget cancels the task context when the agent has burned
		// its active-time allowance, and the inner watchdog inside
		// agentLoop force-fails the DB record if the agent goroutine
		// doesn't respect the cancel. This outer guard only handles
		// engine-shutdown: we give the task 5 minutes to wind down after
		// the parent context is cancelled, then abandon the goroutine
		// and exit the loop so the engine can stop cleanly.
		done := make(chan struct{})
		go func() {
			e.safeProcessTask(ctx, task)
			close(done)
		}()

		select {
		case <-done:
			// Task completed normally.
		case <-ctx.Done():
			select {
			case <-done:
			case <-time.After(5 * time.Minute):
				errMsg := "task abandoned by engine (stuck past inner watchdog after shutdown)"
				log.Printf("engine: %s: task=%s", errMsg, task.ID)
				if err := e.tasks.SetError(task.ID, errMsg); err != nil {
					log.Printf("engine: SetError for abandoned task %s: %v", task.ID, err)
				}
				e.broker.Emit("task:failed", map[string]interface{}{
					"task_id": task.ID,
					"error":   errMsg,
				})
				e.audit.Log(audit.Entry{
					Action:    "task_abandoned",
					Category:  "task",
					TaskID:    task.ID,
					Details:   errMsg,
					RiskLevel: "critical",
				})
				e.mu.Lock()
				e.currentTaskID = ""
				e.cancel = nil
				e.currentBudget = nil
				e.mu.Unlock()
			}
			return
		}
	}
}

// safeProcessTask wraps processTask with panic recovery so the engine
// loop never dies from an unexpected panic in agent code.
func (e *Engine) safeProcessTask(ctx context.Context, task *Task) {
	defer func() {
		if r := recover(); r != nil {
			errMsg := fmt.Sprintf("agent panic: %v", r)
			log.Printf("PANIC recovered in task %s: %s", task.ID, errMsg)

			// Ensure engine state is cleaned up.
			e.mu.Lock()
			e.currentTaskID = ""
			e.cancel = nil
			e.currentBudget = nil
			e.mu.Unlock()

			if statusErr := e.tasks.SetError(task.ID, errMsg); statusErr != nil {
				log.Printf("error setting panic error for task %s: %v", task.ID, statusErr)
			}
			e.broker.Emit("task:failed", map[string]interface{}{
				"task_id": task.ID,
				"error":   errMsg,
			})
			e.audit.Log(audit.Entry{
				Action:    "task_panic",
				Category:  "task",
				TaskID:    task.ID,
				Details:   errMsg,
				RiskLevel: "critical",
			})
			e.markOpenInputsExpired(task)
		}
	}()
	e.processTask(ctx, task)
}

func (e *Engine) processTask(parentCtx context.Context, task *Task) {
	// The budget is the task-level cancellation source. It cancels ctx when
	// the agent has consumed TaskTimeout of *active* time (waiting_for_input
	// time is paused and does not count). Manual cancel still works through
	// the same ctx — the budget just adds an additional reason it might fire.
	ctx, budget := newTaskBudget(parentCtx, e.TaskTimeout)
	ctx, cancel := context.WithCancel(ctx)

	e.mu.Lock()
	e.currentTaskID = task.ID
	e.currentTaskConvID = task.ConversationID
	e.cancel = cancel
	e.currentBudget = budget
	e.progressSteps[task.ID] = 0
	e.mu.Unlock()

	defer func() {
		cancel()
		e.mu.Lock()
		e.currentTaskID = ""
		e.currentTaskConvID = ""
		e.cancel = nil
		e.currentBudget = nil
		delete(e.progressSteps, task.ID)
		e.mu.Unlock()
	}()

	// Mark as running.
	if err := e.tasks.UpdateStatus(task.ID, TaskRunning); err != nil {
		log.Printf("error updating task status: %v", err)
		return
	}
	if fresh, err := e.tasks.GetTask(task.ID); err == nil {
		*task = *fresh
	}

	// conversation_id rides on every lifecycle event so the dashboard
	// can scope behaviors like "only auto-cancel when the user types in
	// THIS conversation." Foreground attention can switch between
	// conversations without killing background work the user directed
	// elsewhere.
	e.broker.Emit("task:started", map[string]interface{}{
		"task_id":         task.ID,
		"conversation_id": task.ConversationID,
		"status":          "running",
	})

	// Write the "Running…" placeholder activity entry so the History tab
	// shows this task immediately, not only after it ends. The placeholder
	// is upserted by WriteStub (at completion) and SummarizeTask (async
	// detailed pass), so we never end up with a stuck "Running" row.
	// Nil-check matches recordActivity — tests build engines without a
	// summarizer attached.
	if e.summarizer != nil {
		if err := e.summarizer.WriteRunningStub(task); err != nil {
			log.Printf("running stub write failed for task %s: %v", task.ID, err)
		}
	}

	e.audit.Log(audit.Entry{
		Action:   "task_started",
		Category: "task",
		TaskID:   task.ID,
		Details:  task.Instruction,
	})

	// Run the agent loop.
	result, err := e.agentLoop(ctx, task)
	if err != nil {
		if budget.Exhausted() {
			// Task burned its active-time budget. The budget already paused
			// for all waiting_for_input time, so this only fires when the
			// agent itself spent TaskTimeout doing actual work — almost
			// always a runaway loop or a stuck tool, not a slow human.
			errMsg := fmt.Sprintf("task hit its active-time budget of %s (paused human-wait time does not count)", e.TaskTimeout)
			if statusErr := e.tasks.SetError(task.ID, errMsg); statusErr != nil {
				log.Printf("error setting task %s timeout error: %v", task.ID, statusErr)
			}
			e.broker.Emit("task:failed", map[string]interface{}{
				"task_id": task.ID,
				"error":   errMsg,
			})
			e.audit.Log(audit.Entry{
				Action:    "task_timeout",
				Category:  "task",
				TaskID:    task.ID,
				Details:   errMsg,
				RiskLevel: "high",
			})
			e.markOpenInputsExpired(task)
			e.recordActivity(parentCtx, task.ID)
			return
		}
		if ctx.Err() != nil {
			// User-initiated cancellation.
			if statusErr := e.tasks.UpdateStatus(task.ID, TaskCancelled); statusErr != nil {
				log.Printf("error updating task %s to cancelled: %v", task.ID, statusErr)
			}
			e.broker.Emit("task:cancelled", map[string]string{
				"task_id": task.ID,
			})
			e.audit.Log(audit.Entry{
				Action:   "task_cancelled",
				Category: "task",
				TaskID:   task.ID,
			})
			e.markOpenInputsExpired(task)
			e.recordActivity(parentCtx, task.ID)
			return
		}
		errMsg := err.Error()
		if statusErr := e.tasks.SetError(task.ID, errMsg); statusErr != nil {
			log.Printf("error setting task %s error: %v", task.ID, statusErr)
		}
		e.broker.Emit("task:failed", map[string]interface{}{
			"task_id": task.ID,
			"error":   errMsg,
		})
		e.audit.Log(audit.Entry{
			Action:    "task_failed",
			Category:  "task",
			TaskID:    task.ID,
			Details:   errMsg,
			RiskLevel: "medium",
		})
		e.markOpenInputsExpired(task)
		e.recordActivity(parentCtx, task.ID)
		return
	}

	// Store assistant response.
	e.tasks.AddMessage(task.ConversationID, "assistant", result)
	e.tasks.SetResult(task.ID, result)

	e.broker.Emit("task:completed", map[string]interface{}{
		"task_id": task.ID,
		"result":  result,
	})

	e.audit.Log(audit.Entry{
		Action:   "task_completed",
		Category: "task",
		TaskID:   task.ID,
	})
	e.recordActivity(parentCtx, task.ID)
}

// recordActivity writes the synchronous stub activity entry and, if the task
// is non-trivial, kicks off the async detailed summarizer in a goroutine.
// The goroutine is intentionally detached from the task's context (so it
// continues even after the next task starts) but has its own 30s timeout.
// Caller must have already transitioned the task to a terminal state.
func (e *Engine) recordActivity(parentCtx context.Context, taskID string) {
	if e.summarizer == nil {
		return
	}
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		log.Printf("recordActivity: get task %s: %v", taskID, err)
		return
	}
	if err := e.summarizer.WriteStub(fresh, ""); err != nil {
		log.Printf("recordActivity: write stub for %s: %v", taskID, err)
		// Do not return — we still want the async summary to attempt an upsert.
	}
	stepCount := e.currentProgressStep(taskID)
	stubMarkdown := ActivityStubMarkdown(fresh.Instruction, fresh.Status, taskElapsed(fresh), "")
	if stepCount > 0 {
		stubMarkdown = fmt.Sprintf("<!-- steps:%d -->\n%s", stepCount, stubMarkdown)
	}
	e.broker.Emit("task:activity", map[string]interface{}{
		"task_id":          fresh.ID,
		"conversation_id":  fresh.ConversationID,
		"status":           string(fresh.Status),
		"duration_ms":      taskElapsed(fresh).Milliseconds(),
		"step_count":       stepCount,
		"result_one_line":  extractResultOneLine(stubMarkdown),
		"summary_markdown": stubMarkdown,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	})

	// Detach summarizer from the task's cancelled context.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Guard against parent process death during the detached window.
		_ = parentCtx
		if err := e.summarizer.SummarizeTask(ctx, fresh); err != nil {
			log.Printf("recordActivity: summarize %s: %v", taskID, err)
		}
	}()
}

func (e *Engine) currentProgressStep(taskID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.progressSteps[taskID]
}

func (e *Engine) nextProgressStep(taskID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.progressSteps[taskID]++
	return e.progressSteps[taskID]
}

func (e *Engine) emitTaskProgress(task *Task, current, evidence string, increment bool) {
	if task == nil || e.broker == nil {
		return
	}
	stepCount := e.currentProgressStep(task.ID)
	if increment {
		stepCount = e.nextProgressStep(task.ID)
	}

	elapsed := time.Duration(0)
	if fresh, err := e.tasks.GetTask(task.ID); err == nil {
		start := taskWindowStart(fresh)
		if !start.IsZero() && time.Now().UTC().After(start) {
			elapsed = time.Since(start)
		}
	}

	markdown := ActivityProgressMarkdown(task.Instruction, elapsed, current, evidence)
	if stepCount > 0 {
		markdown = fmt.Sprintf("<!-- steps:%d -->\n%s", stepCount, markdown)
	}

	e.broker.Emit("task:progress", map[string]interface{}{
		"task_id":          task.ID,
		"conversation_id":  task.ConversationID,
		"status":           string(TaskRunning),
		"duration_ms":      elapsed.Milliseconds(),
		"step_count":       stepCount,
		"current":          current,
		"evidence":         evidence,
		"summary_markdown": markdown,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	})
}

func (e *Engine) toolProgressText(tc managerclient.ToolCall) (string, string) {
	switch tc.Name {
	case "bash":
		command := e.vaultMask.Mask(clippedOneLine(tc.Input["command"], 180))
		lower := strings.ToLower(command)
		current := "Running a shell command."
		switch {
		case strings.Contains(lower, "xterm") && strings.Contains(lower, "codex"):
			current = "Opening Codex in a terminal."
		case strings.Contains(lower, "xterm") && strings.Contains(lower, "claude"):
			current = "Opening Claude Code in a terminal."
		case strings.Contains(lower, "pgrep") && strings.Contains(lower, "codex"):
			current = "Checking whether Codex is running."
		case strings.Contains(lower, "pgrep") && strings.Contains(lower, "claude"):
			current = "Checking whether Claude Code is running."
		case strings.Contains(lower, "xdotool") && strings.Contains(lower, "type"):
			current = "Sending input to the active terminal."
		case strings.Contains(lower, "xdotool") || strings.Contains(lower, "wmctrl"):
			current = "Operating the visible desktop."
		}
		return current, "bash: " + command
	case "computer":
		action := tc.Input["action"]
		switch action {
		case "screenshot":
			return "Checking the screen.", "desktop: screenshot"
		case "type":
			return "Typing into the active window.", "desktop: type"
		case "key":
			return "Pressing a key in the active window.", "desktop: key " + clippedOneLine(tc.Input["text"], 40)
		case "scroll":
			return "Scrolling the desktop.", "desktop: scroll"
		case "left_click", "right_click", "double_click", "middle_click", "left_click_drag", "mouse_move":
			return "Interacting with the desktop.", "desktop: " + action
		default:
			return "Operating the desktop.", "desktop: " + clippedOneLine(action, 60)
		}
	case "str_replace_based_edit_tool", "text_editor", "str_replace_editor":
		return "Editing a file.", "editor: " + e.vaultMask.Mask(clippedOneLine(tc.InputString(), 180))
	default:
		return "Using a tool.", e.vaultMask.Mask(clippedOneLine(tc.InputString(), 180))
	}
}

func clippedOneLine(s string, max int) string {
	s = oneLine(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func (e *Engine) managerModelID() string {
	if mp, ok := e.manager.(ManagerModelProvider); ok {
		if model := strings.TrimSpace(mp.Model()); model != "" {
			return model
		}
	}
	return managerclient.DefaultModelID
}

// agentLoop runs the manager computer-use loop for a single task.
// It sends messages to the manager, processes tool calls (screenshot, shell,
// etc.), applies guardrails, and iterates until the manager produces a final
// text response.
//
// The task-level cancellation source is the taskBudget set up by processTask;
// it cancels ctx when *agent-active* time runs out (waiting on humans is
// paused). This loop just respects ctx — it does not own a wall clock.
func (e *Engine) agentLoop(ctx context.Context, task *Task) (string, error) {
	// Defense-in-depth watchdog: if ctx is cancelled (by the budget, or by
	// a manual stop, or by a parent shutdown) and the main goroutine fails
	// to bail out within 30s, force-fail the task directly in the DB so the
	// dashboard sees a terminal state. Triggers off the cancellation signal,
	// not a wall clock — the budget already handles "agent has worked too
	// long," and this only catches "agent ignored the cancel."
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-watchdogDone:
			return
		case <-ctx.Done():
		}
		select {
		case <-watchdogDone:
			return
		case <-time.After(30 * time.Second):
			errMsg := "task killed by watchdog (agent unresponsive to cancellation)"
			log.Printf("WATCHDOG: %s for task %s", errMsg, task.ID)
			e.tasks.SetError(task.ID, errMsg)
			e.broker.Emit("task:failed", map[string]interface{}{
				"task_id": task.ID,
				"error":   errMsg,
			})
			e.audit.Log(audit.Entry{
				Action:    "task_watchdog_kill",
				Category:  "task",
				TaskID:    task.ID,
				Details:   errMsg,
				RiskLevel: "critical",
			})
		}
	}()

	// Build context.
	memories, _ := e.memory.Search(task.Instruction, 10)
	var memEntries []MemoryEntry
	for _, m := range memories {
		// Exclude activity entries — they are injected via WithActivities
		// with no length truncation. Leaking them through WithMemories would
		// clamp them to MaxMemoryValueLength and produce noise.
		if m.Category == ActivityCategory {
			continue
		}
		memEntries = append(memEntries, MemoryEntry{
			Category: m.Category,
			Key:      m.Key,
			Value:    m.Value,
		})
	}

	msgs, _ := e.tasks.GetMessages(task.ConversationID)

	// Pull the 30 most recent activity notes as the agent's grounded history.
	// WithTaskHistory is intentionally NOT called — activities are strictly
	// richer and including both would duplicate + confuse the model.
	activities := e.fetchRecentActivities(30)

	rules := e.guardrails.ActiveRuleDescriptions()
	vaultNames := e.vault.Names()

	// COMPUTER.md is read from disk on every turn (auto-created with a
	// default template on first access). Cheap fs read; correctness
	// requires we pick up edits the manager just made via str_replace
	// in the same turn-pair, and edits the dashboard pushed via
	// /api/computer-md between turns.
	systemPrompt := NewContextBuilder(e.prompt).
		WithComputerMD(ReadOrCreateComputerMD()).
		WithMemories(memEntries).
		WithMessages(msgs).
		WithActivities(activities).
		WithRules(rules).
		WithVaultNames(vaultNames).
		Build()

	convMsgs := NewContextBuilder("").WithMessages(msgs).BuildMessages()

	// Strip trailing assistant turns from the loaded conversation
	// history. Manager APIs generally expect the next turn to be a user
	// turn, not an assistant prefill. A trailing assistant in our loaded
	// history is always the artifact of a concurrent prior task
	// whose late narration / final-result writes landed AFTER this
	// task's user instruction in the messages table — the timeline is:
	//
	//   user(A) → assistant(A.narration…) → user(B) → assistant(A.late)
	//
	// where Task A was still mid-loop when the user submitted Task B,
	// and Task A's subsequent persists ordered after user(B) by
	// created_at. Those trailing assistants were never logically part
	// of "this task's" conversation pair — they belong to Task A's
	// completed response — and including them in this task's first
	// API request both breaks the contract the API enforces and skews
	// the model's read of where the conversation "left off." The
	// chat UI still shows them; this only affects request shape.
	//
	// managerclient.SendComputerUse also drops trailing assistants as a
	// belt-and-suspenders guard (v0.40.3), but the right place to fix
	// the contract is at the engine layer where we own the construction.
	for len(convMsgs) > 0 && convMsgs[len(convMsgs)-1].Role == "assistant" {
		convMsgs = convMsgs[:len(convMsgs)-1]
	}

	// Convert to manager API messages. Messages with attachments are
	// rendered as structured content blocks (image + text) so the manager can
	// actually see the file; messages without attachments stay as plain
	// strings so the existing (well-trodden) API path is undisturbed.
	var apiMessages []managerclient.Message
	for _, m := range convMsgs {
		apiMessages = append(apiMessages, managerclient.Message{
			Role:    m.Role,
			Content: buildMessageContent(m),
		})
	}
	initMsgCount := len(apiMessages) // track initial messages for context windowing
	modelID := e.managerModelID()

	if shouldAnswerImageAttachmentDirectly(task.Instruction, convMsgs) {
		if directManager, ok := e.manager.(NoToolsManagerAPI); ok {
			callCtx, callCancel := context.WithTimeout(ctx, apiCallTimeout)
			resp, err := directManager.SendNoTools(callCtx, systemPrompt+"\n"+directImageAttachmentPrompt, apiMessages)
			callCancel()
			if err == nil {
				if e.usage != nil {
					if recErr := e.usage.Record(modelID, task.ConversationID, resp.Usage); recErr != nil {
						log.Printf("usage: record failed for direct attachment task %s: %v", task.ID, recErr)
					}
				}
				if text := strings.TrimSpace(resp.TextContent); text != "" {
					return text, nil
				}
			} else {
				log.Printf("manager: direct image attachment path failed for task %s, falling back to tool loop: %v", task.ID, err)
			}
		}
	}

	maxIterations := 300
	for i := 0; i < maxIterations; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		e.broker.Emit("task:thinking", map[string]interface{}{
			"task_id":         task.ID,
			"conversation_id": task.ConversationID,
			"iteration":       i + 1,
		})

		// Per-API-call timeout context.
		callCtx, callCancel := context.WithTimeout(ctx, apiCallTimeout)
		resp, err := e.manager.SendComputerUse(callCtx, systemPrompt, apiMessages)
		callCancel()
		if err != nil {
			return "", fmt.Errorf("manager API error: %w", err)
		}

		// Record token consumption for the Usage panel. A nil usage recorder
		// is the test path; production always wires usage.Store at startup.
		if e.usage != nil {
			if err := e.usage.Record(modelID, task.ConversationID, resp.Usage); err != nil {
				// Usage tracking must never break a task. Log and continue.
				log.Printf("engine: usage.Record(SendComputerUse): %v", err)
			}
		}

		// Check if response is a final text answer (no tool use).
		// Keep this short-circuit: processTask will store the final text via
		// AddMessage and emit task:completed. Do NOT iterate Blocks here or
		// the final answer would double-emit as narration + completion.
		if resp.StopReason == "end_turn" && len(resp.ToolCalls) == 0 {
			finalText := resp.TextContent
			masked := e.vaultMask.Mask(finalText)
			// Empty final response = silent failure. Surface it as an error
			// instead of completing with an empty chat bubble the user
			// can't interpret.
			if len(masked) == 0 {
				return "", fmt.Errorf("agent returned empty response (no text, no tool calls)")
			}
			return masked, nil
		}

		// Iterate blocks in the natural order the manager emitted them. Text
		// blocks (narration) are buffered one-ahead: if the NEXT block is
		// a screenshot tool_use, the buffered text becomes that screenshot's
		// caption rather than a separate bubble. Any other successor (more
		// text, a non-screenshot tool) flushes the buffer as a standalone
		// message. This keeps the "what the agent is doing" sentence
		// attached to the image it describes instead of stranded above it.
		var toolResults []managerclient.ToolResult
		var pendingCaption string
		flushPending := func() {
			if pendingCaption == "" {
				return
			}
			masked := e.vaultMask.Mask(pendingCaption)
			e.tasks.AddMessage(task.ConversationID, "assistant", masked)
			e.broker.Emit("task:message", map[string]interface{}{
				"task_id":   task.ID,
				"content":   masked,
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			})
			pendingCaption = ""
		}
		for _, block := range resp.Blocks {
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				// A second narration block means the first didn't caption a
				// screenshot — flush it as a bubble before buffering this one.
				flushPending()
				pendingCaption = block.Text
			case "tool_use":
				if block.ToolCall == nil {
					continue
				}
				tc := *block.ToolCall
				isScreenshot := tc.Name == "computer" && tc.Input["action"] == "screenshot"
				caption := ""
				if isScreenshot {
					// Peek at the buffered narration but don't consume yet —
					// if the screenshot fails to persist, we fall back to
					// flushing it as a bubble so the user still sees the
					// narration.
					caption = pendingCaption
				} else {
					// Non-screenshot action (click/type/scroll/bash/etc) —
					// buffered narration was about this action; emit it as
					// a bubble since the action itself is invisible.
					flushPending()
				}
				toolResult, skip, screenshotPersisted := e.runToolBlock(ctx, task, tc, caption)
				if skip {
					return "", ctx.Err()
				}
				toolResults = append(toolResults, toolResult)

				if isScreenshot {
					if screenshotPersisted {
						// Caption landed on the image — drop the buffered copy.
						pendingCaption = ""
					} else {
						// Screenshot was blocked / errored / denied. Flush
						// the caption as a standalone narration bubble so
						// the user still sees what the agent was about to do.
						flushPending()
					}
				}
			}
		}
		// Trailing narration with no following tool_use (rare — would only
		// happen if the manager ends a turn with text after a tool call, which
		// the end_turn short-circuit above catches — but handle it defensively).
		flushPending()

		// Build the assistant message and tool results into messages for next iteration.
		apiMessages = append(apiMessages, managerclient.Message{
			Role:        "assistant",
			RawContent:  resp.RawContent,
			ToolResults: nil,
		})

		// Add tool results as a user message.
		apiMessages = append(apiMessages, managerclient.Message{
			Role:        "user",
			ToolResults: toolResults,
		})

		// Token-threshold compaction. Instead of fixed-message-count
		// windowing (which silently drops history the agent needs and
		// causes the "scroll forever" amnesia bug), we let history grow
		// until the model's input is close to crossing into the >200k
		// premium-pricing tier, then collapse old turns into a structured
		// summary. Short tasks pay zero compaction tax; long tasks pay
		// it exactly once per ~140k tokens of growth.
		if e.compactor != nil && ShouldCompact(resp.Usage) {
			log.Printf(
				"engine: triggering compaction (input=%d, total=%d, threshold=%d) for task %s",
				resp.Usage.InputTokens, resp.Usage.TotalInputTokens(), CompactionThresholdTokens, task.ID,
			)
			compacted, cerr := e.compactor.Compact(ctx, task.ID, task.ConversationID, apiMessages, initMsgCount)
			if cerr != nil {
				// Degrade gracefully — log and continue with full history.
				// Next iteration may cost more, but the task isn't broken.
				log.Printf("engine: compaction failed for task %s: %v — continuing uncompacted", task.ID, cerr)
				e.audit.Log(audit.Entry{
					Action:    "compaction_failed",
					Category:  "agent",
					TaskID:    task.ID,
					Details:   cerr.Error(),
					RiskLevel: "low",
				})
			} else {
				before := len(apiMessages)
				apiMessages = compacted
				log.Printf("engine: compaction reduced messages %d → %d for task %s", before, len(apiMessages), task.ID)
				e.audit.Log(audit.Entry{
					Action:   "compaction_applied",
					Category: "agent",
					TaskID:   task.ID,
					Details:  fmt.Sprintf("%d → %d messages", before, len(apiMessages)),
				})
			}
		}
	}

	return "", fmt.Errorf("agent loop exceeded maximum iterations (%d)", maxIterations)
}

// maskToolCall returns a copy of the tool call whose input values have all
// been run through the vault masker, so any plaintext secret that the
// manager passed inline (or that would be resolved into the value before
// execution) is replaced with its [SECRET_NAME] marker. The returned copy
// is used only for audit/stream/approval rendering — never for execution.
// The original tc is left untouched; the copy gets a fresh map so mutating
// it can never alias back into the live call.
func (e *Engine) maskToolCall(tc managerclient.ToolCall) managerclient.ToolCall {
	if len(tc.Input) == 0 {
		return tc
	}
	masked := make(map[string]string, len(tc.Input))
	for k, v := range tc.Input {
		masked[k] = e.vaultMask.Mask(v)
	}
	tc.Input = masked
	return tc
}

// runToolBlock handles guardrails, optional user confirmation, and tool
// execution for a single tool_use block. Returns a ToolResult ready to send
// back to the manager on the next turn. The skip return is true only when the
// task's context was cancelled while waiting for user confirmation — the
// caller should abort the agentLoop immediately in that case.
//
// caption is the narration text the manager emitted immediately before this
// tool call (empty if none). For a screenshot action it becomes the
// image's caption in chat; for everything else it is ignored (the caller
// already flushed it as its own message bubble).
//
// The third return (screenshotPersisted) is true only when the tool was a
// computer.screenshot AND we successfully wrote a screenshot message to the
// conversation. The caller uses it to decide whether the caption was
// consumed by the image or needs to be flushed as a fallback bubble.
func (e *Engine) runToolBlock(ctx context.Context, task *Task, tc managerclient.ToolCall, caption string) (managerclient.ToolResult, bool, bool) {
	// request_credentials is a pure user-interaction tool: it opens a form
	// in chat, the user fills it, values write straight to the vault. No
	// shell command ever runs, so the guardrail layer has nothing to
	// evaluate — short-circuit before it fires.
	if tc.Name == "request_credentials" {
		res, sk := e.handleCredentialRequest(ctx, task, tc)
		return res, sk, false
	}

	// Masked view of the tool call for everything that gets PERSISTED,
	// STREAMED, or sent OFF-MACHINE: audit Details, SSE payloads, and the
	// approval card the user sees and that lands in the durable chat
	// history. Tool output is already scrubbed with e.vaultMask; input was
	// not, so a command like `curl -H "Authorization: Bearer $API_KEY"`
	// (resolved at execution time) would otherwise leak the plaintext
	// secret into the audit log, the SSE stream, and the platform audit
	// sink. tcMasked replaces each input value with its [SECRET_NAME]
	// marker; maskedInput is its rendered one-liner.
	//
	// The guardrail engine and the risk classifier below still receive the
	// REAL tc — they must pattern-match the actual command to judge
	// safety, and they neither persist nor transmit it. executeTool also
	// uses the real tc so the resolved secret reaches the child process.
	tcMasked := e.maskToolCall(tc)
	maskedInput := tcMasked.InputString()

	decision := e.guardrails.Evaluate(guardrails.Action{
		Type:    tc.Name,
		Command: tc.InputString(),
		TaskID:  task.ID,
	})

	if decision.Action == guardrails.Block {
		e.audit.Log(audit.Entry{
			Action:    "action_blocked",
			Category:  "guardrail",
			TaskID:    task.ID,
			Details:   fmt.Sprintf("Blocked %s: %s. Reason: %s", tc.Name, maskedInput, decision.Reason),
			RiskLevel: "high",
		})
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   fmt.Sprintf("Action blocked by safety policy: %s", decision.Reason),
			IsError:   true,
		}, false, false
	}

	if decision.Action == guardrails.Confirm {
		// Auto-review pass. The regex-based guardrails are deliberately
		// blunt — they catch many patterns with the same rule. The
		// classifier reads the actual command in context
		// and returns one of three verdicts:
		//   - approve:  silently safe, skip the card entirely
		//   - escalate: routine human approval (neutral framing)
		//   - deny:     classifier-recommended deny (red framing,
		//               friction override on the card)
		// Failures (timeout, network, malformed reply) default to
		// escalate — the classifier can only relax/intensify, never
		// soften a hard System Block.
		classifierReason := ""
		classifierVerdict := guardrails.VerdictEscalate
		if e.classifier != nil {
			// Build a short transcript of the recent conversation for
			// the classifier so it can judge intent-alignment, not just
			// pattern-match the command. The customer might be
			// explicitly testing something, or asked the agent to clean
			// up a specific path — the context changes what "safe"
			// means. Cap the transcript to keep the classifier input small.
			chatCtx := e.recentChatForClassifier(task.ConversationID, 12)
			result := e.classifier.Classify(ctx, guardrails.Action{
				Type:    tc.Name,
				Command: tc.InputString(),
				TaskID:  task.ID,
			}, decision, task.Instruction, chatCtx)
			classifierVerdict = result.Verdict
			classifierReason = result.Reason

			if result.Verdict == guardrails.VerdictApprove {
				e.audit.Log(audit.Entry{
					Action:    "action_auto_approved",
					Category:  "guardrail",
					TaskID:    task.ID,
					Details:   fmt.Sprintf("Auto-approved %s: %s. Rule: %s. Auto-review: %s", tc.Name, maskedInput, decision.Rule, result.Reason),
					RiskLevel: "low",
				})
				// Surface auto-approvals inline in the chat so the
				// customer can see what the system did on their behalf
				// (transparency principle). The dashboard renders this
				// as a small pill below the agent's narration.
				e.broker.Emit("task:auto_approved", map[string]interface{}{
					"task_id":   task.ID,
					"tool":      tc.Name,
					"title":     humanTitle(tc.Name, maskedInput, decision.Rule),
					"reason":    result.Reason,
					"command":   maskedInput,
					"timestamp": time.Now().UTC().Format(time.RFC3339),
				})
				// Fall through to the normal execute path. Action runs
				// as if the guardrail had returned Allow.
				goto autoApproved
			}
			// Use the classifier's specific reason as the card text for
			// both escalate and deny paths — it's concrete where the
			// regex's pattern-match reason is generic.
			if result.Reason != "" {
				decision.Reason = result.Reason
			}
		}

		// Build the card from the MASKED tool call: its Command field is
		// shown to the user and persisted into the durable chat history,
		// so it must not carry a plaintext secret the manager passed
		// inline. pickFriction/humanTitle operate on the masked command,
		// which is fine — device paths and system-tree targets are not
		// secrets and survive masking unchanged.
		var approvalForUser ApprovalPayload
		if classifierVerdict == guardrails.VerdictDeny {
			approvalForUser = buildSoftDenyApproval(tcMasked, decision, classifierReason)
		} else {
			approvalForUser = buildApproval(tcMasked, decision)
		}
		// JSON is the durable form — rehydrate parses it into structured
		// fields on the client. Plain-text is the fallback for older
		// clients that don't know about the structured shape yet.
		initialApprovalJSON := approvalForUser.JSON()
		question := approvalForUser.PlainText()

		// Drain stale signals BEFORE flipping the task to
		// waiting_for_input. Any submit/redirect that legitimately
		// targets this card can only fire after the status flip
		// (SubmitInput / SubmitCredentials / NotifyNewTaskInConversation
		// all gate on TaskWaitingForInput), so the drain only ever
		// removes leftovers from a previous card. Order matters: if we
		// drained after the flip, a redirect that arrives in the
		// nanosecond window between flip and drain would be eaten and
		// the select below would block forever.
		select {
		case <-e.inputCh:
		default:
		}
		select {
		case <-e.redirectCh:
		default:
		}

		// Persist the approval prompt as a conversation message so it
		// survives page reloads and lives in the durable chat history,
		// not just on the transient task.result field. Capture the id so
		// we can flip the card to expired on timeout, ctx-cancel, or
		// cross-conversation redirect (markApprovalCardExpired). The id
		// also rides inside the payload so SSE replay rebuilds the
		// rendered card without a side-channel lookup.
		approvalMsgID, err := e.tasks.AddTypedMessageReturningID(task.ConversationID, "assistant", initialApprovalJSON, "approval", nil)
		if err != nil {
			log.Printf("error persisting approval prompt for task %s: %v", task.ID, err)
		}
		approvalForUser.MessageID = approvalMsgID
		approvalJSON := approvalForUser.JSON()
		if approvalMsgID != "" {
			if err := e.tasks.UpdateMessageContent(approvalMsgID, approvalJSON); err != nil {
				log.Printf("approval: failed to embed message_id for task %s: %v", task.ID, err)
			}
			e.mu.Lock()
			e.pendingApprovalMsgs[task.ID] = pendingApprovalMsg{MessageID: approvalMsgID, Payload: approvalForUser}
			e.mu.Unlock()
		}
		// Mirror onto task.result only after the durable card has been
		// prepared. Tests and clients observe waiting_for_input as the
		// signal that the corresponding event/card is ready.
		e.tasks.SetWaitingForInput(task.ID, approvalJSON)
		// Clean up the registry no matter how we exit so a future
		// approval card for the same task doesn't read stale state.
		defer func() {
			e.mu.Lock()
			delete(e.pendingApprovalMsgs, task.ID)
			e.mu.Unlock()
		}()
		e.broker.Emit("task:waiting", map[string]interface{}{
			"task_id":  task.ID,
			"question": question,
			"approval": approvalForUser,
		})
		e.emitTaskProgress(task, "Waiting for your approval before continuing.", approvalForUser.Title, false)
		e.audit.Log(audit.Entry{
			Action:    "action_confirmation_requested",
			Category:  "guardrail",
			TaskID:    task.ID,
			Details:   fmt.Sprintf("Requested confirmation for %s: %s. Reason: %s", tc.Name, maskedInput, decision.Reason),
			RiskLevel: "medium",
		})

		// Pause the active-time budget for the whole approval wait —
		// the user thinking is not agent work and should not consume the
		// task's compute budget.
		e.pauseBudget()
		var userInput string
		select {
		case <-ctx.Done():
			e.resumeBudget()
			e.markApprovalCardExpired(approvalMsgID, approvalForUser, "task ended")
			e.broker.Emit("task:approval_expired", map[string]interface{}{
				"task_id":    task.ID,
				"message_id": approvalMsgID,
				"reason":     "task ended",
			})
			return managerclient.ToolResult{}, true, false
		case userInput = <-e.inputCh:
			e.resumeBudget()
		case <-e.redirectCh:
			// Customer started a new task in a different conversation
			// while this approval card was open. Hard-stop the task —
			// see the credential-card redirect arm for the rationale.
			// Returning a tool_error with "withdraw" copy is too soft:
			// the model can decide to retry a different path and pin
			// the engine to an abandoned conversation. Cancelling the
			// ctx + skip=true lets processTask transition cleanly to
			// Cancelled and the engine loop picks up the new task.
			e.resumeBudget()
			e.markApprovalCardExpired(approvalMsgID, approvalForUser, "customer redirected attention to another conversation")
			e.broker.Emit("task:approval_expired", map[string]interface{}{
				"task_id":    task.ID,
				"message_id": approvalMsgID,
				"reason":     "redirected",
			})
			e.audit.Log(audit.Entry{
				Action:    "action_confirmation_redirected",
				Category:  "guardrail",
				TaskID:    task.ID,
				Details:   fmt.Sprintf("Approval card for %s superseded by new task in another conversation", tc.Name),
				RiskLevel: "low",
			})
			e.tasks.AddMessage(task.ConversationID, "assistant", "Closing this task — you started a new conversation, so I've stopped here. Come back if you want to resume; the approval request is no longer open.")
			e.mu.Lock()
			if e.cancel != nil {
				e.cancel()
			}
			e.mu.Unlock()
			return managerclient.ToolResult{}, true, false
		case <-time.After(HumanWaitTimeout):
			e.resumeBudget()
			e.tasks.UpdateStatus(task.ID, TaskRunning)
			e.markApprovalCardExpired(approvalMsgID, approvalForUser, "no response within 24h")
			e.broker.Emit("task:approval_expired", map[string]interface{}{
				"task_id":    task.ID,
				"message_id": approvalMsgID,
				"reason":     "no response within 24h",
			})
			e.audit.Log(audit.Entry{
				Action:    "action_confirmation_timeout",
				Category:  "guardrail",
				TaskID:    task.ID,
				Details:   fmt.Sprintf("Confirmation timed out after %s for %s: %s", HumanWaitTimeout, tc.Name, maskedInput),
				RiskLevel: "medium",
			})
			return managerclient.ToolResult{
				ToolUseID: tc.ID,
				Content:   "The customer did not respond to the approval card within 24 hours. Do not silently retry the same action — if it is still needed, raise the question in a fresh message and let them direct.",
				IsError:   true,
			}, false, false
		}

		e.tasks.UpdateStatus(task.ID, TaskRunning)
		e.broker.Emit("task:resumed", map[string]string{"task_id": task.ID})

		approved := userInput == "yes" || userInput == "y"
		// Flip the persisted approval card to its receipt state so a page
		// reload paints the resolved card, not the still-actionable form.
		// Mirror of the credential_request "stored" flip.
		if approvalMsgID != "" {
			receipt := approvalForUser
			if approved {
				receipt.Resolved = "approved"
			} else {
				receipt.Resolved = "denied"
			}
			if updated := receipt.JSON(); updated != "" {
				if err := e.tasks.UpdateMessageContent(approvalMsgID, updated); err != nil {
					log.Printf("approval: failed to update persisted card for task %s: %v", task.ID, err)
				}
			}
		}
		// Distinguish a normal escalation approval from a Soft Deny
		// override. Soft Deny overrides are higher-signal audit events:
		// "the classifier said no, the customer said yes anyway." Worth
		// finding in the audit log at a glance, and (eventually)
		// feeding into eval data on whether the deny rule was too broad.
		isSoftDenyOverride := approvalForUser.Severity == SeveritySoftDeny
		if approved {
			if isSoftDenyOverride {
				e.audit.Log(audit.Entry{
					Action:    "action_overridden_with_friction",
					Category:  "guardrail",
					TaskID:    task.ID,
					Details:   fmt.Sprintf("Customer overrode soft-deny (friction=%s) for %s: %s. Classifier reason: %s", approvalForUser.Friction, tc.Name, maskedInput, approvalForUser.Reason),
					RiskLevel: "high",
				})
			} else {
				e.audit.Log(audit.Entry{
					Action:    "action_approved",
					Category:  "guardrail",
					TaskID:    task.ID,
					Details:   fmt.Sprintf("User approved %s: %s", tc.Name, maskedInput),
					RiskLevel: "medium",
				})
			}
		} else {
			action := "action_denied"
			if isSoftDenyOverride {
				action = "action_soft_deny_upheld"
			}
			e.audit.Log(audit.Entry{
				Action:    action,
				Category:  "guardrail",
				TaskID:    task.ID,
				Details:   fmt.Sprintf("User declined %s: %s (input: %q)", tc.Name, maskedInput, userInput),
				RiskLevel: "medium",
			})
			return managerclient.ToolResult{
				ToolUseID: tc.ID,
				Content:   "Action denied by user.",
				IsError:   true,
			}, false, false
		}
	}

autoApproved:
	current, evidence := e.toolProgressText(tc)
	e.emitTaskProgress(task, current, evidence, true)
	result, execErr := e.executeTool(ctx, tc)

	e.audit.Log(audit.Entry{
		Action:   "tool_executed",
		Category: "agent",
		TaskID:   task.ID,
		Details:  fmt.Sprintf("%s: %s", tc.Name, maskedInput),
	})

	if execErr != nil {
		masked := e.vaultMask.Mask(fmt.Sprintf("Error: %s", execErr.Error()))
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   masked,
			IsError:   true,
		}, false, false
	}

	masked := e.vaultMask.Mask(result.Content)
	// Only show explicit screenshot actions to the user. Auto-captures after
	// clicks/moves are internal tool results for the manager, not chat content.
	screenshotPersisted := false
	if result.IsImage && tc.Name == "computer" && tc.Input["action"] == "screenshot" {
		maskedCaption := e.vaultMask.Mask(caption)
		e.broker.Emit("task:screenshot", map[string]interface{}{
			"task_id": task.ID,
			"image":   result.Content,
			"caption": maskedCaption,
		})
		if err := e.tasks.AddScreenshotMessage(task.ConversationID, maskedCaption, result.Content); err != nil {
			log.Printf("screenshot: persist failed for task %s: %v", task.ID, err)
		} else {
			screenshotPersisted = true
		}
	}

	return managerclient.ToolResult{
		ToolUseID:     tc.ID,
		Content:       masked,
		IsError:       false,
		IsBase64Image: result.IsImage,
	}, false, screenshotPersisted
}

// handleCredentialRequest is the request_credentials short-circuit from
// runToolBlock. It validates the tool input, persists a credential_request
// bubble in chat, suspends the task, and blocks until the user submits or
// dismisses the card. Returns the tool result to feed back to the manager.
//
// The "skip" return is true only when the task's context is cancelled while
// we're blocked — the caller aborts the agent loop in that case.
func (e *Engine) handleCredentialRequest(ctx context.Context, task *Task, tc managerclient.ToolCall) (managerclient.ToolResult, bool) {
	payload, err := ParseCredentialRequestInput(tc.Input)
	if err != nil {
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   fmt.Sprintf("request_credentials rejected: %s. Fix the arguments and try again.", err.Error()),
			IsError:   true,
		}, false
	}
	payload.ToolUseID = tc.ID
	payload.TaskID = task.ID

	// Two-step insert so the persisted payload (and the mirror on
	// task.result) both carry MessageID. The id has to go INSIDE the
	// payload — that's what SSE replay and rehydration read to wire
	// the card up after a reconnect or a daemon restart, without
	// needing a side-channel lookup into engine memory.
	initialJSON := payload.JSON()
	if initialJSON == "" {
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   "request_credentials: failed to serialise the request.",
			IsError:   true,
		}, false
	}
	msgID, err := e.tasks.AddTypedMessageReturningID(task.ConversationID, "assistant", initialJSON, "credential_request", nil)
	if err != nil {
		log.Printf("credential_request: persist failed for task %s: %v", task.ID, err)
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   "request_credentials: failed to persist the request card.",
			IsError:   true,
		}, false
	}

	payload.MessageID = msgID
	payloadJSON := payload.JSON()
	if err := e.tasks.UpdateMessageContent(msgID, payloadJSON); err != nil {
		// Not fatal — the UI can still render from the msgID-less
		// content. Log and continue; the SSE event carries the id
		// so live clients are unaffected.
		log.Printf("credential_request: failed to embed message_id for task %s: %v", task.ID, err)
	}

	e.mu.Lock()
	e.pendingCredMsgs[task.ID] = pendingCredMsg{MessageID: msgID, Payload: payload}
	e.mu.Unlock()
	// Clean up the map entry no matter how we exit — success, cancel, or
	// context cancellation.
	defer func() {
		e.mu.Lock()
		delete(e.pendingCredMsgs, task.ID)
		e.mu.Unlock()
	}()

	// Drain stale signals BEFORE flipping the task to waiting_for_input.
	// SubmitCredentials and NotifyNewTaskInConversation both gate on
	// status == waiting_for_input, so the drain only ever removes
	// leftovers from a previous card. If we drained AFTER the flip a
	// legitimate signal arriving in the nanosecond gap between flip and
	// drain could be eaten and the select below would block forever
	// (the exact race that broke the redirect tests).
	select {
	case <-e.credentialsInputCh:
	default:
	}
	select {
	case <-e.redirectCh:
	default:
	}

	// Mirror the payload onto task.result so the SSE replay path in
	// handleStream can rebuild the card for a reconnecting client,
	// matching what the approval flow does with its payload.
	e.tasks.SetWaitingForInput(task.ID, payloadJSON)

	e.broker.Emit("task:credentials_requested", map[string]interface{}{
		"task_id":    task.ID,
		"message_id": msgID,
		"payload":    payload,
	})

	e.audit.Log(audit.Entry{
		Action:    "credentials_requested",
		Category:  "vault",
		TaskID:    task.ID,
		Details:   fmt.Sprintf("%s (%d fields)", payload.Title, len(payload.Fields)),
		RiskLevel: "low",
	})

	// Pause the task's active-time budget for the whole human-wait window.
	// A customer stepping away from a credential prompt is not the agent
	// burning compute — the budget should not charge them for it.
	e.pauseBudget()
	defer e.resumeBudget()

	var resp CredentialResponse
	select {
	case <-ctx.Done():
		// Task ended while the card was open (manual cancel, budget
		// exhaustion of pre-paused time, parent shutdown). Stamp the
		// persisted card as expired so a returning client sees the
		// resolved state instead of a still-actionable form.
		e.markCredentialCardExpired(msgID, payload, "task ended")
		e.broker.Emit("task:credentials_expired", map[string]interface{}{
			"task_id":    task.ID,
			"message_id": msgID,
			"reason":     "task ended",
		})
		return managerclient.ToolResult{}, true
	case resp = <-e.credentialsInputCh:
	case <-e.redirectCh:
		// Customer started a new task in a different conversation while
		// this credential card was open. Don't keep the manager loop
		// running on a task the customer has plainly walked away from —
		// the old prompt was "withdraw, do not retry" and the model
		// reinterpreted that as "try a different sign-in path", which
		// kept the engine pinned to the abandoned conversation and the
		// new chat queued. Hard-stop instead: expire the card, post a
		// final assistant note explaining what happened, cancel the
		// task's ctx so processTask transitions it to Cancelled, and
		// return skip=true so the agent loop bails immediately.
		e.markCredentialCardExpired(msgID, payload, "customer redirected attention to another conversation")
		e.broker.Emit("task:credentials_expired", map[string]interface{}{
			"task_id":    task.ID,
			"message_id": msgID,
			"reason":     "redirected",
		})
		e.audit.Log(audit.Entry{
			Action:    "credentials_request_redirected",
			Category:  "vault",
			TaskID:    task.ID,
			Details:   payload.Title,
			RiskLevel: "low",
		})
		e.tasks.AddMessage(task.ConversationID, "assistant", "Closing this task — you started a new conversation, so I've stopped here. Come back if you want to resume; the credential request is no longer open.")
		e.mu.Lock()
		if e.cancel != nil {
			e.cancel()
		}
		e.mu.Unlock()
		return managerclient.ToolResult{}, true
	case <-time.After(HumanWaitTimeout):
		e.tasks.UpdateStatus(task.ID, TaskRunning)
		e.markCredentialCardExpired(msgID, payload, "no response within 24h")
		e.broker.Emit("task:credentials_expired", map[string]interface{}{
			"task_id":    task.ID,
			"message_id": msgID,
			"reason":     "no response within 24h",
		})
		e.audit.Log(audit.Entry{
			Action:    "credentials_request_timeout",
			Category:  "vault",
			TaskID:    task.ID,
			Details:   payload.Title,
			RiskLevel: "medium",
		})
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   "The customer did not respond to the credential request within 24 hours. Move on — do not silently retry the same ask. If the credentials are still needed, surface that clearly in a follow-up message.",
			IsError:   true,
		}, false
	}

	e.tasks.UpdateStatus(task.ID, TaskRunning)
	e.broker.Emit("task:resumed", map[string]string{"task_id": task.ID})

	if resp.Cancelled {
		e.audit.Log(audit.Entry{
			Action:    "credentials_request_cancelled",
			Category:  "vault",
			TaskID:    task.ID,
			Details:   payload.Title,
			RiskLevel: "low",
		})
		return managerclient.ToolResult{
			ToolUseID: tc.ID,
			Content:   "The customer dismissed the credential request without filling it in. Do not re-ask unless the customer raises the need again.",
			IsError:   true,
		}, false
	}

	storedNames := make([]string, len(resp.Values))
	for i, v := range resp.Values {
		storedNames[i] = v.Name
	}

	summary := map[string]interface{}{
		"ok":     true,
		"stored": storedNames,
		"note":   "Values are in the encrypted vault. Reference them as $NAME (or ${NAME}) in bash and when typing into forms. Never echo the raw value.",
	}
	summaryJSON, _ := json.Marshal(summary)
	return managerclient.ToolResult{
		ToolUseID: tc.ID,
		Content:   string(summaryJSON),
		IsError:   false,
	}, false
}

// ToolExecResult holds the output of a tool execution.
type ToolExecResult struct {
	Content string
	IsImage bool
}

func (e *Engine) executeTool(ctx context.Context, tc managerclient.ToolCall) (*ToolExecResult, error) {
	switch tc.Name {
	case "computer":
		return e.executeComputerTool(ctx, tc)
	case "bash":
		return e.executeBashTool(ctx, tc)
	case "str_replace_based_edit_tool":
		// Canonical name for text_editor_20250728. We also accept the
		// legacy names ("text_editor", "str_replace_editor") for safety
		// — the dispatch was a no-op for them previously, so making them
		// route here strictly improves behaviour.
		return e.executeTextEditorTool(ctx, tc)
	case "text_editor", "str_replace_editor":
		return e.executeTextEditorTool(ctx, tc)
	default:
		return nil, fmt.Errorf("unknown tool: %s", tc.Name)
	}
}

func (e *Engine) executeComputerTool(ctx context.Context, tc managerclient.ToolCall) (*ToolExecResult, error) {
	if e.screenshot == nil || e.computer == nil {
		return nil, fmt.Errorf("computer tools unavailable: display not wired")
	}
	action := tc.Input["action"]

	switch action {
	case "screenshot":
		imgData, err := e.screenshot.CaptureBase64(ctx)
		if err != nil {
			return nil, err
		}
		return &ToolExecResult{Content: imgData, IsImage: true}, nil

	case "mouse_move":
		x, y := tc.InputCoords()
		err := e.computer.MouseMove(x, y)
		if err != nil {
			return nil, err
		}
		// Take screenshot after action.
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Mouse moved"}, nil

	case "left_click":
		x, y := tc.InputCoords()
		if x != 0 || y != 0 {
			e.computer.MouseMove(x, y)
		}
		err := e.computer.LeftClick()
		if err != nil {
			return nil, err
		}
		time.Sleep(300 * time.Millisecond)
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Left click performed"}, nil

	case "right_click":
		x, y := tc.InputCoords()
		if x != 0 || y != 0 {
			e.computer.MouseMove(x, y)
		}
		err := e.computer.RightClick()
		if err != nil {
			return nil, err
		}
		time.Sleep(300 * time.Millisecond)
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Right click performed"}, nil

	case "double_click":
		x, y := tc.InputCoords()
		if x != 0 || y != 0 {
			e.computer.MouseMove(x, y)
		}
		err := e.computer.DoubleClick()
		if err != nil {
			return nil, err
		}
		time.Sleep(300 * time.Millisecond)
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Double click performed"}, nil

	case "middle_click":
		x, y := tc.InputCoords()
		if x != 0 || y != 0 {
			e.computer.MouseMove(x, y)
		}
		err := e.computer.MiddleClick()
		if err != nil {
			return nil, err
		}
		return &ToolExecResult{Content: "Middle click performed"}, nil

	case "left_click_drag":
		x, y := tc.InputCoords()
		err := e.computer.LeftClickDrag(x, y)
		if err != nil {
			return nil, err
		}
		return &ToolExecResult{Content: "Drag performed"}, nil

	case "type":
		text, _ := tc.Input["text"]
		// Resolve vault references. If a real vault entry was injected,
		// do not return the automatic post-type screenshot: pixels cannot be
		// vault-masked, and the target field might visibly contain the secret.
		typedVaultSecret := e.referencesKnownVaultSecret(text)
		resolved := text
		if e.vault != nil {
			resolved = e.vault.ResolveReferences(text)
		}
		err := e.computer.TypeText(resolved)
		if err != nil {
			return nil, err
		}
		if typedVaultSecret {
			return &ToolExecResult{Content: "Text typed. Automatic screenshot suppressed because the typed text included a vault secret reference."}, nil
		}
		time.Sleep(200 * time.Millisecond)
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Text typed"}, nil

	case "key":
		key, _ := tc.Input["text"]
		err := e.computer.KeyPress(key)
		if err != nil {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Key pressed"}, nil

	case "scroll":
		x, y := tc.InputCoords()
		direction, _ := tc.Input["direction"]
		amount := 3 // default scroll amount
		err := e.computer.Scroll(x, y, direction, amount)
		if err != nil {
			return nil, err
		}
		time.Sleep(300 * time.Millisecond)
		imgData, _ := e.screenshot.CaptureBase64(ctx)
		if imgData != "" {
			return &ToolExecResult{Content: imgData, IsImage: true}, nil
		}
		return &ToolExecResult{Content: "Scrolled"}, nil

	case "cursor_position":
		x, y, err := e.computer.GetCursorPosition()
		if err != nil {
			return nil, err
		}
		return &ToolExecResult{Content: fmt.Sprintf("Cursor at (%d, %d)", x, y)}, nil

	default:
		return nil, fmt.Errorf("unknown computer action: %s", action)
	}
}

func (e *Engine) referencesKnownVaultSecret(text string) bool {
	if e.vault == nil {
		return false
	}
	for _, name := range vault.ExtractReferences(text) {
		if _, ok := e.vault.Get(name); ok {
			return true
		}
	}
	return false
}

// SetAgentShell installs the long-lived agent-shell sandbox as the
// engine's bash surface (Phase 2). Called once at startup on Linux.
func (e *Engine) SetAgentShell(s ShellExecutor) { e.agentShell = s }

// sh returns the active bash surface: the agent-shell sandbox if wired,
// else host bash. Every bash/text_editor path goes through this so the
// agent's tool behavior is identical whether sandboxed or not.
func (e *Engine) sh() ShellExecutor {
	if e.agentShell != nil {
		return e.agentShell
	}
	return e.shell
}

func (e *Engine) executeBashTool(ctx context.Context, tc managerclient.ToolCall) (*ToolExecResult, error) {
	command, _ := tc.Input["command"]
	// Vault secrets are delivered through the child's environment, not
	// substituted into the command string — that keeps plaintext out of
	// argv (ps auxe, /proc/<pid>/cmdline). The command runs verbatim;
	// bash expands $NAME from the injected env at exec time. (Phase 0a;
	// no env-var prefix per Decision D1.)
	refs := vault.ExtractReferences(command)
	secretEnv := e.vault.BuildEnv(refs)
	output, err := e.sh().ExecuteWithEnv(ctx, command, secretEnv)
	if err != nil {
		return &ToolExecResult{Content: output}, err
	}
	return &ToolExecResult{Content: output}, nil
}

// buildMessageContent renders a stored ConversationMessage into the shape
// the manager API expects. Plain messages become a bare string (the fast
// path, unchanged behavior). Messages with attachments become a content
// block array of {text...}, {image...} — the model gets the prompt, image, and a
// short index of where each file lives on disk.
//
// Image inlining is bounded: at most maxImagesPerMessage images, and any
// single image over maxInlineImageBytes is referenced by path only. The
// agent can always open larger files from bash; sending them through
// the manager is a bandwidth optimization, not a requirement.
func buildMessageContent(m ConversationMessage) interface{} {
	if len(m.Attachments) == 0 {
		return m.Content
	}

	imageBlocks := make([]map[string]interface{}, 0, maxImagesPerMessage)
	imagesInlined := 0
	var imageSummaryLines []string
	var fileSummaryLines []string
	var skippedLines []string

	for _, att := range m.Attachments {
		displayName := att.Original
		if displayName == "" {
			displayName = att.Name
		}

		if supportedImageMIMEs[strings.ToLower(att.MIME)] && imagesInlined < maxImagesPerMessage {
			if att.Size > 0 && att.Size > maxInlineImageBytes {
				skippedLines = append(skippedLines, fmt.Sprintf("- %s (too large to show inline — read from disk if you need to see it): %s", displayName, att.Path))
				continue
			}
			data, err := os.ReadFile(att.Path)
			if err != nil {
				skippedLines = append(skippedLines, fmt.Sprintf("- %s (could not read from disk): %s", displayName, att.Path))
				continue
			}
			if len(data) > maxInlineImageBytes {
				skippedLines = append(skippedLines, fmt.Sprintf("- %s (too large to show inline — read from disk if you need to see it): %s", displayName, att.Path))
				continue
			}
			imageBlocks = append(imageBlocks, map[string]interface{}{
				"type": "image",
				"source": map[string]string{
					"type":       "base64",
					"media_type": normalizeImageMIME(att.MIME),
					"data":       base64.StdEncoding.EncodeToString(data),
				},
			})
			imageSummaryLines = append(imageSummaryLines, fmt.Sprintf("- %s — %s", displayName, att.Path))
			imagesInlined++
			continue
		}

		fileSummaryLines = append(fileSummaryLines, fmt.Sprintf("- %s (%s, %s) — %s", displayName, att.MIME, humanSize(att.Size), att.Path))
	}

	// Assemble the text block: user's typed message, then a compact index
	// of everything they attached.
	var textParts []string
	userText := strings.TrimSpace(m.Content)
	if userText != "" {
		textParts = append(textParts, userText)
	}
	if len(imageSummaryLines) > 0 {
		textParts = append(textParts, "Images attached (inline image bytes are included in this same message; inspect them directly. If the image does not render, use the saved disk path before answering. Do not tell the customer the attachment content is unavailable unless both inline and disk inspection fail):\n"+strings.Join(imageSummaryLines, "\n"))
	}
	if len(fileSummaryLines) > 0 {
		textParts = append(textParts, "Files attached (read from disk with bash / text_editor):\n"+strings.Join(fileSummaryLines, "\n"))
	}
	if len(skippedLines) > 0 {
		textParts = append(textParts, "Images too large to inline (still saved):\n"+strings.Join(skippedLines, "\n"))
	}
	if len(textParts) == 0 {
		textParts = []string{"(the user sent a message with no text)"}
	}
	blocks := make([]map[string]interface{}, 0, len(imageBlocks)+1)
	blocks = append(blocks, map[string]interface{}{
		"type": "text",
		"text": strings.Join(textParts, "\n\n"),
	})
	blocks = append(blocks, imageBlocks...)

	// If, for whatever reason, no image blocks ended up in the list, fall
	// back to a plain string — simpler payload, identical semantics.
	if imagesInlined == 0 && len(skippedLines) == 0 {
		return blocks[len(blocks)-1]["text"]
	}
	return blocks
}

func shouldAnswerImageAttachmentDirectly(instruction string, messages []ConversationMessage) bool {
	msg, ok := latestUserMessage(messages)
	if !ok || !messageHasSupportedImage(msg) {
		return false
	}

	text := strings.ToLower(strings.TrimSpace(instruction + " " + msg.Content))
	if text == "" {
		return true
	}

	actionTerms := []string{
		"click", "type into", "type this", "enter ", "open ", "go to", "navigate", "run ", "execute",
		"install", "download", "upload", "save", "create", "build", "make ",
		"edit", "change", "fix", "launch", "start", "stop", "move", "delete",
		"send", "post", "publish", "deploy", "fill ",
	}
	for _, term := range actionTerms {
		if strings.Contains(text, term) {
			return false
		}
	}

	visualAskTerms := []string{
		"describe", "what do you see", "what's in", "what is in", "what is this",
		"what's this", "look at", "analyze", "analyse", "read", "ocr",
		"transcribe", "summarize", "summarise", "explain", "extract",
		"review", "identify",
	}
	for _, term := range visualAskTerms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func latestUserMessage(messages []ConversationMessage) (ConversationMessage, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i], true
		}
	}
	return ConversationMessage{}, false
}

func messageHasSupportedImage(msg ConversationMessage) bool {
	for _, att := range msg.Attachments {
		if supportedImageMIMEs[strings.ToLower(att.MIME)] {
			return true
		}
	}
	return false
}

// normalizeImageMIME returns the MIME type the manager expects. Our uploads
// sometimes carry "image/jpg"; the API wants "image/jpeg".
func normalizeImageMIME(mt string) string {
	m := strings.ToLower(strings.TrimSpace(mt))
	if m == "image/jpg" {
		return "image/jpeg"
	}
	return m
}

// humanSize formats a byte count for agent-facing output. Big enough
// to read, small enough to skim.
func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "0 B"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/1024/1024/1024)
	}
}

func (e *Engine) executeTextEditorTool(ctx context.Context, tc managerclient.ToolCall) (*ToolExecResult, error) {
	command, _ := tc.Input["command"]
	path, _ := tc.Input["path"]

	switch command {
	case "view":
		output, err := e.sh().Execute(ctx, fmt.Sprintf("cat -n %q", path))
		if err != nil {
			return nil, err
		}
		return &ToolExecResult{Content: output}, nil

	case "create":
		content, _ := tc.Input["file_text"]
		resolved := e.vault.ResolveReferences(content)
		// Secret-resolved content is fed via stdin, never argv — keeps
		// plaintext out of ps/​/proc/<pid>/cmdline (Phase 0a follow-up).
		// Python still handles file creation (avoids heredoc injection);
		// only the non-secret path stays in the argv script.
		script := fmt.Sprintf(`python3 -c "
import os, sys
path = %q
content = sys.stdin.read()
os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
with open(path, 'w') as f:
    f.write(content)
print('File created: ' + path)
"`, path)
		output, err := e.sh().ExecuteInteractive(ctx, script, resolved)
		if err != nil {
			return nil, fmt.Errorf("creating file: %w (output: %s)", err, output)
		}
		return &ToolExecResult{Content: fmt.Sprintf("File created: %s", path)}, nil

	case "str_replace":
		oldStr, _ := tc.Input["old_str"]
		newStr, _ := tc.Input["new_str"]
		resolvedNew := e.vault.ResolveReferences(newStr)
		// Only new_str is vault-resolved (can carry secrets) — it goes via
		// stdin, never argv. old_str/path are non-secret matching text and
		// stay in the script (Phase 0a follow-up).
		script := fmt.Sprintf(`python3 -c "
import sys
path = %q
old = %q
new = sys.stdin.read()
with open(path) as f:
    content = f.read()
if old not in content:
    print('ERROR: old_str not found in file')
    sys.exit(1)
count = content.count(old)
if count > 1:
    print(f'ERROR: old_str found {count} times, must be unique')
    sys.exit(1)
content = content.replace(old, new, 1)
with open(path, 'w') as f:
    f.write(content)
print('Replacement done')
"`, path, oldStr)
		output, err := e.sh().ExecuteInteractive(ctx, script, resolvedNew)
		if err != nil {
			return nil, fmt.Errorf("str_replace: %w (output: %s)", err, output)
		}
		return &ToolExecResult{Content: output}, nil

	case "insert":
		insertLine, _ := tc.Input["insert_line"]
		newStr, _ := tc.Input["new_str"]
		resolvedNew := e.vault.ResolveReferences(newStr)
		// new_str is vault-resolved (can carry secrets) — via stdin, never
		// argv. path/line are non-secret (Phase 0a follow-up).
		script := fmt.Sprintf(`python3 -c "
import sys
path = %q
line_num = int(%q)
new_text = sys.stdin.read()
with open(path) as f:
    lines = f.readlines()
lines.insert(line_num, new_text + '\n')
with open(path, 'w') as f:
    f.writelines(lines)
print('Insert done')
"`, path, insertLine)
		output, err := e.sh().ExecuteInteractive(ctx, script, resolvedNew)
		if err != nil {
			return nil, fmt.Errorf("insert: %w (output: %s)", err, output)
		}
		return &ToolExecResult{Content: output}, nil

	default:
		return nil, fmt.Errorf("unknown text_editor command: %s", command)
	}
}
