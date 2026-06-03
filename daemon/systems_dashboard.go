// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// systems_dashboard.go is the BROWSER-side surface for the operator's
// at-a-glance "what's running on my computer" view. Parallel to
// systems.go (which serves the manager via localhost-token over the
// per-sandbox unix socket), this handler is cookie/bearer-authed so
// the dashboard SPA can render the sidebar Systems section without a
// JWT dance.
//
// Why a separate handler and not a re-export of /api/systems with
// dual auth: the dashboard view needs different fields (humanized
// name, hosted-app URL cross-reference, a fast live probe) AND a
// different durability posture (~1s response time budget for the
// sidebar, not the manager's "I'll spend a few seconds describing
// everything precisely" budget). Keeping them separate lets each
// evolve without breaking the other.
//
// Strictly read-only. Mutation (uninstall) stays on the localhost
// path — the manager owns its authored systems, the customer
// directs the manager.

// dashboardSystemView is the on-the-wire shape for GET /api/dashboard/
// systems. Tuned for the sidebar rendering: every field maps to one
// rendered element. No internal fields like crontab_lines or
// has_run_sh leak through — those are developer artifacts; the
// non-technical operator wants name + status + last-touched.
type dashboardSystemView struct {
	Name        string `json:"name"`                  // raw folder name (the manager's identifier)
	DisplayName string `json:"display_name"`          // humanized (kebab→space, title case)
	Kind        string `json:"kind"`                  // "app" | "scheduled" | "idle"
	URL         string `json:"url,omitempty"`         // public URL if this is a hosted app
	Live        string `json:"live,omitempty"`        // "up" | "down" — populated only when URL is set
	LastActive  string `json:"last_active,omitempty"` // RFC3339; most recent of dir mtime / logs / route timestamp
}

// handleDashboardSystems lists authored systems for the sidebar. Auth
// is cookie/bearer (registered with withCookieOrBearer in main.go).
//
// Latency budget: ~1s. The hot path is the per-hosted-app HTTP probe;
// each probe is bounded by httpProbeTimeout and all probes fan out in
// parallel, so worst case is httpProbeTimeout itself regardless of N.
func (s *Server) handleDashboardSystems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Snapshot the route table so we can cross-reference each system
	//    with a hosted-app URL. List is cheap and synchronous; if it
	//    errors we still return systems, just without URLs (degraded
	//    but not broken — the dashboard renders the names + timestamps).
	routesByName := map[string]struct {
		URL  string
		Port int
	}{}
	if rl, err := s.routeMgr.List(); err == nil {
		for _, r := range rl {
			routesByName[r.Name] = struct {
				URL  string
				Port int
			}{URL: s.routeMgr.URL(r.Name), Port: r.Port}
		}
	}

	// 2. Walk ~/systems/<name>/ for everything the manager has authored.
	entries, err := os.ReadDir(systemsRoot)
	if err != nil && !os.IsNotExist(err) {
		jsonError(w, "read systems dir", http.StatusInternalServerError)
		return
	}
	views := make([]dashboardSystemView, 0)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !systemNamePattern.MatchString(name) {
			continue
		}
		views = append(views, buildSystemView(name, routesByName))
	}

	// 3. Pure hosted apps that don't have a corresponding ~/systems/<name>/
	//    directory (a route registered without an authored system folder).
	//    Still surface them — they're as "running" as anything else from
	//    the operator's perspective.
	for routeName, route := range routesByName {
		alreadyListed := false
		for _, v := range views {
			if v.Name == routeName {
				alreadyListed = true
				break
			}
		}
		if alreadyListed {
			continue
		}
		v := dashboardSystemView{
			Name:        routeName,
			DisplayName: humanizeName(routeName),
			Kind:        "app",
			URL:         route.URL,
		}
		views = append(views, v)
	}

	// 4. Probe live status for every system that has a URL. Parallel
	//    so total latency is bounded by httpProbeTimeout regardless
	//    of how many apps are listed.
	portByName := make(map[string]int, len(routesByName))
	for n, info := range routesByName {
		portByName[n] = info.Port
	}
	probeLiveStatus(r.Context(), views, portByName)

	// 5. Stable ordering: live apps first (the customer's "look, it's
	//    working" reward), then scheduled, then idle. Alphabetical
	//    within each kind.
	sort.SliceStable(views, func(i, j int) bool {
		ri, rj := kindRank(views[i]), kindRank(views[j])
		if ri != rj {
			return ri < rj
		}
		return views[i].DisplayName < views[j].DisplayName
	})

	jsonResponse(w, http.StatusOK, map[string]any{"systems": views})
}

// buildSystemView pulls together everything we know about a single
// system folder. Reuses describeSystem (systems.go) for the on-disk
// state and adds the route cross-reference + humanized name.
func buildSystemView(name string, routesByName map[string]struct {
	URL  string
	Port int
}) dashboardSystemView {
	entry := describeSystem(name)

	v := dashboardSystemView{
		Name:        name,
		DisplayName: humanizeName(name),
		LastActive:  pickMostRecent(entry.LastRunAt, entry.LastModified),
	}

	if route, ok := routesByName[name]; ok && route.URL != "" {
		v.URL = route.URL
		v.Kind = "app"
		return v
	}
	if entry.HasCrontab && entry.CrontabLines > 0 {
		v.Kind = "scheduled"
		return v
	}
	v.Kind = "idle"
	return v
}

