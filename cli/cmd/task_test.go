// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/cli/exit"
	"github.com/carloslfu/computer.md/cli/output"
)

// taskDaemon answers /api/task POST and the GET status loop with the
// scripted body. The body is invoked for each GET so a test can advance
// the task through states.
func taskDaemon(t *testing.T, submitBody string, statusBodies []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var hits int
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(submitBody))
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/task/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		idx := hits
		if idx >= len(statusBodies) {
			idx = len(statusBodies) - 1
		}
		hits++
		_, _ = w.Write([]byte(statusBodies[idx]))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestTaskSubmit_Wait_Completed_EmitsTaskDataAndExit0(t *testing.T) {
	srv := taskDaemon(t,
		`{"id":"t1","status":"queued","conversation_id":"c1","created_at":"2026-05-21T10:00:00Z"}`,
		[]string{
			`{"id":"t1","status":"completed","result":"the answer is 42","conversation_id":"c1","instruction":"go"}`,
		},
	)
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)

	// Reset flags between subtests.
	flagTaskNoWait = false
	flagTaskWait = true

	out, err := captureStdout(t, func() error {
		return runTaskSubmit(taskSubmitCmd, []string{"go"})
	})
	if err != nil {
		t.Fatalf("runTaskSubmit: %v", err)
	}

	var env map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); jerr != nil {
		t.Fatalf("envelope did not parse: %v\nout=%q", jerr, out)
	}
	if env["ok"] != true {
		t.Errorf("ok: got %v, want true", env["ok"])
	}
	data, _ := env["data"].(map[string]any)
	if data == nil || data["status"] != "completed" {
		t.Errorf("data.status: got %v, want completed", data)
	}
	if data["result"] != "the answer is 42" {
		t.Errorf("data.result: got %v", data["result"])
	}
}

func TestTaskSubmit_Wait_Failed_EmitsTaskDataAndExit2(t *testing.T) {
	srv := taskDaemon(t,
		`{"id":"t1","status":"queued","conversation_id":"c1"}`,
		[]string{
			`{"id":"t1","status":"failed","error_message":"out of credits","instruction":"go"}`,
		},
	)
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)
	flagTaskNoWait = false
	flagTaskWait = true

	out, runErr := captureStdout(t, func() error {
		return runTaskSubmit(taskSubmitCmd, []string{"go"})
	})

	if runErr == nil {
		t.Fatalf("expected OutcomeError, got nil")
	}
	var oe *exit.OutcomeError
	if !errors.As(runErr, &oe) {
		t.Fatalf("expected *OutcomeError, got %T: %v", runErr, runErr)
	}
	if oe.Code != exit.TaskFailed {
		t.Errorf("OutcomeError.Code: got %d, want %d", oe.Code, exit.TaskFailed)
	}

	// Success envelope should still have been written first.
	var env map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); jerr != nil {
		t.Fatalf("envelope did not parse: %v\nout=%q", jerr, out)
	}
	if env["ok"] != true {
		t.Errorf("ok: got %v, want true (CLI succeeded; task failed)", env["ok"])
	}
}

func TestTaskSubmit_NoWait_EmitsTaskSubmitData(t *testing.T) {
	srv := taskDaemon(t,
		`{"id":"t1","status":"queued","conversation_id":"c1","created_at":"2026-05-21T10:00:00Z"}`,
		[]string{},
	)
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)
	flagTaskNoWait = true
	flagTaskWait = false

	out, err := captureStdout(t, func() error {
		return runTaskSubmit(taskSubmitCmd, []string{"go"})
	})
	if err != nil {
		t.Fatalf("runTaskSubmit: %v", err)
	}
	var env map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); jerr != nil {
		t.Fatalf("envelope did not parse: %v\nout=%q", jerr, out)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil || data["status"] != "queued" {
		t.Errorf("data: got %v, want status=queued", data)
	}
	// No `result` / `error` should be on the no-wait envelope.
	if _, has := data["result"]; has {
		t.Errorf("no-wait envelope should not have 'result'")
	}

	// Reset for sibling tests.
	flagTaskNoWait = false
	flagTaskWait = true
}

// resetVersionHandshake clears the once-per-process version handshake
// cache so a test's fresh httptest server is probed again. Without this,
// the first newClient() in the package run pins the result and later
// tests against a different server skip the probe.
func resetVersionHandshake() {
	versionHandshakeMu.Lock()
	versionHandshakeDone = false
	versionHandshakeErr = nil
	versionHandshakeMu.Unlock()
}

// streamDaemon serves an SSE stream that writes the given raw event
// blocks (each a complete "event:/data:\n\n" chunk) and then CLOSES the
// connection WITHOUT a terminal event — simulating a dropped proxy /
// daemon restart. GET /api/task/<id>/status returns statusBody so the
// CLI's fallback can read the authoritative terminal state. It does not
// serve /api/version (404 → treated as implicit schema v1, like the
// real install base).
func streamDaemon(t *testing.T, events []string, statusBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, ev := range events {
			_, _ = w.Write([]byte(ev))
			if fl != nil {
				fl.Flush()
			}
		}
		// Return without sending a terminal event: the handler exiting
		// closes the response body, which the client reads as EOF.
	})
	mux.HandleFunc("/api/task/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close() })
	return srv
}

