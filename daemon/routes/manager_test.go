// SPDX-License-Identifier: Apache-2.0

package routes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

func setupTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Use a simple key for testing (SQLCipher needs one).
	db, err := persistence.Open(dbPath, "testkey1234567890123456789012345678")
	if err != nil {
		t.Fatalf("failed to open test DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"dashboard", true},
		{"my-app", true},
		{"app1", true},
		{"a", true},
		{"my-cool-app-123", true},
		{"", false},
		{"-starts-with-hyphen", false},
		{"ends-with-hyphen-", false},
		{"has.dot", false},
		{"has space", false},
		{"UPPERCASE", false},
		{"has_underscore", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validName.MatchString(tt.name); got != tt.valid {
				t.Errorf("validName(%q) = %v, want %v", tt.name, got, tt.valid)
			}
		})
	}
}

func TestValidateRoute(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "test.vc.vibecraft.so")

	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"dashboard", 3000, false},
		{"my-app", 8080, false},
		{"app", 1024, false},
		{"app", 65535, false},
		// Invalid names.
		{"", 3000, true},
		{"UPPER", 3000, true},
		// Invalid ports.
		{"app", 0, true},
		{"app", 1023, true},
		{"app", 65536, true},
		{"app", 8420, true}, // Daemon port reserved.
		{"app", 80, true},   // HTTP (Caddy).
		{"app", 443, true},  // HTTPS (Caddy).
		{"app", 2019, true}, // Caddy admin API.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mgr.validateRoute(tt.name, tt.port)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRoute(%q, %d) error = %v, wantErr = %v", tt.name, tt.port, err, tt.wantErr)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-abc123.vc.vibecraft.so")

	// Register a route directly in DB (bypass Caddy reload for test).
	if err := db.CreateRoute("dashboard", 3000); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		hostname string
		want     bool
	}{
		{"dashboard.vc-abc123.vc.vibecraft.so", true},
		{"nonexistent.vc-abc123.vc.vibecraft.so", false},
		{"vc-abc123.vc.vibecraft.so", false},                // Base domain, not a route.
		{"evil.dashboard.vc-abc123.vc.vibecraft.so", false}, // Nested subdomain.
		{"dashboard.vc-wrong.vc.vibecraft.so", false},       // Wrong machine.
		{"dashboard.totally-different.com", false},          // Wrong domain.
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.hostname, func(t *testing.T) {
			if got := mgr.Verify(tt.hostname); got != tt.want {
				t.Errorf("Verify(%q) = %v, want %v", tt.hostname, got, tt.want)
			}
		})
	}
}

func TestBuildCaddyfile(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	// No routes.
	cf := mgr.buildCaddyfile(nil)
	if !contains(cf, "vc-test.vc.vibecraft.so") {
		t.Error("Caddyfile should contain machine host")
	}
	if !contains(cf, "on_demand_tls") {
		t.Error("Caddyfile should contain on_demand_tls block")
	}
	if !contains(cf, "reverse_proxy localhost:8420") {
		t.Error("Caddyfile should proxy to daemon")
	}

	// With routes.
	routes := []persistence.Route{
		{Name: "dashboard", Port: 3000},
		{Name: "api", Port: 4000},
	}
	cf = mgr.buildCaddyfile(routes)
	if !contains(cf, "dashboard.vc-test.vc.vibecraft.so") {
		t.Error("Caddyfile should contain dashboard route")
	}
	if !contains(cf, "reverse_proxy localhost:3000") {
		t.Error("Caddyfile should proxy dashboard to port 3000")
	}
	if !contains(cf, "api.vc-test.vc.vibecraft.so") {
		t.Error("Caddyfile should contain api route")
	}
	if !contains(cf, "reverse_proxy localhost:4000") {
		t.Error("Caddyfile should proxy api to port 4000")
	}
	if !contains(cf, "on_demand") {
		t.Error("Caddyfile should use on_demand TLS for app routes")
	}
}

