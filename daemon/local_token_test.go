// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/routes"
	"github.com/carloslfu/computer.md/daemon/sandbox"
)

// Phase 0b: localhost-only data-plane endpoints require the per-machine
// local token. The token is a boot invariant (LoadConfig generates one
// if absent), so there is NO legacy no-token mode — a tokenless loopback
// caller is rejected unconditionally.

func TestLocalToken_DaemonTaskEnforced(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "the-local-token-abc123"
	body := `{"instruction":"task from the agent shell"}`

	t.Run("loopback without token is rejected", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("loopback with wrong token is rejected", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body,
			map[string]string{"Authorization": "Bearer not-the-token"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("loopback with correct token is accepted", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body,
			map[string]string{"Authorization": "Bearer the-local-token-abc123"})
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-loopback is rejected regardless of token", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/task", "192.0.2.5:5000", body,
			map[string]string{"Authorization": "Bearer the-local-token-abc123"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestLocalToken_SandboxSocketPrivilegeIsAgentShellOnly(t *testing.T) {
	ts := newTestServer(t)
	body := `{"instruction":"task from sandbox socket"}`

	doSandbox := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/daemon/task", strings.NewReader(body))
		req.RemoteAddr = "unix"
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(sandbox.ContextWithSandboxID(req.Context(), id))
		w := httptest.NewRecorder()
		ts.mux.ServeHTTP(w, req)
		return w
	}

	t.Run("agent shell socket remains authorized", func(t *testing.T) {
		w := doSandbox(sandbox.AgentShellID)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("system sandbox socket is not a local token bypass", func(t *testing.T) {
		w := doSandbox("system-invoice-triage")
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// No legacy mode: even with an (impossible-in-prod) empty configured
// token, a tokenless loopback POST is rejected. LoadConfig guarantees a
// non-empty token via generate-if-absent, so this guards that the
// removed empty-token fallback never silently returns.
func TestLocalToken_NoLegacyModeEvenWhenUnset(t *testing.T) {
	ts := newTestServer(t)
	// Force the impossible-in-prod empty-token state to prove the
	// removed legacy fallback never silently returns.
	ts.server.cfg.LocalToken = ""
	body := `{"instruction":"tokenless caller must be rejected, not waved through"}`
	w := ts.doRaw(t, "POST", "/daemon/task", "127.0.0.1:55123", body, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no legacy mode: tokenless loopback must be 401, got %d: %s", w.Code, w.Body.String())
	}
}

// /routes/verify uses withLoopbackOnly: Caddy's tokenless on-demand-TLS
// `ask` must keep working even when a local token is configured, while
// the external ${machineHost} reverse_proxy probe (X-Forwarded-For set)
// is rejected.
func TestRouteVerify_LoopbackOnlyNoTokenRequired(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "a-configured-local-token"
	ts.server.routeMgr = routes.NewManager(ts.server.db, ts.server.cfg.MachineHost)
	if err := ts.server.db.CreateRoute("blog", 3000); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}
	domain := "blog." + ts.server.cfg.MachineHost // blog.vc-test123.vc.vibecraft.so

	t.Run("Caddy ask path: loopback, no token, no X-F-F -> handler runs (200, not 401)", func(t *testing.T) {
		w := ts.doRaw(t, "GET", "/routes/verify?domain="+domain, "127.0.0.1:40000", "", nil)
		if w.Code == http.StatusUnauthorized {
			t.Fatalf("token must NOT be required on /routes/verify (would break Caddy on-demand TLS); got 401")
		}
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for a registered route, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("external probe via reverse_proxy (X-Forwarded-For set) -> 403", func(t *testing.T) {
		w := ts.doRaw(t, "GET", "/routes/verify?domain="+domain, "127.0.0.1:40000", "",
			map[string]string{"X-Forwarded-For": "203.0.113.7"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for X-Forwarded-For probe, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-loopback RemoteAddr -> 403", func(t *testing.T) {
		w := ts.doRaw(t, "GET", "/routes/verify?domain="+domain, "198.51.100.9:7000", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-loopback, got %d: %s", w.Code, w.Body.String())
		}
	})
}
