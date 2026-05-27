// SPDX-License-Identifier: Apache-2.0

package routes

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

const (
	caddyfilePath = "/etc/caddy/Caddyfile"
	daemonPort    = 8420
	minPort       = 1024
	maxPort       = 65535
	maxRoutes     = 50
)

var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Manager handles route registration and Caddyfile management.
type Manager struct {
	db          *persistence.DB
	machineHost string
	mu          sync.Mutex
}

// NewManager creates a route manager for the given machine host.
func NewManager(db *persistence.DB, machineHost string) *Manager {
	return &Manager{
		db:          db,
		machineHost: machineHost,
	}
}

// SyncCaddyfile loads all routes from the database and regenerates the Caddyfile.
// Called on daemon startup. Retries if Caddy isn't ready yet (common on first boot).
func (m *Manager) SyncCaddyfile() error {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(5 * time.Second)
			log.Printf("retrying Caddyfile sync (attempt %d/6)", attempt+1)
		}

		m.mu.Lock()
		lastErr = m.regenerateAndReload()
		m.mu.Unlock()

		if lastErr == nil {
			return nil
		}
	}

	return fmt.Errorf("Caddyfile sync failed after 6 attempts: %w", lastErr)
}

// Register adds or updates a route and reloads Caddy.
func (m *Manager) Register(name string, port int) error {
	if err := m.validateRoute(name, port); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check route count limit.
	existing, err := m.db.ListRoutes()
	if err != nil {
		return fmt.Errorf("listing routes: %w", err)
	}

	isUpdate := false
	for _, r := range existing {
		if r.Name == name {
			isUpdate = true
			break
		}
	}
	if !isUpdate && len(existing) >= maxRoutes {
		return fmt.Errorf("maximum of %d routes reached", maxRoutes)
	}

	// Check for port conflicts.
	for _, r := range existing {
		if r.Port == port && r.Name != name {
			return fmt.Errorf("port %d is already used by route %q", port, r.Name)
		}
	}

	if err := m.db.CreateRoute(name, port); err != nil {
		return fmt.Errorf("saving route: %w", err)
	}

	if err := m.regenerateAndReload(); err != nil {
		// Route is saved but Caddy reload failed. Log but don't undo the DB write;
		// next SyncCaddyfile will pick it up.
		log.Printf("warning: route %q saved but Caddy reload failed: %v", name, err)
		return fmt.Errorf("route saved but Caddy reload failed: %w", err)
	}

	return nil
}

// Unregister removes a route and reloads Caddy.
func (m *Manager) Unregister(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.db.DeleteRoute(name); err != nil {
		return err
	}

	if err := m.regenerateAndReload(); err != nil {
		log.Printf("warning: route %q deleted but Caddy reload failed: %v", name, err)
		return fmt.Errorf("route deleted but Caddy reload failed: %w", err)
	}

	return nil
}

// List returns all registered routes.
func (m *Manager) List() ([]persistence.Route, error) {
	routes, err := m.db.ListRoutes()
	if err != nil {
		return nil, err
	}
	if routes == nil {
		routes = []persistence.Route{}
	}
	return routes, nil
}

// SetSSOEnabled toggles the per-app SSO flag. Regenerates the
// Caddyfile because the /__auth/whoami auto-proxy block depends on
// the flag.
func (m *Manager) SetSSOEnabled(name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.db.SetRouteSSOEnabled(name, enabled); err != nil {
		return err
	}
	return m.regenerateAndReload()
}

// Verify checks if a hostname is a valid registered route subdomain.
// Used by Caddy's on_demand_tls to approve certificate issuance.
func (m *Manager) Verify(hostname string) bool {
	// The hostname must be {name}.{machineHost}.
	suffix := "." + m.machineHost
	if !strings.HasSuffix(hostname, suffix) {
		return false
	}

	name := strings.TrimSuffix(hostname, suffix)
	if name == "" || strings.Contains(name, ".") {
		return false
	}

	_, err := m.db.GetRoute(name)
	return err == nil
}

// URL returns the full URL for a route.
func (m *Manager) URL(name string) string {
	return fmt.Sprintf("https://%s.%s", name, m.machineHost)
}

// reservedPorts are system/infrastructure ports that customer apps cannot bind to.
var reservedPorts = map[int]string{
	8420: "daemon",
	80:   "HTTP (Caddy)",
	443:  "HTTPS (Caddy)",
	2019: "Caddy admin API",
}

