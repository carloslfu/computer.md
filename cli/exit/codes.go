// SPDX-License-Identifier: Apache-2.0

// Package exit defines the CLI's process exit code contract and the
// sentinel error types used to signal task outcomes back to main().
//
// Exit codes are part of the wire contract — agents branch on them.
// Stability matters more than expressiveness; resist the urge to grow
// the catalog.
//
//	0  Success — CLI succeeded, task (if any) completed normally
//	1  CLI error — flag, network, auth, validation, daemon 5xx
//	2  Task failed — CLI succeeded but the task itself ended in `failed`
//	3  Task needs input — task ended in `waiting_for_input`; respond with `task respond`
//	4  Task cancelled — task ended in `cancelled`
package exit

import (
	"errors"
	"fmt"

	"github.com/carloslfu/computer.md/cli/schema"
)

// Process exit codes. Stable.
const (
	OK             = 0
	CLIError       = 1
	TaskFailed     = 2
	TaskNeedsInput = 3
	TaskCancelled  = 4
)

// OutcomeError signals a non-zero exit code for a task-outcome reason
// (failed / waiting_for_input / cancelled) WITHOUT triggering the
// error-envelope emission path. Commands that observe a non-success
// task outcome:
//
//  1. Emit the success envelope describing the final task state via output.Emit
//  2. Return &OutcomeError{Code: <one of TaskFailed | TaskNeedsInput | TaskCancelled>}
//
// The output writer sees an OutcomeError and skips the error envelope
// (output was already written). main() unwraps it and exits with Code.
type OutcomeError struct {
	Code int
}

func (e *OutcomeError) Error() string {
	return fmt.Sprintf("task outcome exit code %d", e.Code)
}

// FromError maps an error returned from a Cobra RunE into a process
// exit code. The mapping:
//
//   - nil                  → OK (0)
//   - *OutcomeError        → its Code field (2 / 3 / 4)
//   - *schema.Error        → CLIError (1) — these are CLI-level failures
//   - any other error      → CLIError (1)
//
// Note that *schema.Error always maps to 1, regardless of the error
// code. The error code distinguishes failure reason for the agent; the
// process exit code distinguishes failure CLASS (CLI vs. task outcome).
func FromError(err error) int {
	if err == nil {
		return OK
	}
	var oe *OutcomeError
	if errors.As(err, &oe) {
		return oe.Code
	}
	var se *schema.Error
	if errors.As(err, &se) {
		return CLIError
	}
	return CLIError
}

// ForTaskStatus returns the exit code that corresponds to a terminal
// task status string from the daemon. Non-terminal statuses ("queued",
// "running") return OK — callers should not invoke this until the task
// has reached a terminal state.
func ForTaskStatus(status string) int {
	switch status {
	case "completed":
		return OK
	case "failed":
		return TaskFailed
	case "waiting_for_input":
		return TaskNeedsInput
	case "cancelled":
		return TaskCancelled
	default:
		return OK
	}
}
