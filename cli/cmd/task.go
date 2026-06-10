// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/exit"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagTaskNoWait         bool
	flagTaskWait           bool
	flagTaskTimeout        time.Duration
	flagTaskConversationID string
	flagTaskIdempotencyKey string
	flagTaskListStatus     string
	flagTaskListSince      string
	flagTaskListLimit      int
	flagStreamFollowFinal  bool
)

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Submit and manage tasks",
	Long:  "Send tasks to your VibeCraft computer and track their progress.",
}

var taskSubmitCmd = &cobra.Command{
	Use:   "submit [message]",
	Short: "Submit a new task",
	Long: `Submit a task to your VibeCraft computer.

By default, --wait is implied: the CLI blocks until the task reaches a terminal
state (completed / failed / waiting_for_input / cancelled) and emits a TaskData
envelope describing the final state.

Use --no-wait to submit and return immediately with just the task ID.

Message reading:
  vibecraft task submit "do the thing"           # positional arg
  vibecraft task submit -                        # read prompt from stdin
  echo "do the thing" | vibecraft task submit -  # piped`,
	Args: cobra.ArbitraryArgs,
	RunE: runTaskSubmit,
}

var taskRespondCmd = &cobra.Command{
	Use:   "respond <id> [message]",
	Short: "Respond to a task waiting for input",
	Long: `Send a reply to a task that ended in waiting_for_input.

Message reading mirrors 'task submit':
  vibecraft task respond <id> "yes, go ahead"
  vibecraft task respond <id> -                   # read from stdin
  echo "yes, go" | vibecraft task respond <id> -

By default the CLI waits for the task to reach the next terminal state and
emits a TaskData envelope. Pass --no-wait to return immediately.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runTaskRespond,
}

var taskMessagesCmd = &cobra.Command{
	Use:   "messages <id>",
	Short: "Print a task's conversation transcript",
	Long: `Emit the full conversation transcript for the task as a JSON array of
{role, content, task_id, created_at, ...} messages.`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskMessages,
}

var flagTaskCredsCancel bool

var taskCredentialsCmd = &cobra.Command{
	Use:   "credentials <id>",
	Short: "Answer a task's credential request",
	Long: `When a task hits a sign-in wall it pauses with a request_credentials
card (status waiting_for_input). Supply the values as a JSON object on
stdin — one key per requested field:

  echo '{"GITHUB_TOKEN":"ghp_…","NPM_TOKEN":"npm_…"}' | vibecraft task credentials <id>

Read the requested field names first with 'vibecraft task messages <id>'.
Values go on stdin (never argv) so they stay out of 'ps' and shell history.

  vibecraft task credentials <id> --cancel    # dismiss the request instead`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskCredentials,
}

var taskStreamCmd = &cobra.Command{
	Use:   "stream <id>",
	Short: "Stream events for a task",
	Long: `Open an SSE connection to the daemon and emit one JSON Lines event per
line. Stops at the task's terminal state by default.

Pass --follow-final to keep streaming until the connection drops.`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskStream,
}

var taskWaitCmd = &cobra.Command{
	Use:   "wait <id>",
	Short: "Wait for an already-submitted task to finish",
	Long: `Block until the given task reaches a terminal state, then emit a TaskData
envelope. Exit code reflects task outcome (see vibecraft docs).`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskWait,
}

var taskListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent tasks",
	Long: `List the most recent tasks on this machine. Filters:

  --status <name>    only tasks with this status
  --since  <rfc3339> only tasks created at-or-after this timestamp
  --limit  <n>       cap the result count`,
	RunE: runTaskList,
}

var taskGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get a task's state",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskGet,
}

var taskStatusAlias = &cobra.Command{
	Use:    "status <id>",
	Short:  "Alias for 'get'",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE:   runTaskGet,
}

var taskCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Cancel a running task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskCancel,
}