// A dropped SSE connection on a task that actually finished as `failed`
// must NOT exit 0. The CLI falls back to GET /status and surfaces the
// real terminal outcome (exit 2), so an agent never reads truncation as
// success.
func TestTaskStream_ConnectionClosed_FallsBackToTerminalStatus(t *testing.T) {
	srv := streamDaemon(t,
		[]string{"event: message\ndata: {\"text\":\"working\"}\n\n"},
		`{"id":"t1","status":"failed","error_message":"boom","instruction":"go"}`,
	)
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)
	resetVersionHandshake()
	flagStreamFollowFinal = false

	_, runErr := captureStdout(t, func() error {
		return runTaskStream(taskStreamCmd, []string{"t1"})
	})

	if runErr == nil {
		t.Fatalf("expected non-nil error for a stream that closed on a failed task, got nil (exit 0)")
	}
	var oe *exit.OutcomeError
	if !errors.As(runErr, &oe) {
		t.Fatalf("expected *OutcomeError from the status fallback, got %T: %v", runErr, runErr)
	}
	if oe.Code != exit.TaskFailed {
		t.Errorf("OutcomeError.Code: got %d, want %d (TaskFailed)", oe.Code, exit.TaskFailed)
	}
}

// A task that parks on an approval / credential card (task:waiting) must
// TERMINATE the stream and exit 3 (TaskNeedsInput), matching `task wait`.
// Before the fix the daemon's task:waiting event (no `status` field) matched
// neither the terminal switch nor the status fallback, so the SSE hung on
// keepalives forever — an agent blocked on the very pause that needs it to act.
func TestTaskStream_WaitingForInput_ExitsNeedsInput(t *testing.T) {
	srv := streamDaemon(t,
		[]string{"event: task:waiting\ndata: {\"task_id\":\"t1\"}\n\n"},
		`{"id":"t1","status":"waiting_for_input","instruction":"go"}`,
	)
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)
	resetVersionHandshake()
	flagStreamFollowFinal = false

	_, runErr := captureStdout(t, func() error {
		return runTaskStream(taskStreamCmd, []string{"t1"})
	})

	if runErr == nil {
		t.Fatalf("expected non-nil error (exit 3) for a task parked on an approval card, got nil")
	}
	var oe *exit.OutcomeError
	if !errors.As(runErr, &oe) {
		t.Fatalf("expected *OutcomeError, got %T: %v", runErr, runErr)
	}
	if oe.Code != exit.TaskNeedsInput {
		t.Errorf("OutcomeError.Code: got %d, want %d (TaskNeedsInput)", oe.Code, exit.TaskNeedsInput)
	}
}

// A dropped SSE connection on a task that is STILL running must surface a
// distinct non-zero error (not a false success and not a task-outcome
// code), so the caller knows the stream was truncated mid-flight.
func TestTaskStream_ConnectionClosed_StillRunning_NonZero(t *testing.T) {
	srv := streamDaemon(t,
		[]string{"event: message\ndata: {\"text\":\"still going\"}\n\n"},
		`{"id":"t1","status":"running","instruction":"go"}`,
	)
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)
	resetVersionHandshake()
	flagStreamFollowFinal = false

	out, runErr := captureStdout(t, func() error {
		return runTaskStream(taskStreamCmd, []string{"t1"})
	})

	if runErr == nil {
		t.Fatalf("expected a non-nil error for a stream dropped on a running task, got nil (exit 0)")
	}
	// Must NOT be an OutcomeError (the task did not reach a terminal state).
	var oe *exit.OutcomeError
	if errors.As(runErr, &oe) {
		t.Fatalf("did not expect an OutcomeError for a still-running task, got code %d", oe.Code)
	}
	// It maps to a CLI-class failure (exit 1), and a connection_closed end
	// marker should have been emitted on stdout.
	if code := exit.FromError(runErr); code != exit.CLIError {
		t.Errorf("exit.FromError: got %d, want %d (CLIError)", code, exit.CLIError)
	}
	if !strings.Contains(out, "connection_closed") {
		t.Errorf("expected a connection_closed end event on stdout, got: %q", out)
	}
}

func TestTaskCancel_EmitsCancelData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/task/t1/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"cancel requested"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)

	out, err := captureStdout(t, func() error {
		return runTaskCancel(taskCancelCmd, []string{"t1"})
	})
	if err != nil {
		t.Fatalf("runTaskCancel: %v", err)
	}
	if !strings.Contains(out, `"id":"t1"`) || !strings.Contains(out, `"status":"cancelled"`) {
		t.Errorf("envelope missing expected fields: %q", out)
	}
}