// pickMostRecent returns whichever RFC3339 timestamp is later, falling
// back gracefully when one or both are empty. Used so the sidebar's
// "last active" reflects either the most recent log write (a
// scheduled run) OR the most recent dir mtime (the manager edited it),
// whichever is more recent.
func pickMostRecent(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil {
		return b
	}
	if errB != nil {
		return a
	}
	if ta.After(tb) {
		return a
	}
	return b
}

// humanizeName converts a folder name like "expense-tracker" or
// "invoice_triage_2" into a sidebar-friendly "Expense tracker" /
// "Invoice triage 2". Non-technical operators see real words, not
// kebab-case identifiers.
func humanizeName(name string) string {
	if name == "" {
		return name
	}
	// Replace separators with spaces, then sentence-case the first
	// letter. We don't title-case every word — "Expense tracker" reads
	// less shouty than "Expense Tracker" and matches our brand voice.
	replaced := strings.NewReplacer("-", " ", "_", " ").Replace(name)
	runes := []rune(replaced)
	if runes[0] >= 'a' && runes[0] <= 'z' {
		runes[0] = runes[0] - 32
	}
	return string(runes)
}

// kindRank gives the sort priority for the sidebar. "Up" apps lead;
// "down" apps come next so they're still visible (and obvious); then
// scheduled, then idle.
func kindRank(v dashboardSystemView) int {
	switch v.Kind {
	case "app":
		if v.Live == "up" {
			return 0
		}
		return 1 // down or unknown — still surfaced near the top
	case "scheduled":
		return 2
	default:
		return 3
	}
}

// httpProbeTimeout is the per-app deadline for the live-status check.
// 1.5s is a generous ceiling for a localhost-bound HTTP request;
// anything slower and we'd rather show "down" than make the sidebar
// feel laggy.
const httpProbeTimeout = 1500 * time.Millisecond

// probeLiveStatus probes the LOCAL backend port of each hosted app
// concurrently and mutates the views in place. Same view Caddy has:
// if we can reach 127.0.0.1:PORT on the host's loopback, Caddy on
// the same loopback can reverse-proxy a real (signed-in) request to
// it. Backend reachable here ↔ app is genuinely live for the
// customer who's allowed to see it.
//
// Why NOT probe the public URL: for Private apps the auth gate
// returns 302 → /signin to anonymous probes, and 302 is in
// [200, 500). The pre-v0.47 implementation treated that as "up" —
// which lied about Private apps whose backend was actually 502
// behind the gate. The localhost probe sees through the gate by
// not going through it: Caddy isn't in the path here, only the
// backend listener is, which is exactly what determines whether a
// real authenticated request would succeed.
//
// Why HEAD-then-GET: some servers (Python's BaseHTTPRequestHandler
// is the worst offender we've actually shipped) return 501 to HEAD.
// Falling through to GET catches that case without needing a
// special list.
func probeLiveStatus(ctx context.Context, views []dashboardSystemView, portByName map[string]int) {
	var wg sync.WaitGroup
	for i := range views {
		port, ok := portByName[views[i].Name]
		if !ok || port == 0 {
			// No route → nothing to probe. Idle/scheduled systems
			// stay un-annotated; the sidebar shows them with their
			// last-active timestamp instead of a status dot.
			continue
		}
		wg.Add(1)
		go func(idx int, p int) {
			defer wg.Done()
			views[idx].Live = probeLocalPort(ctx, p)
		}(i, port)
	}
	wg.Wait()
}

// probeLocalPort returns "up" | "down" for 127.0.0.1:port within
// httpProbeTimeout. Two-stage check:
//
//  1. TCP dial first — if nothing's listening, we're done and the
//     app is "down" without bothering to construct an HTTP request.
//     This is the dominant failure mode (server died, never started,
//     bound to a different port). Fast.
//
//  2. HEAD then GET on http://127.0.0.1:PORT/. Any 2xx/3xx/4xx means
//     the backend answered (a 404 from a wrong path still means the
//     server is alive). 5xx counts as "down" — the backend is
//     broken in a customer-visible way.
//
// The probe deliberately does NOT follow the public URL through
// Caddy — see the package-level comment on probeLiveStatus for why.
func probeLocalPort(ctx context.Context, port int) string {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	probeCtx, cancel := context.WithTimeout(ctx, httpProbeTimeout)
	defer cancel()

	// TCP dial — first cheap signal. If this fails, we're done.
	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	if c, err := dialer.DialContext(probeCtx, "tcp", addr); err != nil {
		return "down"
	} else {
		_ = c.Close()
	}

	// HTTP probe — the backend is listening, but is it actually
	// answering? A bound port with no handler (or a crashed handler
	// returning 500) is still "down" from the customer's view.
	client := &http.Client{Timeout: httpProbeTimeout}
	url := "http://" + addr + "/"
	tryRequest := func(method string) (int, error) {
		req, err := http.NewRequestWithContext(probeCtx, method, url, nil)
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}

	status, err := tryRequest(http.MethodHead)
	if err == nil && status == http.StatusNotImplemented {
		status, err = tryRequest(http.MethodGet)
	}
	if err != nil {
		return "down"
	}
	if status >= 200 && status < 500 {
		return "up"
	}
	return "down"
}

// ensureSystemsRoot is a defensive helper used by tests. Production
// always has /home/vibecraft/systems by the time the dashboard renders
// (the manager creates it on first system), but unit tests need to
// point at a temp dir.
func ensureSystemsRoot(path string) string {
	if path == "" {
		return systemsRoot
	}
	return filepath.Clean(path)
}