func init() {
	rootCmd.AddCommand(taskCmd)

	taskSubmitCmd.Flags().BoolVar(&flagTaskNoWait, "no-wait", false, "Submit and return immediately")
	taskSubmitCmd.Flags().BoolVar(&flagTaskWait, "wait", true, "Block until terminal state (default)")
	taskSubmitCmd.Flags().DurationVar(&flagTaskTimeout, "timeout", 0, "Max time to wait (0 = no timeout)")
	taskSubmitCmd.Flags().StringVar(&flagTaskConversationID, "conversation-id", "", "Continue an existing conversation")
	taskSubmitCmd.Flags().StringVar(&flagTaskIdempotencyKey, "idempotency-key", "", "Dedup key for safe retries")

	taskRespondCmd.Flags().BoolVar(&flagTaskNoWait, "no-wait", false, "Respond and return immediately")
	taskRespondCmd.Flags().BoolVar(&flagTaskWait, "wait", true, "Block until next terminal state (default)")
	taskRespondCmd.Flags().DurationVar(&flagTaskTimeout, "timeout", 0, "Max time to wait (0 = no timeout)")

	taskWaitCmd.Flags().DurationVar(&flagTaskTimeout, "timeout", 0, "Max time to wait (0 = no timeout)")

	taskStreamCmd.Flags().BoolVar(&flagStreamFollowFinal, "follow-final", false, "Keep streaming past task's terminal state")

	taskListCmd.Flags().StringVar(&flagTaskListStatus, "status", "", "Filter by status (queued/running/completed/failed/...)")
	taskListCmd.Flags().StringVar(&flagTaskListSince, "since", "", "Only tasks at-or-after this RFC3339 timestamp")
	taskListCmd.Flags().IntVar(&flagTaskListLimit, "limit", 0, "Cap result count (0 = no cap)")

	taskCredentialsCmd.Flags().BoolVar(&flagTaskCredsCancel, "cancel", false, "Dismiss the credential request instead of answering it")

	taskCmd.AddCommand(taskSubmitCmd)
	taskCmd.AddCommand(taskRespondCmd)
	taskCmd.AddCommand(taskMessagesCmd)
	taskCmd.AddCommand(taskCredentialsCmd)
	taskCmd.AddCommand(taskStreamCmd)
	taskCmd.AddCommand(taskWaitCmd)
	taskCmd.AddCommand(taskListCmd)
	taskCmd.AddCommand(taskGetCmd)
	taskCmd.AddCommand(taskStatusAlias)
	taskCmd.AddCommand(taskCancelCmd)

	// `vibecraft task "..."` shortcut for `vibecraft task submit "..."`.
	taskCmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return runTaskSubmit(cmd, args)
	}
	taskCmd.Args = cobra.ArbitraryArgs
}

func runTaskSubmit(cmd *cobra.Command, args []string) error {
	if flagTaskNoWait && cmd.Flags().Changed("wait") && flagTaskWait {
		return schema.Newf(schema.CodeValidationError, "--wait and --no-wait are mutually exclusive")
	}

	message, err := readMessageFromArgsOrStdin(args)
	if err != nil {
		return err
	}
	if message == "" {
		return schema.Newf(schema.CodeValidationError, "task message cannot be empty")
	}

	// Fan-out path. --machine all / list / glob → JSON Lines, one per machine.
	if isFanoutSelector(flagMachineID) {
		cfg, err := loadConfig()
		if err != nil {
			return schema.Newf(schema.CodeInternal, "%s", err.Error())
		}
		machines, err := matchMachines(cfg, flagMachineID)
		if err != nil {
			return err
		}
		return runTaskSubmitFanout(machines, message)
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	// A11: auto-generate a per-invocation idempotency key when none is
	// supplied. Retrying the same `vibecraft task submit` against a
	// network blip should NOT produce two tasks. The daemon de-dupes by
	// this key for ~10 minutes. Users can override with --idempotency-key
	// to opt in to their own naming.
	idempotencyKey := flagTaskIdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = newIdempotencyKey()
	}

	resp, err := c.SubmitTaskWith(client.SubmitTaskRequest{
		Instruction:    message,
		ConversationID: flagTaskConversationID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return mapDaemonError(err, "submitting task")
	}

	if flagTaskNoWait {
		return output.Emit(schema.TaskSubmitData{
			ID:             resp.ID,
			Status:         resp.Status,
			ConversationID: resp.ConversationID,
			CreatedAt:      resp.CreatedAt,
		})
	}

	return waitForTask(c, resp.ID)
}

func runTaskRespond(cmd *cobra.Command, args []string) error {
	if flagTaskNoWait && cmd.Flags().Changed("wait") && flagTaskWait {
		return schema.Newf(schema.CodeValidationError, "--wait and --no-wait are mutually exclusive")
	}

	id := args[0]
	rest := args[1:]
	message, err := readMessageFromArgsOrStdin(rest)
	if err != nil {
		return err
	}
	if message == "" {
		return schema.Newf(schema.CodeValidationError, "response message cannot be empty")
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	if err := c.RespondToTask(id, message); err != nil {
		return mapDaemonError(err, "responding to task")
	}

	if flagTaskNoWait {
		return output.Emit(schema.TaskRespondData{ID: id, Status: "input_received"})
	}

	return waitForTask(c, id)
}

func runTaskMessages(cmd *cobra.Command, args []string) error {
	id := args[0]

	c, err := newClient()
	if err != nil {
		return err
	}

	conv, err := c.GetTaskMessages(id)
	if err != nil {
		return mapDaemonError(err, "fetching messages")
	}

	out := schema.TaskMessagesData{
		ID:       id,
		Messages: make([]schema.MessageData, 0, len(conv.Messages)),
	}
	if cid, ok := conv.Conversation["id"].(string); ok {
		out.ConversationID = cid
	}
	for _, m := range conv.Messages {
		out.Messages = append(out.Messages, schema.MessageData{
			ID:        m.ID,
			Role:      m.Role,
			Content:   m.Content,
			TaskID:    m.TaskID,
			CreatedAt: m.CreatedAt,
		})
	}
	return output.Emit(out)
}

func runTaskCredentials(cmd *cobra.Command, args []string) error {
	id := args[0]
	c, err := newClient()
	if err != nil {
		return err
	}

	if flagTaskCredsCancel {
		if err := c.SubmitCredentials(id, nil, true); err != nil {
			return mapDaemonError(err, "dismissing credential request")
		}
		return output.Emit(schema.TaskCredentialsData{ID: id, Status: "dismissed"})
	}

	// Values come from a JSON object on stdin: {"NAME":"value",...}.
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return schema.Newf(schema.CodeInternal, "reading stdin: %s", err.Error())
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return schema.Newf(schema.CodeValidationError,
			"no credentials on stdin").
			WithHint(`pipe a JSON object: echo '{"NAME":"value"}' | vibecraft task credentials <id>`)
	}
	var pairs map[string]string
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return schema.Newf(schema.CodeValidationError,
			"stdin must be a JSON object of {\"NAME\":\"value\"}: %s", err.Error())
	}
	if len(pairs) == 0 {
		return schema.Newf(schema.CodeValidationError, "no credentials provided")
	}

	creds := make([]client.CredentialValue, 0, len(pairs))
	names := make([]string, 0, len(pairs))
	for name, value := range pairs {
		creds = append(creds, client.CredentialValue{Name: name, Value: value})
		names = append(names, name)
	}
	sort.Strings(names)

	if err := c.SubmitCredentials(id, creds, false); err != nil {
		return mapDaemonError(err, "submitting credentials")
	}
	return output.Emit(schema.TaskCredentialsData{
		ID:     id,
		Status: "submitted",
		Names:  names,
	})
}

