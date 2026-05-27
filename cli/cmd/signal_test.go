// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestCtrlCPostsCancel verifies that a SIGINT mid-poll causes the CLI
// to POST /task/<id>/cancel to the daemon (A13).
//
// We launch the freshly-built binary as a subprocess pointed at a fake
// daemon that returns a never-terminal task, then SIGINT it after a
// brief delay, and assert the cancel endpoint was hit.
func TestCtrlCPostsCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal semantics differ on Windows; this test is for Unix")
	}

	var cancelHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"abc-123","status":"queued","conversation_id":"c1"}`))
	})
	mux.HandleFunc("/api/task/abc-123/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Always "running" — never reach terminal so the poll loop keeps spinning.
		_, _ = w.Write([]byte(`{"id":"abc-123","status":"running"}`))
	})
	mux.HandleFunc("/api/task/abc-123/cancel", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cancelHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"cancel requested"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Find the built binary.
	binPath := findBinary(t)

	cmd := exec.Command(binPath, "task", "submit", "long-running-test", "--wait")
	cmd.Env = append(os.Environ(),
		"VIBECRAFT_MACHINE_URL="+srv.URL,
		"VIBECRAFT_API_KEY=vc_machine_test_test_test_xyz",
		"HOME="+t.TempDir(),
		"VIBECRAFT_NO_AUTO_UPDATE=1", // exec'd binary must not phone home
	)
	// Give the subprocess its own process group so we can target SIGINT to it cleanly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting CLI: %v", err)
	}

	// Give it a moment to submit + start polling.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("CLI did not exit after SIGINT within 10s")
	}

	// Give the cancel request a moment to land before asserting.
	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&cancelHits); got != 1 {
		t.Errorf("expected exactly one cancel hit, got %d", got)
	}
}

func findBinary(t *testing.T) string {
	t.Helper()
	// The Makefile builds at cli/vibecraft. Tests in cli/cmd run from cli/cmd,
	// so the binary is at ../vibecraft.
	candidates := []string{
		filepath.Join("..", "vibecraft"),
		filepath.Join(".", "vibecraft"),
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	t.Skipf("vibecraft binary not built; run 'go build -o vibecraft .' in cli/ first")
	return ""
}
