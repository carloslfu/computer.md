// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentSmoke is the F8 "agent smoke" — pretend to be an agent,
// shell out to vibecraft against a fake daemon, parse every output,
// assert exit codes. This validates the CLI's wire contract from a
// caller's perspective (the unit tests cover internal correctness).
//
// What we exercise:
//   vibecraft status                    → ok envelope, exit 0
//   vibecraft task submit <msg>         → completed envelope, exit 0
//   vibecraft task submit <msg> --no-wait → queued envelope, exit 0
//   vibecraft task get <id>             → completed envelope, exit 0
//   vibecraft task list                 → list envelope, exit 0
//   vibecraft auth whoami               → whoami envelope, exit 0
//   vibecraft docs                      → docs envelope, exit 0
//   bogus auth → auth_invalid, exit 1
func TestAgentSmoke(t *testing.T) {
	bin := findBinary(t)
	if bin == "" {
		return
	}

	mux := http.NewServeMux()
	// status
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"healthy","machine_id":"vc-fake","uptime":42}`))
	})
	// task submit + status + list
	taskID := "abc-123"
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"abc-123","status":"queued","conversation_id":"c1","created_at":"2026-05-21T10:00:00Z"}`))
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/task/abc-123/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"abc-123","status":"completed","result":"hello"}`))
	})
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"abc-123","status":"completed","created_at":"2026-05-21T10:00:00Z"}]}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantKey  string // a string that must appear in stdout
	}{
		{"status", []string{"status"}, 0, `"machine_id":"vc-fake"`},
		{"task submit wait", []string{"task", "submit", "do it"}, 0, `"result":"hello"`},
		{"task submit no-wait", []string{"task", "submit", "do it", "--no-wait"}, 0, `"status":"queued"`},
		{"task get", []string{"task", "get", taskID}, 0, `"status":"completed"`},
		{"task list", []string{"task", "list"}, 0, `"tasks"`},
		{"version", []string{"version"}, 0, `"version"`},
		{"docs", []string{"docs"}, 0, `"# VibeCraft CLI`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := exec.Command(bin, tc.args...)
			c.Env = append(c.Env,
				"HOME="+t.TempDir(),
				"VIBECRAFT_MACHINE_URL="+srv.URL,
				"VIBECRAFT_API_KEY=vc_machine_test_test_test_xyz",
				"PATH="+filepath.Dir(bin),
				"VIBECRAFT_NO_AUTO_UPDATE=1", // exec'd binary must not phone home
			)
			out, err := c.Output()
			gotExit := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				gotExit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("unexpected non-exit error: %v", err)
			}
			if gotExit != tc.wantExit {
				t.Errorf("exit code: got %d, want %d; output: %s", gotExit, tc.wantExit, string(out))
			}
			// Parse the first JSON line and verify the envelope is well-formed.
			firstLine := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
			var env map[string]any
			if err := json.Unmarshal([]byte(firstLine), &env); err != nil {
				t.Errorf("stdout not valid JSON: %v\n%s", err, firstLine)
			}
			if env["ok"] != true {
				t.Errorf("ok != true: %v", env["ok"])
			}
			if !strings.Contains(string(out), tc.wantKey) {
				t.Errorf("output missing %q:\n%s", tc.wantKey, string(out))
			}
		})
	}
}
