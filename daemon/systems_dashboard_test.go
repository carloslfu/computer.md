// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The dashboard view's job is to render the customer's "what's running"
// in the sidebar. These tests pin the customer-facing pieces — the
// humanized name and the kind classification — without spinning up
// the full daemon. The live-probe path is exercised by the live URL
// fan-out in handleDashboardSystems and bounded by httpProbeTimeout;
// we trust the standard library's http.Client behavior under timeout
// rather than wiring up a fake server for one boolean.

func TestHumanizeName(t *testing.T) {
	// Non-technical operators see real words, not kebab-case. The
	// transform is sentence-case (first letter only), NOT title-case
	// — "Expense tracker" reads less shouty than "Expense Tracker"
	// and matches the prompt's calm brand voice.
	cases := []struct {
		in, want string
	}{
		{"expense-tracker", "Expense tracker"},
		{"invoice_triage", "Invoice triage"},
		{"invoice-triage-v2", "Invoice triage v2"},
		{"a", "A"},
		{"already-Sentence", "Already Sentence"}, // mid-string capitals preserved
	}
	for _, c := range cases {
		got := humanizeName(c.in)
		if got != c.want {
			t.Errorf("humanizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanizeNameEmptyStringIsSafe(t *testing.T) {
	if got := humanizeName(""); got != "" {
		t.Errorf("humanizeName(\"\") = %q, want \"\"", got)
	}
}

func TestKindRankPutsLiveAppsFirst(t *testing.T) {
	// Sidebar sort order is part of the customer experience: live apps
	// lead (their "look, it works" moment), then down/unknown apps
	// (still surfaced — silently hiding a broken app is worse than
	// showing it), then scheduled systems, then idle.
	cases := []struct {
		v        dashboardSystemView
		wantRank int
	}{
		{dashboardSystemView{Kind: "app", Live: "up"}, 0},
		{dashboardSystemView{Kind: "app", Live: "down"}, 1},
		{dashboardSystemView{Kind: "app", Live: ""}, 1}, // unknown → near top
		{dashboardSystemView{Kind: "scheduled"}, 2},
		{dashboardSystemView{Kind: "idle"}, 3},
	}
	for _, c := range cases {
		got := kindRank(c.v)
		if got != c.wantRank {
			t.Errorf("kindRank(%+v) = %d, want %d", c.v, got, c.wantRank)
		}
	}
}

func TestPickMostRecent(t *testing.T) {
	// "Last active" is the later of (most recent log mtime, dir mtime).
	// Edge: empty strings, parse failures fall back gracefully so the
	// sidebar never blanks an entry that has any timestamp at all.
	cases := []struct {
		a, b, want string
		desc       string
	}{
		{"2026-05-21T01:00:00Z", "2026-05-21T02:00:00Z", "2026-05-21T02:00:00Z", "b is later"},
		{"2026-05-21T03:00:00Z", "2026-05-21T02:00:00Z", "2026-05-21T03:00:00Z", "a is later"},
		{"", "2026-05-21T02:00:00Z", "2026-05-21T02:00:00Z", "a empty"},
		{"2026-05-21T01:00:00Z", "", "2026-05-21T01:00:00Z", "b empty"},
		{"", "", "", "both empty"},
		{"garbage", "2026-05-21T02:00:00Z", "2026-05-21T02:00:00Z", "a unparseable"},
	}
	for _, c := range cases {
		got := pickMostRecent(c.a, c.b)
		if got != c.want {
			t.Errorf("%s: pickMostRecent(%q, %q) = %q, want %q", c.desc, c.a, c.b, got, c.want)
		}
	}
}

// The HTTP shape (405 on non-GET) is the same contract every other
// dual-registered handler honors. Pin it so a refactor that flips to
// a non-method-checked handler is caught loudly.
func TestHandleDashboardSystems_RejectsNonGet(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/systems", nil)
	w := httptest.NewRecorder()
	srv.handleDashboardSystems(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/dashboard/systems = %d, want 405", w.Code)
	}
}

// Regression for the v0.47 sidebar probe fix: pre-v0.47 we probed
// the PUBLIC URL through Caddy. For Private apps with a dead backend,
// Caddy's forward_auth returned 302 → /signin and the probe counted
// the 302 as "up" (in [200,500)). The sidebar lied — green dot for
// an app actually returning 502 to signed-in users.
//
// The fix: probe the local backend port directly, same view Caddy
// has. A backend that's listening + answering 2xx/3xx/4xx is "up";
// nothing listening / 5xx is "down."

// TestProbeLocalPort_UpWhenBackendAnswers2xx — happy path, 200 from a
// real listener → "up".
func TestProbeLocalPort_UpWhenBackendAnswers2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := mustPortFromURL(t, srv.URL)

	if got := probeLocalPort(context.Background(), port); got != "up" {
		t.Errorf("200 listener → expected up, got %q", got)
	}
}

// TestProbeLocalPort_UpEvenOn404 — a 404 from a wrong path still
// means the backend is reachable. Probe shouldn't punish that.
func TestProbeLocalPort_UpEvenOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	port := mustPortFromURL(t, srv.URL)

	if got := probeLocalPort(context.Background(), port); got != "up" {
		t.Errorf("404 listener → expected up (backend reachable), got %q", got)
	}
}

// TestProbeLocalPort_DownWhenBackendReturns5xx — the listener exists
// but the handler is broken. The customer sees "Internal Server
// Error" if they visit. Sidebar should reflect that as "down."
func TestProbeLocalPort_DownWhenBackendReturns5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	port := mustPortFromURL(t, srv.URL)

	if got := probeLocalPort(context.Background(), port); got != "down" {
		t.Errorf("500 listener → expected down (backend broken), got %q", got)
	}
}

