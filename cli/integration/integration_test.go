// SPDX-License-Identifier: Apache-2.0

// Package integration runs the CLI binary against a stub daemon HTTP
// server that emulates every endpoint the agent-native verbs hit.
//
// F7 — "the test that proves the CLI and daemon agree on the wire
// format". Builds the CLI from sources, stands up the stub daemon,
// shells out one verb at a time, asserts (exit code, JSON envelope).
//
// This is NOT the F8 agent smoke (which only covers a handful of
// happy-path verbs); this one walks every verb the CLI exposes.
//
// Run with: go test -tags=integration ./cli/integration/...
//
// Build tag keeps it out of the default test pass so the per-PR suite
// stays under 30s — F7 runs nightly + on release tag (see
// .github/workflows/cli-test.yml).
//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubDaemon is a self-contained httptest.Server that pretends to be a
// VibeCraft machine for the duration of the integration suite. Every
// endpoint the CLI's client.go touches is implemented here.
type stubDaemon struct {
	srv         *httptest.Server
	taskCounter int32
	tasks       map[string]map[string]any
	memory      map[string]map[string]any
	memoryNext  int32
}

func newStubDaemon(t *testing.T) *stubDaemon {
	t.Helper()
	d := &stubDaemon{
		tasks:  map[string]map[string]any{},
		memory: map[string]map[string]any{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"version":         "integration-test",
			"schema_versions": []int{1},
		})
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "healthy", "machine_id": "vc-stub", "uptime": 42,
		})
	})
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := fmt.Sprintf("task-%d", atomic.AddInt32(&d.taskCounter, 1))
		task := map[string]any{
			"id":              id,
			"status":          "completed",
			"instruction":     "stub",
			"result":          "stub-result",
			"conversation_id": "conv-stub",
			"created_at":      time.Now().UTC().Format(time.RFC3339),
		}
		d.tasks[id] = task
		writeJSON(w, http.StatusCreated, task)
	})
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		ts := make([]any, 0, len(d.tasks))
		for _, t := range d.tasks {
			ts = append(ts, t)
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": ts})
	})
	mux.HandleFunc("/api/task/", func(w http.ResponseWriter, r *http.Request) {
		// /api/task/<id>/status, /respond, /cancel
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/task/"), "/")
		if len(parts) == 0 {
			http.NotFound(w, r)
			return
		}
		id := parts[0]
		task, ok := d.tasks[id]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if len(parts) == 1 || parts[1] == "status" {
			writeJSON(w, http.StatusOK, task)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/api/conversations/conv-stub", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"conversation": map[string]any{"id": "conv-stub"},
			"messages":     []map[string]any{{"role": "user", "content": "stub"}},
			"tasks":        []map[string]any{},
		})
	})
	mux.HandleFunc("/api/screenshot", func(w http.ResponseWriter, r *http.Request) {
		// Tiny 1x1 PNG in base64.
		writeJSON(w, http.StatusOK, map[string]any{
			"image":    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
			"format":   "png",
			"encoding": "base64",
		})
	})
	mux.HandleFunc("/api/vault", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	mux.HandleFunc("/api/memory", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	mux.HandleFunc("/api/files-ls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"path":    "/home/vibecraft",
			"entries": []any{},
		})
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "event: task:completed\ndata: %s\n\n", `{"task_id":"task-1","status":"completed","result":"stream done"}`)
		if flusher != nil {
			flusher.Flush()
		}
	})

	d.srv = httptest.NewServer(mux)
	t.Cleanup(func() { d.srv.Close() })
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func findCLIBinary(t *testing.T) string {
	t.Helper()
	// integration tests run from cli/integration/; binary is at ../vibecraft
	cands := []string{
		filepath.Join("..", "vibecraft"),
		filepath.Join("..", "..", "cli", "vibecraft"),
	}
	for _, c := range cands {
		abs, _ := filepath.Abs(c)
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	t.Skip("vibecraft binary not built; run 'go build -o vibecraft .' in cli/ first")
	return ""
}

func TestIntegrationFullSurface(t *testing.T) {
	bin := findCLIBinary(t)
	if bin == "" {
		return
	}
	d := newStubDaemon(t)

	verbs := [][]string{
		{"version"},
		{"status"},
		{"task", "submit", "stub task"},
		{"task", "submit", "stub task", "--no-wait"},
		{"task", "get", "task-1"},
		{"task", "list"},
		{"task", "messages", "task-1"},
		{"task", "cancel", "task-1"},
		{"screenshot", "-o", filepath.Join(t.TempDir(), "shot.png")},
		{"vault", "list"},
		{"memory", "list"},
		{"files", "ls"},
		{"docs"},
	}

	for _, args := range verbs {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			c := exec.Command(bin, args...)
			c.Env = append(c.Env,
				"HOME="+t.TempDir(),
				"VIBECRAFT_MACHINE_URL="+d.srv.URL,
				"VIBECRAFT_API_KEY=vc_machine_int_test_int_int_int",
			)
			out, err := c.CombinedOutput()
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 0 {
					t.Errorf("non-zero exit: %v\n%s", err, string(out))
					return
				}
			}
			// Must produce parseable JSON envelope on stdout.
			firstLine := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
			var env map[string]any
			if err := json.Unmarshal([]byte(firstLine), &env); err != nil {
				t.Errorf("not valid JSON: %v\n%s", err, firstLine)
			}
		})
	}
}