func (m *Manager) validateRoute(name string, port int) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid route name %q: must be lowercase alphanumeric with hyphens, 1-63 chars", name)
	}

	if port < minPort || port > maxPort {
		return fmt.Errorf("port must be between %d and %d", minPort, maxPort)
	}

	if reason, reserved := reservedPorts[port]; reserved {
		return fmt.Errorf("port %d is reserved for %s", port, reason)
	}

	return nil
}

// regenerateAndReload writes the Caddyfile and reloads Caddy. Must be called with m.mu held.
func (m *Manager) regenerateAndReload() error {
	routes, err := m.db.ListRoutes()
	if err != nil {
		return fmt.Errorf("listing routes: %w", err)
	}

	caddyfile := m.buildCaddyfile(routes)

	if err := safeWriteAndReload(caddyfile); err != nil {
		return fmt.Errorf("applying Caddyfile: %w", err)
	}

	log.Printf("Caddyfile updated: %d app route(s)", len(routes))
	return nil
}

func (m *Manager) buildCaddyfile(routes []persistence.Route) string {
	var b strings.Builder

	// Global options.
	b.WriteString("{\n")
	b.WriteString("  on_demand_tls {\n")
	b.WriteString(fmt.Sprintf("    ask http://localhost:%d/routes/verify\n", daemonPort))
	b.WriteString("  }\n")
	b.WriteString("}\n\n")

	// Daemon API (main domain).
	b.WriteString(fmt.Sprintf("%s {\n", m.machineHost))
	b.WriteString(fmt.Sprintf("  reverse_proxy localhost:%d\n", daemonPort))
	b.WriteString("}\n")

	// Customer app routes. Two-axis design:
	//
	//   /__auth/whoami — ALWAYS mounted, regardless of SSOEnabled. This
	//     is the identity-helper endpoint apps can fetch same-origin
	//     to learn who the visitor is (or get a clean 401 if anonymous).
	//     A public app that wants to greet the operator by name still
	//     gets to read it; making it conditional on SSOEnabled was a
	//     non-feature and confused the meaning of the toggle.
	//
	//   SSOEnabled (renamed "Private" in the UI) — when true, Caddy
	//     forward_auth's the catch-all to /api/auth/whoami first.
	//     Daemon returns 200 for a valid vc_session / vc_sso cookie,
	//     401 otherwise. On 401 we redirect to the platform sign-in
	//     with ?next= so the visitor lands back on the same URL after
	//     auth. When false, the app is publicly reachable — anyone
	//     with the URL gets through (current behavior, kept as the
	//     "Public" mode).
	//
	// The /__auth/whoami `handle` block runs first (it's path-matched
	// and Caddy short-circuits handle blocks on match), so forward_auth
	// never gates the identity-helper endpoint — apps fetching it
	// always get a real answer (200 with identity, or 401 anonymous),
	// never a sign-in redirect.
	for _, r := range routes {
		b.WriteString(fmt.Sprintf("\n%s.%s {\n", r.Name, m.machineHost))
		b.WriteString("  tls {\n")
		b.WriteString("    on_demand\n")
		b.WriteString("  }\n")
		b.WriteString("  handle /__auth/whoami {\n")
		b.WriteString(fmt.Sprintf("    reverse_proxy localhost:%d {\n", daemonPort))
		b.WriteString("      rewrite /api/auth/whoami\n")
		b.WriteString("    }\n")
		b.WriteString("  }\n")
		if r.SSOEnabled {
			b.WriteString("  handle {\n")
			b.WriteString(fmt.Sprintf("    forward_auth localhost:%d {\n", daemonPort))
			b.WriteString("      uri /api/auth/whoami\n")
			b.WriteString("      copy_headers Cookie\n")
			b.WriteString("      @sso-denied status 401 403\n")
			b.WriteString("      handle_response @sso-denied {\n")
			b.WriteString("        redir https://www.vibecraft.so/signin?next={scheme}://{host}{uri} 302\n")
			b.WriteString("      }\n")
			b.WriteString("    }\n")
			b.WriteString(fmt.Sprintf("    reverse_proxy localhost:%d\n", r.Port))
			b.WriteString("  }\n")
		} else {
			b.WriteString(fmt.Sprintf("  reverse_proxy localhost:%d\n", r.Port))
		}
		b.WriteString("}\n")
	}

	return b.String()
}