// TestProbeLocalPort_DownWhenNothingListening — the most common
// production case: server died, no listener. Must NOT lie about it.
// Bound the test runtime — the probe should give up well under the
// httpProbeTimeout for an immediately-refused connection.
func TestProbeLocalPort_DownWhenNothingListening(t *testing.T) {
	// Grab a port then close the listener so we know the port is free.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	start := time.Now()
	got := probeLocalPort(context.Background(), port)
	elapsed := time.Since(start)

	if got != "down" {
		t.Errorf("closed port → expected down, got %q", got)
	}
	if elapsed > httpProbeTimeout+500*time.Millisecond {
		t.Errorf("probe took %v on a refused port — should give up faster", elapsed)
	}
}

// TestProbeLocalPort_HandlesBaseHTTPHeadQuirk — Python's
// BaseHTTPRequestHandler returns 501 to HEAD even when GET works.
// We hit this on a real customer build at v0.43.0. Probe must fall
// through to GET when HEAD returns 501.
func TestProbeLocalPort_HandlesBaseHTTPHeadQuirk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := mustPortFromURL(t, srv.URL)

	if got := probeLocalPort(context.Background(), port); got != "up" {
		t.Errorf("Python-BaseHTTP-shape (501 HEAD, 200 GET) → expected up, got %q", got)
	}
}

// TestProbeLiveStatus_SkipsRouteless — systems with no registered
// route (idle, scheduled, watcher) shouldn't be probed at all and
// shouldn't get a "down" label. They render with a timestamp, not
// a status dot.
func TestProbeLiveStatus_SkipsRouteless(t *testing.T) {
	views := []dashboardSystemView{
		{Name: "scheduled-thing", Kind: "scheduled"},
		{Name: "idle-thing", Kind: "idle"},
	}
	portByName := map[string]int{} // empty — no routes registered

	probeLiveStatus(context.Background(), views, portByName)

	for _, v := range views {
		if v.Live != "" {
			t.Errorf("routeless system %q got Live=%q, expected empty (no probe)", v.Name, v.Live)
		}
	}
}

// mustPortFromURL extracts the port from an httptest.NewServer URL.
// Small helper, isolated so the table-tests above stay readable.
func mustPortFromURL(t *testing.T, url string) int {
	t.Helper()
	// httptest URLs look like "http://127.0.0.1:NNNN"
	i := strings.LastIndex(url, ":")
	if i < 0 {
		t.Fatalf("can't parse port from %q", url)
	}
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("bad port in %q: %v", url, err)
	}
	return p
}
