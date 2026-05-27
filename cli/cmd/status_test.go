// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/cli/output"
)

// fakeDaemon returns an httptest.Server that answers GET /api/status with
// the given JSON body and status code. The bearer-token check mirrors the
// real daemon's expectation (vc_machine_* prefix) but is otherwise loose.
func fakeDaemon(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer vc_machine_") {
			http.Error(w, `{"error":"missing bearer"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

// isolateEnv points the CLI at the given machine via env vars and gives
// it an empty HOME so loadConfig() finds no on-disk credentials. Both
// are restored on test cleanup.
func isolateEnv(t *testing.T, machineURL, apiKey string) {
	t.Helper()
	t.Setenv("VIBECRAFT_MACHINE_URL", machineURL)
	t.Setenv("VIBECRAFT_API_KEY", apiKey)
	t.Setenv("HOME", t.TempDir())
}

// withOutputMode swaps the output package's mode for one test.
func withOutputMode(t *testing.T, m output.Mode) {
	t.Helper()
	prev := output.CurrentMode()
	output.SetMode(m)
	t.Cleanup(func() { output.SetMode(prev) })
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything that was written. output.Emit writes to os.Stdout directly,
// so we have to swap the FD; passing a writer through isn't an option.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	prev := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = prev }()

	runErr := fn()
	_ = w.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), runErr
}

func TestStatus_JSONSuccess(t *testing.T) {
	srv := fakeDaemon(t, 200, `{"status":"healthy","machine_id":"vc-abc","uptime":3600,"current_task":"abc-123"}`)
	defer srv.Close()

	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)

	got, err := captureStdout(t, func() error { return runStatus(statusCmd, nil) })
	if err != nil {
		t.Fatalf("runStatus: %v", err)
	}

	want := `{"v":1,"ok":true,"data":{"machine_id":"vc-abc","status":"healthy","uptime_seconds":3600,"current_task":"abc-123"}}` + "\n"
	if got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestStatus_TextSuccess(t *testing.T) {
	srv := fakeDaemon(t, 200, `{"status":"healthy","machine_id":"vc-abc","uptime":3600}`)
	defer srv.Close()

	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeText)

	got, err := captureStdout(t, func() error { return runStatus(statusCmd, nil) })
	if err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !strings.Contains(got, "Daemon: healthy") {
		t.Errorf("text mode missing status line: %q", got)
	}
	if !strings.Contains(got, "Machine: vc-abc") {
		t.Errorf("text mode missing machine line: %q", got)
	}
}

func TestStatus_AuthRequired_NoCreds(t *testing.T) {
	isolateEnv(t, "", "")

	err := runStatus(statusCmd, nil)
	if err == nil {
		t.Fatal("expected auth_required error, got nil")
	}
	if !strings.Contains(err.Error(), "auth_required") {
		t.Errorf("expected auth_required, got: %v", err)
	}
}

func TestStatus_Maps401ToAuthInvalid(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer bad.Close()

	isolateEnv(t, bad.URL, "vc_machine_test_test_test_xyz")

	err := runStatus(statusCmd, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "auth_invalid") {
		t.Errorf("expected auth_invalid, got: %v", err)
	}
}

func TestStatus_Maps5xxToServerError(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"oops"}`, http.StatusInternalServerError)
	}))
	defer bad.Close()

	isolateEnv(t, bad.URL, "vc_machine_test_test_test_xyz")

	err := runStatus(statusCmd, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "server_error") {
		t.Errorf("expected server_error, got: %v", err)
	}
}