func runTaskStream(cmd *cobra.Command, args []string) error {
	id := args[0]
	c, err := newClient()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	terminate := errors.New("stream: terminal task event reached")

	// Track whether we actually observed a terminal event. The daemon's
	// SSE connection can drop (idle proxy, daemon restart, network blip)
	// before the terminal event is sent; parseSSE treats a clean EOF as a
	// nil return, so without this flag a truncated stream looks identical
	// to a completed one and the CLI would exit 0 — an agent then reads
	// truncation as success. See the close-handling block after Stream().
	sawTerminal := false
	// Did the task park on an approval / credential card (waiting_for_input)?
	// The daemon emits task:waiting / task:credentials_requested for this, with
	// NO `status` field, so neither the terminal switch nor the status fallback
	// used to catch it — the SSE stayed open on keepalives FOREVER, blocking an
	// agent on the very pause that needs it to act. We treat it as terminal for
	// the stream and map it to exit 3, mirroring `task wait`.
	sawWaiting := false

	onEvent := func(ev client.StreamEvent) error {
		// Forward the event as a JSON Lines line. We pass through the
		// daemon's payload verbatim; the schema's job here is just to add
		// the {v, event, ts} envelope wrapper.
		fields := map[string]any{}
		if len(ev.Data) > 0 {
			var generic map[string]any
			if err := json.Unmarshal(ev.Data, &generic); err == nil {
				for k, v := range generic {
					if k == "v" || k == "event" || k == "ts" {
						continue
					}
					fields[k] = v
				}
			} else {
				// Non-JSON payload — keep as raw string under "raw".
				fields["raw"] = ev.Raw
			}
		}
		eventType := ev.Type
		if eventType == "" {
			eventType = schema.EventMessage
		}
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		if err := output.EmitEvent(eventType, ts, fields); err != nil {
			return err
		}

		// Decide whether this event terminates the stream.
		if !flagStreamFollowFinal {
			// The daemon emits per-task lifecycle events with type strings
			// like "task:completed", "task:failed", "task:cancelled" — these
			// are the load-bearing terminal signals on the wire.
			switch eventType {
			case "task:completed", "task:failed", "task:cancelled":
				sawTerminal = true
				return terminate
			case "task:waiting", "task:credentials_requested":
				// Parked on an approval / credential card. Terminal for the
				// stream — otherwise it hangs on keepalives until the operator
				// acts. Resolved to exit 3 after the stream ends.
				sawTerminal = true
				sawWaiting = true
				return terminate
			case schema.EventFinal:
				sawTerminal = true
				return terminate
			}
			// Belt-and-suspenders: some payloads also carry a `status` field
			// with the same terminal string. Catch either form.
			if statusStr, ok := fields["status"].(string); ok {
				switch statusStr {
				case "completed", "failed", "cancelled":
					sawTerminal = true
					return terminate
				case "waiting_for_input":
					sawTerminal = true
					sawWaiting = true
					return terminate
				}
			}
		}
		return nil
	}

	streamErr := c.Stream(ctx, client.StreamOpts{TaskID: id}, onEvent)
	if streamErr != nil && !errors.Is(streamErr, terminate) {
		// Don't surface ctx.Canceled as an error; emit a clean stream end.
		if errors.Is(streamErr, context.Canceled) {
			return output.EmitEvent(schema.EventEnd, time.Now().UTC().Format(time.RFC3339Nano), map[string]any{"reason": "interrupted"})
		}
		return mapDaemonError(streamErr, "streaming")
	}

	// Clean stream end (Stream returned nil, or our terminate sentinel).
	// In --follow-final mode the caller asked to stream until the
	// connection drops, so a clean end IS the success condition.
	if flagStreamFollowFinal {
		return nil
	}

	// The task parked on an approval / credential card. Mirror `task wait`:
	// resolve the authoritative status and map it to its exit code
	// (waiting_for_input -> 3) so an agent driving an approval-gated task
	// unblocks and can act, instead of the stream hanging forever.
	if sawWaiting {
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		if task, getErr := c.GetTask(id); getErr == nil {
			if code := exit.ForTaskStatus(task.Status); code != exit.OK {
				_ = output.EmitEvent(schema.EventFinal, ts, map[string]any{"status": task.Status})
				return &exit.OutcomeError{Code: code}
			}
		}
		// Couldn't confirm, or the task already moved past waiting — fall
		// through to a clean end rather than inventing an exit code.
	}

	// Default mode: a stream that ended WITHOUT a terminal event is a
	// dropped connection, not a finished task. Returning nil here would
	// make an agent read truncation as success. Fall back to the
	// authoritative task state so the exit code reflects the real
	// outcome; if the task is still in flight, surface a distinct,
	// non-zero "connection_closed" end instead of a false success.
	if !sawTerminal {
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		task, getErr := c.GetTask(id)
		if getErr != nil {
			// Couldn't recover the terminal state — emit the end marker and
			// fail loudly rather than exit 0 on a half-read stream.
			_ = output.EmitEvent(schema.EventEnd, ts, map[string]any{"reason": "connection_closed"})
			return mapDaemonError(getErr, "stream closed before a terminal event; fetching task state")
		}
		if code := exit.ForTaskStatus(task.Status); code != exit.OK {
			// Task actually reached a terminal state we missed on the wire.
			_ = output.EmitEvent(schema.EventFinal, ts, map[string]any{"status": task.Status})
			return &exit.OutcomeError{Code: code}
		}
		if task.Status == "completed" {
			// Genuinely done; the terminal event was just lost in transit.
			_ = output.EmitEvent(schema.EventFinal, ts, map[string]any{"status": task.Status})
			return nil
		}
		// Still queued/running: the connection dropped on a live task.
		// Distinct non-zero exit so the caller never mistakes this for done.
		_ = output.EmitEvent(schema.EventEnd, ts, map[string]any{"reason": "connection_closed", "status": task.Status})
		return schema.Newf(schema.CodeMachineUnreachable,
			"stream closed before task %s reached a terminal state (last status: %s)", id, task.Status).
			WithHint("re-attach with 'vibecraft task stream " + id + "' or poll with 'vibecraft task wait " + id + "'")
	}
	return nil
}