// TestBuildCaddyfile_PrivateGatesAccess pins the v0.46.0 behavior:
// SSOEnabled=true MUST emit a forward_auth block that gates the
// catch-all reverse_proxy. Pre-v0.46.0 the toggle only added a
// /__auth/whoami helper and the app was still publicly reachable —
// non-technical operators (rightly) read "SSO on" as "only I can
// see this." This test prevents a regression to that misleading
// posture.
func TestBuildCaddyfile_PrivateGatesAccess(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	// Private route: SSOEnabled=true means "require sign-in".
	cf := mgr.buildCaddyfile([]persistence.Route{
		{Name: "private-app", Port: 3000, SSOEnabled: true},
	})

	// forward_auth must wrap the catch-all so anonymous requests hit
	// the daemon first. Without this the app is publicly reachable
	// regardless of the toggle (the original bug).
	if !contains(cf, "forward_auth localhost:8420") {
		t.Error("private route must use forward_auth to gate the catch-all")
	}
	if !contains(cf, "uri /api/auth/whoami") {
		t.Error("forward_auth must call /api/auth/whoami (returns 401 anonymous)")
	}
	if !contains(cf, "copy_headers Cookie") {
		t.Error("forward_auth must forward the request Cookie header so vc_session / vc_sso are checked")
	}
	// 401/403 → redirect to platform sign-in with a `next` param so
	// the visitor lands back on the original URL after auth.
	if !contains(cf, "@sso-denied status 401 403") {
		t.Error("forward_auth must match 401/403 as the denied response")
	}
	if !contains(cf, "redir https://www.vibecraft.so/signin?next=") {
		t.Error("denied response must redirect to platform sign-in")
	}
	// /__auth/whoami helper must be present even on private routes
	// so apps can resolve identity same-origin without bouncing
	// through the redirect.
	if !contains(cf, "handle /__auth/whoami") {
		t.Error("private route must still expose /__auth/whoami helper")
	}
}

// TestBuildCaddyfile_PublicDoesNotGate pins the inverse: SSOEnabled
// false (Public) MUST NOT emit forward_auth. The app is reachable to
// anyone with the URL. This is the explicit alternate mode the
// operator picks for a marketing page / demo / public dashboard.
func TestBuildCaddyfile_PublicDoesNotGate(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	cf := mgr.buildCaddyfile([]persistence.Route{
		{Name: "public-app", Port: 3001, SSOEnabled: false},
	})

	if contains(cf, "forward_auth") {
		t.Error("public route MUST NOT include forward_auth (would gate access)")
	}
	if !contains(cf, "reverse_proxy localhost:3001") {
		t.Error("public route must still have a catch-all reverse_proxy")
	}
	// /__auth/whoami helper is ALWAYS mounted (apps may still want
	// identity-when-available even when access is public — e.g. a
	// public page that greets the signed-in operator by name).
	if !contains(cf, "handle /__auth/whoami") {
		t.Error("public route must still expose /__auth/whoami helper (identity-when-available)")
	}
}

// TestBuildCaddyfile_WhoamiNeverRedirects: forward_auth gates the
// catch-all, but the /__auth/whoami HELPER endpoint must remain a
// real 200/401 response — apps fetching it must not get redirected
// when anonymous (the fetch() should see a 401 and the app picks
// how to react). Pin the structural property: the /__auth/whoami
// handle block sits ABOVE the gated catch-all in the Caddyfile, so
// Caddy short-circuits on it before forward_auth runs.
func TestBuildCaddyfile_WhoamiHelperShortCircuitsGate(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	cf := mgr.buildCaddyfile([]persistence.Route{
		{Name: "private-app", Port: 3000, SSOEnabled: true},
	})

	whoamiIdx := strings.Index(cf, "handle /__auth/whoami")
	forwardAuthIdx := strings.Index(cf, "forward_auth localhost:8420")
	if whoamiIdx == -1 || forwardAuthIdx == -1 {
		t.Fatalf("expected both whoami and forward_auth in Caddyfile; got %d / %d", whoamiIdx, forwardAuthIdx)
	}
	if whoamiIdx >= forwardAuthIdx {
		t.Errorf("/__auth/whoami handle must appear BEFORE forward_auth so it short-circuits "+
			"and apps fetching whoami see real 401/200 instead of a sign-in redirect "+
			"(whoami at %d, forward_auth at %d)", whoamiIdx, forwardAuthIdx)
	}
}

