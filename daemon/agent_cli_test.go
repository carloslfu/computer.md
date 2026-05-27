// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandleVersion(t *testing.T) {
	s := &Server{}
	srv := httptest.NewServer(http.HandlerFunc(s.handleVersion))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var v struct {
		Version        string `json:"version"`
		SchemaVersions []int  `json:"schema_versions"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	if v.Version == "" {
		t.Errorf("missing version")
	}
	found := false
	for _, sv := range v.SchemaVersions {
		if sv == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("schema_versions did not contain 1: %v", v.SchemaVersions)
	}
}

func TestIdempotencyStore(t *testing.T) {
	idempotencyMu.Lock()
	idempotencyStore = map[string]idempotencyEntry{}
	idempotencyMu.Unlock()

	// Empty key is a no-op for both store and lookup.
	idempotencyStoreSet("", "task-x")
	if _, ok := idempotencyLookup(""); ok {
		t.Errorf("empty key should not be stored")
	}

	idempotencyStoreSet("key-1", "task-1")
	tid, ok := idempotencyLookup("key-1")
	if !ok || tid != "task-1" {
		t.Errorf("got (%q, %v), want (task-1, true)", tid, ok)
	}

	// Expire it.
	idempotencyMu.Lock()
	idempotencyStore["key-1"] = idempotencyEntry{TaskID: "task-1", ExpiresAt: time.Now().Add(-1 * time.Second)}
	idempotencyMu.Unlock()
	if _, ok := idempotencyLookup("key-1"); ok {
		t.Errorf("expired entry should not return")
	}
}

func TestResolveJailedPath_RejectsEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "ok" or "err"
	}{
		{"empty", "", "err"},
		{"traversal", "/etc/passwd", "err"},
		{"relative traversal", "../etc/passwd", "err"},
		{"dot dot in middle", "/home/vibecraft/../etc/passwd", "err"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveJailedPath(tc.in)
			if tc.want == "err" && err == nil {
				t.Errorf("resolveJailedPath(%q) returned nil error; want non-nil", tc.in)
			}
		})
	}
}

// TestHandleFiles_HappyPath uses a temp directory we pretend is the
// /home/vibecraft jail. We can't override the const at runtime without
// refactoring, but we CAN verify the path-safety rejection logic via
// the unit test above. The full happy path is exercised in the
// integration test against the running daemon.
func TestHandleFilesLs_RejectsFile(t *testing.T) {
	// Make a file under the real /home/vibecraft if we're root; otherwise
	// skip — we can't actually create paths in the production jail from
	// a unit-test process.
	if _, err := os.Stat(fileJailRoot); err != nil {
		t.Skipf("jail root %s does not exist on this host; skipping", fileJailRoot)
	}
	tmp := filepath.Join(fileJailRoot, "agent-cli-test-tmp.txt")
	if err := os.WriteFile(tmp, []byte("test"), 0600); err != nil {
		t.Skipf("cannot write under %s: %v", fileJailRoot, err)
	}
	defer os.Remove(tmp)

	s := &Server{}
	req := httptest.NewRequest("GET", "/files-ls?path="+tmp, nil)
	rr := httptest.NewRecorder()
	s.handleFilesLs(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for file-not-dir; got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "is a file") {
		t.Errorf("body did not mention 'is a file': %s", rr.Body.String())
	}
}