func runTaskWait(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return waitForTask(c, args[0])
}

func runTaskList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}

	resp, err := c.ListTasksWith(client.ListTasksOpts{
		Status: flagTaskListStatus,
		Since:  flagTaskListSince,
		Limit:  flagTaskListLimit,
	})
	if err != nil {
		return mapDaemonError(err, "listing tasks")
	}

	out := schema.TaskListData{Tasks: make([]schema.TaskData, 0, len(resp.Tasks))}
	for _, t := range resp.Tasks {
		out.Tasks = append(out.Tasks, toTaskData(&t))
	}
	return output.Emit(out)
}

func runTaskGet(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	task, err := c.GetTask(args[0])
	if err != nil {
		return mapDaemonError(err, "fetching task")
	}
	return output.Emit(toTaskData(task))
}

func runTaskCancel(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.CancelTask(args[0]); err != nil {
		return mapDaemonError(err, "cancelling task")
	}
	return output.Emit(schema.TaskCancelData{ID: args[0], Status: "cancelled"})
}

// waitForTask polls until the task reaches a terminal state, then emits
// the TaskData envelope and returns the appropriate OutcomeError.
func waitForTask(c *client.Client, id string) error {
	parentCtx := context.Background()
	if flagTaskTimeout > 0 {
		var cancel context.CancelFunc
		parentCtx, cancel = context.WithTimeout(parentCtx, flagTaskTimeout)
		defer cancel()
	}
	ctx, cancel := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	task, err := c.PollTask(ctx, id, 2*time.Second, nil)
	if err != nil {
		// Ctrl-C cancels — post a cancel to the daemon so the worker
		// actually stops, not just our local loop.
		if errors.Is(err, context.Canceled) {
			_ = c.CancelTask(id)
			latest, _ := c.GetTask(id)
			if latest != nil {
				if emitErr := output.Emit(toTaskData(latest)); emitErr != nil {
					return emitErr
				}
				return &exit.OutcomeError{Code: exit.TaskCancelled}
			}
			return schema.Newf(schema.CodeCancelled, "cancelled by user")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return schema.Newf(schema.CodeTimeout, "wait exceeded --timeout")
		}
		return mapDaemonError(err, "polling task")
	}

	if err := output.Emit(toTaskData(task)); err != nil {
		return err
	}

	if code := exit.ForTaskStatus(task.Status); code != exit.OK {
		return &exit.OutcomeError{Code: code}
	}
	return nil
}