func TestWriteCaddyfileAtomicDoesNotFollowPredictableTmpSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Caddyfile")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	predictableTmp := target + ".tmp"
	if err := os.Symlink(victim, predictableTmp); err != nil {
		t.Fatal(err)
	}
	if err := writeCaddyfileAtomic(target, "new config"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(victim); err != nil {
		t.Fatal(err)
	} else if string(got) != "old" {
		t.Fatalf("predictable tmp symlink target was modified: %q", got)
	}
	if got, err := os.ReadFile(target); err != nil {
		t.Fatal(err)
	} else if string(got) != "new config" {
		t.Fatalf("Caddyfile = %q", got)
	}
	if fi, err := os.Lstat(predictableTmp); err != nil {
		t.Fatal(err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("predictable tmp symlink should have been ignored, mode=%s", fi.Mode())
	}
}

func TestPortConflictDetection(t *testing.T) {
	db := setupTestDB(t)

	// Register first route directly in DB.
	if err := db.CreateRoute("app1", 3000); err != nil {
		t.Fatal(err)
	}

	// Check that listing shows the port conflict.
	existing, err := db.ListRoutes()
	if err != nil {
		t.Fatal(err)
	}

	conflictFound := false
	for _, r := range existing {
		if r.Port == 3000 && r.Name != "app2" {
			conflictFound = true
			break
		}
	}
	if !conflictFound {
		t.Error("expected to find port 3000 conflict")
	}
}

func TestURL(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-abc123.vc.vibecraft.so")

	url := mgr.URL("dashboard")
	want := "https://dashboard.vc-abc123.vc.vibecraft.so"
	if url != want {
		t.Errorf("URL() = %q, want %q", url, want)
	}
}

func TestRouteCRUD(t *testing.T) {
	db := setupTestDB(t)

	// Create.
	if err := db.CreateRoute("myapp", 3000); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	// List.
	routes, err := db.ListRoutes()
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 1 || routes[0].Name != "myapp" || routes[0].Port != 3000 {
		t.Fatalf("ListRoutes: unexpected result: %+v", routes)
	}

	// Get.
	r, err := db.GetRoute("myapp")
	if err != nil {
		t.Fatalf("GetRoute: %v", err)
	}
	if r.Name != "myapp" || r.Port != 3000 {
		t.Fatalf("GetRoute: unexpected result: %+v", r)
	}

	// Update (upsert).
	if err := db.CreateRoute("myapp", 4000); err != nil {
		t.Fatalf("CreateRoute (update): %v", err)
	}
	r, _ = db.GetRoute("myapp")
	if r.Port != 4000 {
		t.Fatalf("expected port 4000 after update, got %d", r.Port)
	}

	// Delete.
	if err := db.DeleteRoute("myapp"); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}
	routes, _ = db.ListRoutes()
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes after delete, got %d", len(routes))
	}

	// Delete nonexistent.
	if err := db.DeleteRoute("nope"); err == nil {
		t.Fatal("expected error deleting nonexistent route")
	}
}

func TestCaddyfileWriteAndContent(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db, "vc-test.vc.vibecraft.so")

	// Build a Caddyfile and write it to a temp location.
	routes := []persistence.Route{
		{Name: "app", Port: 3000},
	}
	content := m.buildCaddyfile(routes)

	tmpFile := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	got := string(data)
	if !contains(got, "ask http://localhost:8420/routes/verify") {
		t.Error("Caddyfile missing on_demand_tls ask URL")
	}
	if !contains(got, "app.vc-test.vc.vibecraft.so") {
		t.Error("Caddyfile missing app route")
	}
}

