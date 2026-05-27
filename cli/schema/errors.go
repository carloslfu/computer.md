// SPDX-License-Identifier: Apache-2.0

package schema

import "fmt"

// Error is the structured error shape emitted on stderr when a CLI
// invocation fails (network, auth, validation, daemon 4xx/5xx).
//
// `code` is a stable, machine-readable string from the catalog below.
// Agents branch on it. `message` is a one-line human summary. `hint` is
// an optional next-step suggestion ("Run `vibecraft auth login`."). All
// three are safe to surface to humans in --text mode.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Error implements the `error` interface so a *schema.Error can flow
// through Cobra's RunE return path unchanged.
func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (hint: %s)", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Newf builds a *schema.Error with a printf-style message.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint returns a copy of e with the hint set.
func (e *Error) WithHint(hint string) *Error {
	cp := *e
	cp.Hint = hint
	return &cp
}

// Error code catalog. Agents branch on these strings; keep them stable.
// Add new codes here rather than inventing them inline at call sites.
const (
	// Auth + identity.
	CodeAuthRequired  = "auth_required"  // no credentials available
	CodeAuthInvalid   = "auth_invalid"   // credentials rejected (401)
	CodeAuthForbidden = "auth_forbidden" // credentials valid but lack permission (403)

	// Reachability + machine state.
	CodeMachineUnreachable = "machine_unreachable" // network error talking to daemon
	CodeMachineNotActive   = "machine_not_active"  // exists but provisioning/stopped/failed
	CodeMachineNotFound    = "machine_not_found"   // unknown machine id

	// Task lifecycle.
	CodeTaskNotFound    = "task_not_found"
	CodeTaskNotTerminal = "task_not_terminal" // operation requires terminal state
	CodeConversationNotFound = "conversation_not_found"

	// File I/O.
	CodePathInvalid     = "path_invalid"     // outside /home/vibecraft or traversal
	CodePathNotFound    = "path_not_found"
	CodePathIsDirectory = "path_is_directory"
	CodePathIsFile      = "path_is_file"
	CodeFileTooLarge    = "file_too_large"

	// Request-level.
	CodeValidationError    = "validation_error"    // bad flags / missing args
	CodeIdempotencyConflict = "idempotency_conflict"
	CodeRateLimited        = "rate_limited" // 429 from daemon
	CodeTimeout            = "timeout"
	CodeCancelled          = "cancelled" // user SIGINT / context cancel

	// Server-side catchall.
	CodeServerError = "server_error" // 5xx from daemon
	CodeInternal    = "internal_error" // CLI itself misbehaved

	// Wire-version mismatch between CLI and daemon (A15 / G11).
	CodeSchemaUnsupported = "schema_unsupported"

	// A critical, irreversible action (e.g. terminating a machine) was
	// invoked without --confirm. The error message states exactly what
	// would happen; re-run with --confirm to proceed.
	CodeConfirmationRequired = "confirmation_required"

	// An account-level limit blocks the action (e.g. provisioning a
	// machine when the plan is at its machine cap). The fix is a human
	// decision — upgrade the plan — not something the CLI does.
	CodeLimitReached = "limit_reached"
)
