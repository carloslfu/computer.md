// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// app.go is the Phase 4 customer-app layer: an app the manager deployed
// (~/apps/<name>/) runs in its own sandbox, narrower than a system
// (just a runtime + declared deps + its data dir). It cannot reach the
// agent's secrets, other apps' data, or exfiltrate freely.
//
// Caddy-retarget decision (deviation from the plan's literal
// "rewrite buildCaddyfile to target the veth IP", recorded here): the
// Caddyfile generator is brick-critical — a bad regen takes down the
// dashboard, the apex site, AND every hosted app at once. Instead the
// daemon runs a tiny host→sandbox TCP forwarder
// (127.0.0.1:<port> → <sandbox-veth-ip>:<port>), so the generated
// `reverse_proxy localhost:<port>` stays BYTE-IDENTICAL. Same
// isolation (the app process is fully sandboxed), near-zero blast
// radius on the brick-critical path. Pure-logic half here; the
// forwarder + lifecycle is app_linux.go.

// AppManifest is ~/apps/<name>/manifest.json.
type AppManifest struct {
	// Port the app listens on INSIDE its sandbox (bound 0.0.0.0:Port).
	Port int `json:"port"`
	// StartCmd is the argv that launches the app (long-running).
	StartCmd []string `json:"start_cmd"`
	// RestartPolicy: "always" (default) | "never".
	RestartPolicy string `json:"restart_policy"`
	// Egress the app may reach (apps are usually narrow: a DB, an API).
	AllowFQDNs    []string `json:"allow_fqdns"`
	AllowCIDRs    []string `json:"allow_cidrs"`
	EnforceEgress bool     `json:"enforce_egress"`
	// VaultRefs the app's sandbox env gets (only these).
	VaultRefs []string `json:"vault_refs"`
}

func LoadAppManifest(appsRoot, name string) (*AppManifest, error) {
	if !systemNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid app name %q", name)
	}
	b, err := os.ReadFile(filepath.Join(appsRoot, name, "manifest.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m AppManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing app manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *AppManifest) Validate() error {
	if m.Port < 1 || m.Port > 65535 {
		return fmt.Errorf("app port %d out of range", m.Port)
	}
	if m.Port == 8420 {
		return fmt.Errorf("app port 8420 is reserved (daemon)")
	}
	if len(m.StartCmd) == 0 {
		return fmt.Errorf("app start_cmd is required")
	}
	switch m.RestartPolicy {
	case "", "always", "never":
	default:
		return fmt.Errorf("invalid restart_policy %q", m.RestartPolicy)
	}
	for _, f := range m.AllowFQDNs {
		if !isPlausibleFQDNPattern(f) {
			return fmt.Errorf("invalid egress FQDN %q", f)
		}
	}
	for _, c := range m.AllowCIDRs {
		if !cidrPattern.MatchString(c) {
			return fmt.Errorf("invalid egress CIDR %q", c)
		}
	}
	for _, r := range m.VaultRefs {
		if !vaultRefName.MatchString(r) {
			return fmt.Errorf("invalid vault ref %q", r)
		}
	}
	return nil
}

func (m *AppManifest) Mode() EgressMode {
	if m.EnforceEgress {
		return EgressEnforce
	}
	return EgressAudit
}

func (m *AppManifest) restartAlways() bool {
	return m.RestartPolicy == "" || m.RestartPolicy == "always"
}

// ToSandboxManifest builds the customer-app sandbox. appDir
// (~/apps/<name>) is identity-bound (same path in/out) so the app's
// own absolute paths resolve. No X display (apps are headless
// services); narrow egress; only declared vault refs.
func (m *AppManifest) ToSandboxManifest(name, appDir string, resolvedEnv map[string]string) *SandboxManifest {
	return &SandboxManifest{
		ID:       "app-" + name,
		Type:     TypeCustomerApp,
		Lifetime: Persistent,
		Hostname: "app-" + name,
		Mounts:   []Mount{{HostPath: appDir, SandboxPath: appDir}},
		Env:      resolvedEnv,
		Egress:   EgressPolicy{AllowFQDNs: m.AllowFQDNs, AllowCIDRs: m.AllowCIDRs},
		XDisplay: XDisplayNone,
		HomeDir:  appDir,
	}
}

// sanitizeAppName guards interpolation into forwarder/log identifiers.
func sanitizeAppName(s string) string { return strings.ReplaceAll(s, "/", "_") }