// TestValidateRoute_CaddyfileInjectionRejected is the Phase 6
// attack-surface test for the agent-supplied route name. The route name
// is interpolated verbatim into the generated Caddyfile
// (buildCaddyfile: `fmt.Sprintf("\n%s.%s {\n", r.Name, ...)`), so a name
// carrying a newline + `}` could close the site block early and inject
// arbitrary Caddy directives (e.g. a `reverse_proxy` to an attacker
// host, or a `respond` that exfiltrates). validateRoute is the only
// thing between the HTTP body and that template, so every one of these
// must be rejected BEFORE it can reach buildCaddyfile.
func TestValidateRoute_CaddyfileInjectionRejected(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	overLong := strings.Repeat("a", 64) // > 63-char DNS label cap.

	malicious := []struct {
		desc string
		name string
	}{
		{"newline + injected block close + reverse_proxy", "app\n}\nevil.vc-test.vc.vibecraft.so {\n  reverse_proxy attacker.example.com:80\n}"},
		{"CRLF injection", "app\r\n}\n:80 {\n}"},
		{"closing brace only", "app}"},
		{"opening brace only", "app{"},
		{"bare brace pair", "{}"},
		{"space then directive", "app respond 200"},
		{"tab then directive", "app\trespond"},
		{"leading newline", "\napp"},
		{"trailing newline", "app\n"},
		{"path traversal", "../../etc/caddy/Caddyfile"},
		{"dot segment", "a.b"},
		{"slash", "a/b"},
		{"null byte", "app\x00evil"},
		{"backtick", "app`whoami`"},
		{"semicolon", "app;rm"},
		{"dollar brace", "app${x}"},
		{"uppercase (out of DNS label class)", "App"},
		{"over-long (64 chars)", overLong},
		{"empty", ""},
		{"hyphen prefix", "-app"},
		{"hyphen suffix", "app-"},
		{"unicode homoglyph", "аpp"}, // Cyrillic 'а'.
	}

	for _, m := range malicious {
		t.Run(m.desc, func(t *testing.T) {
			// The load-bearing guarantee: validateRoute is the single
			// gate between the /routes HTTP body and the Caddyfile
			// template. buildCaddyfile interpolates the name verbatim and
			// does NOT sanitize — so validateRoute MUST reject every one
			// of these, or a crafted name reaches the on-disk Caddyfile
			// and Caddy reload. This is the assertion that matters.
			if err := mgr.validateRoute(m.name, 3000); err == nil {
				t.Fatalf("SECURITY: validateRoute(%q) = nil — a name that "+
					"would inject into the Caddyfile was accepted", m.name)
			}

			// Demonstrate WHY the validator is load-bearing (not a claim
			// that the template self-defends): feed the name straight to
			// buildCaddyfile, bypassing the gate. The newline/brace
			// payloads are expected to produce a structurally different
			// Caddyfile here — that is precisely the injection the
			// validator prevents upstream, so we only *record* it, never
			// rely on buildCaddyfile to neutralize it.
			crafted := mgr.buildCaddyfile([]persistence.Route{{Name: m.name, Port: 3000}})
			if injectedSiteBlocks(crafted) {
				t.Logf("confirmed: name %q WOULD inject if validateRoute "+
					"were bypassed (extra/!=3 top-level blocks) — gate is "+
					"the only defense, and it held above", m.name)
			}
		})
	}
}

// injectedSiteBlocks reports whether the generated Caddyfile contains
// MORE top-level blocks than the exactly-three a single-route config
// must have: the global-options "{" block, the daemon-host block, and
// the one app-route block. A route name that broke out of its
// `<name>.<host> {` template — by closing the block and opening a new
// one, or by smuggling a directive that Caddy parses as a new site —
// yields a fourth (or differently-shaped) top-level block. Counting,
// not pattern-matching, is what makes this robust even when the
// injected host shares the machine-host suffix.
func injectedSiteBlocks(caddyfile string) bool {
	topLevelBlocks := 0
	for _, line := range strings.Split(caddyfile, "\n") {
		// buildCaddyfile writes top-level block openers at column 0;
		// everything inside a block is indented. Ignore indented and
		// blank lines.
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if strings.HasSuffix(line, "{") {
			topLevelBlocks++
		}
	}
	// Expected: global options + daemon host + 1 app route = 3.
	return topLevelBlocks != 3
}

// TestRegister_RejectsInjectionBeforeDBWrite asserts the full
// Manager.Register path (the method the /routes HTTP handler calls)
// short-circuits on validateRoute and never reaches CreateRoute /
// regenerateAndReload for a name that could break the Caddyfile out.
func TestRegister_RejectsInjectionBeforeDBWrite(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewManager(db, "vc-test.vc.vibecraft.so")

	bad := "app\n}\n:80 {\n  respond \"pwned\"\n}"
	if err := mgr.Register(bad, 3000); err == nil {
		t.Fatalf("Register(%q) = nil, want validation error", bad)
	}

	// The DB must be untouched — no route row created for the bad name.
	routes, err := db.ListRoutes()
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("Register persisted a route despite invalid name: %+v", routes)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