// readMessageFromArgsOrStdin returns a single message string from either
// positional args (joined with spaces) or stdin if a single "-" arg is
// passed.
func readMessageFromArgsOrStdin(args []string) (string, error) {
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", schema.Newf(schema.CodeInternal, "reading message from stdin: %s", err.Error())
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return strings.TrimSpace(strings.Join(args, " ")), nil
}

// runTaskSubmitFanout dispatches `task submit` against each machine
// concurrently. When --no-wait it returns the queued task ID per
// machine; otherwise it blocks on each machine's terminal state.
func runTaskSubmitFanout(machines []string, message string) error {
	wait := !flagTaskNoWait
	timeout := flagTaskTimeout
	return runFanout(machines, func(id string, c *client.Client) (any, int, error) {
		resp, err := c.SubmitTaskWith(client.SubmitTaskRequest{
			Instruction:    message,
			ConversationID: flagTaskConversationID,
			IdempotencyKey: newIdempotencyKey(),
		})
		if err != nil {
			return nil, 0, mapDaemonError(err, "submitting task")
		}
		if !wait {
			return schema.TaskSubmitData{
				ID:             resp.ID,
				Status:         resp.Status,
				ConversationID: resp.ConversationID,
				CreatedAt:      resp.CreatedAt,
			}, exit.OK, nil
		}
		parentCtx := context.Background()
		if timeout > 0 {
			var cancel context.CancelFunc
			parentCtx, cancel = context.WithTimeout(parentCtx, timeout)
			defer cancel()
		}
		task, err := c.PollTask(parentCtx, resp.ID, 2*time.Second, nil)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, exit.CLIError, schema.Newf(schema.CodeTimeout, "wait exceeded --timeout")
			}
			return nil, 0, mapDaemonError(err, "polling task")
		}
		code := exit.ForTaskStatus(task.Status)
		return toTaskData(task), code, nil
	})
}

// newIdempotencyKey returns a fresh 128-bit hex string. crypto/rand
// avoids needing the uuid dep just for this; the daemon stores it as an
// opaque string anyway.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "k-" + time.Now().UTC().Format("20060102T150405.000")
	}
	return hex.EncodeToString(b[:])
}

func toTaskData(t *client.Task) schema.TaskData {
	if t == nil {
		return schema.TaskData{}
	}
	return schema.TaskData{
		ID:             t.ID,
		Status:         t.Status,
		Instruction:    t.Instruction,
		Result:         t.Result,
		Error:          t.Error,
		ConversationID: t.ConversationID,
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
	}
}
