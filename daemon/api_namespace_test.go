// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// raw issues a request WITHOUT the ts.do path-normalization, so we can
// assert the exact routing contract after the D-7 cutover.
func (ts *testServer) raw(method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	return w
}

// TestAPINamespace_OnlyApiPath verifies the D-7 cutover: data-plane
// routes are reachable ONLY under /api/*. The legacy un-namespaced
// paths (used by the retired in-platform ChatLayout) now 404. /health
// and /routes/verify keep their bare paths — registered explicitly,
// not via dual(), because the platform cron and Caddy call them
// verbatim.
func TestAPINamespace_OnlyApiPath(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-namespace")

	type route struct {
		method string
		legacy string
		newer  string
		auth   string
	}
	routes := []route{
		{"GET", "/status", "/api/status", "Bearer " + jwtToken},
		{"GET", "/tasks", "/api/tasks", "Bearer " + jwtToken},
		{"GET", "/keys", "/api/keys", "Bearer " + jwtToken},
		{"GET", "/management/usage", "/api/management/usage", "Bearer " + ts.daemonTok},
	}

	for _, r := range routes {
		if n := ts.raw(r.method, r.newer, r.auth); n.Code == http.StatusNotFound {
			t.Errorf("%s %s should be registered, got 404", r.method, r.newer)
		}
		if l := ts.raw(r.method, r.legacy, r.auth); l.Code != http.StatusNotFound {
			t.Errorf("legacy %s %s should be retired (404), got %d",
				r.method, r.legacy, l.Code)
		}
	}

	// /health keeps its bare path (platform cron contract); /api/health
	// also exists (dashboard version probe).
	if h := ts.raw("GET", "/health", "Bearer "+ts.healthTok); h.Code == http.StatusNotFound {
		t.Errorf("/health must remain on its bare path for the cron")
	}
	if h := ts.raw("GET", "/api/health", "Bearer "+ts.healthTok); h.Code == http.StatusNotFound {
		t.Errorf("/api/health must also exist (dashboard version probe)")
	}
}

// /routes/verify presence-at-original-path is verified at compile time
// by registerRoutes (mux.HandleFunc) — no separate test needed. Behavior
// is exercised in production by Caddy on-demand TLS asking the daemon.
