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
