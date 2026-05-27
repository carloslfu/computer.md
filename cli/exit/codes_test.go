// SPDX-License-Identifier: Apache-2.0

package exit

import (
	"errors"
	"testing"

	"github.com/carloslfu/computer.md/cli/schema"
)

func TestFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, OK},
		{"outcome failed", &OutcomeError{Code: TaskFailed}, TaskFailed},
		{"outcome waiting", &OutcomeError{Code: TaskNeedsInput}, TaskNeedsInput},
		{"outcome cancelled", &OutcomeError{Code: TaskCancelled}, TaskCancelled},
		{"schema error", schema.Newf(schema.CodeAuthRequired, "x"), CLIError},
		{"plain error", errors.New("boom"), CLIError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromError(tc.err); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFromError_WrappedOutcome(t *testing.T) {
	// errors.As must unwrap a wrapped OutcomeError so callers can
	// add context (e.g. fmt.Errorf) without changing the exit code.
	wrapped := errorsJoin(&OutcomeError{Code: TaskFailed}, errors.New("ctx"))
	if got := FromError(wrapped); got != TaskFailed {
		t.Errorf("wrapped: got %d, want %d", got, TaskFailed)
	}
}

func TestForTaskStatus(t *testing.T) {
	cases := map[string]int{
		"completed":         OK,
		"failed":            TaskFailed,
		"waiting_for_input": TaskNeedsInput,
		"cancelled":         TaskCancelled,
		"running":           OK,
		"queued":            OK,
		"":                  OK,
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			if got := ForTaskStatus(status); got != want {
				t.Errorf("status=%q: got %d, want %d", status, got, want)
			}
		})
	}
}

// errorsJoin is a tiny shim because errors.Join (1.20+) returns a
// multi-error; we just want a wrapped-error chain for errors.As.
func errorsJoin(inner, _ error) error {
	return &wrap{inner: inner}
}

type wrap struct{ inner error }

func (w *wrap) Error() string { return w.inner.Error() }
func (w *wrap) Unwrap() error { return w.inner }
