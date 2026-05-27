// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doRaw lets a test set RemoteAddr — needed for the localhost-bound
// /daemon/task route. The shared `do` helper in auth_test.go uses
// httptest.NewRequest's default RemoteAddr (192.0.2.1), which the
// withLocalhostAuth middleware would reject.
func (ts *testServer) doRaw(t *testing.T, method, path, remoteAddr, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	// D-7: data routes are /api/* only. Normalize bare paths the same
	// way ts.do does so these localhost-binding tests hit the real
	// route. /routes/verify stays bare (Caddy contract).
	if !strings.HasPrefix(path, "/api/") &&
		!strings.HasPrefix(path, "/routes/verify") {
		path = "/api" + path
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	return w
}

// ─── Test: /daemon/task — localhost binding ───

func TestDaemonTaskAcceptsLocalhost(t *testing.T) {
	ts := newTestServer(t)
	body := `{"instruction":"test task from cron"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, ts.lauth())
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["instruction"] != "test task from cron" {
		t.Errorf("expected instruction echoed back, got %v", resp["instruction"])
	}
	if resp["status"] != "queued" {
		t.Errorf("expected new task to be queued, got %v", resp["status"])
	}
}

func TestDaemonTaskAcceptsIPv6Localhost(t *testing.T) {
	ts := newTestServer(t)
	body := `{"instruction":"ipv6 test"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "[::1]:55123", body, ts.lauth())
	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 for ::1, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDaemonTaskRejectsRemoteIP(t *testing.T) {
	ts := newTestServer(t)
	body := `{"instruction":"would-be remote"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "10.0.0.5:44321", body, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for remote IP, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDaemonTaskRejectsXForwardedFor(t *testing.T) {
	ts := newTestServer(t)
	// Even with a 127.0.0.1 RemoteAddr, an X-Forwarded-For header means
	// the request came through Caddy. Reject.
	body := `{"instruction":"smuggled via caddy"}`
	headers := map[string]string{"X-Forwarded-For": "203.0.113.42"}
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, headers)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 with X-Forwarded-For set, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── Test: /daemon/task — body validation ───

func TestDaemonTaskRequiresInstruction(t *testing.T) {
	ts := newTestServer(t)
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", `{}`, ts.lauth())
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDaemonTaskAcceptsMessageAsAlias(t *testing.T) {
	ts := newTestServer(t)
	// "message" should work as an alias for "instruction" — mirrors POST /task.
	body := `{"message":"hello via message field"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, ts.lauth())
	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 for message-aliased instruction, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDaemonTaskGroupsBySystemName(t *testing.T) {
	ts := newTestServer(t)
	body := `{"instruction":"daily check-in","system":"invoice-triage"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, ts.lauth())
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["conversation_id"] != "system:invoice-triage" {
		t.Errorf("expected conversation_id=system:invoice-triage, got %v", resp["conversation_id"])
	}
}

func TestDaemonTaskRespectsExplicitConversationID(t *testing.T) {
	ts := newTestServer(t)
	body := `{"instruction":"step 2","conversation_id":"existing-convo-xyz"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, ts.lauth())
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["conversation_id"] != "existing-convo-xyz" {
		t.Errorf("expected explicit conversation id preserved, got %v", resp["conversation_id"])
	}
}

// ─── Test: /daemon/task — method enforcement ───

func TestDaemonTaskRejectsGET(t *testing.T) {
	ts := newTestServer(t)
	w := ts.doRaw(t, "GET", "/daemon/task", "127.0.0.1:55123", "", ts.lauth())
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", w.Code)
	}
}
