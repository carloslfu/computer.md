// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// system.go is the Phase 3 per-system layer: each system the manager
// authors (~/systems/<name>/) declares a manifest.json scoping its
// egress, the vault secrets it may read, and extra mounts. The daemon
// turns that into a System-type sandbox (persistent, one per system);
// workers spawned within a system run in a child that inherits the
// system's egress + credentials, never the agent's broader scope.
//
// This file is pure (parse + map + validate) so it is unit-tested on
// any OS; the runtime (GetOrCreateSystemSandbox, per-system cron) builds
// on the real-kernel-proven Create/Run/Destroy primitive.

// SystemManifest is ~/systems/<name>/manifest.json.
type SystemManifest struct {
	// Egress the system is allowed to reach. Empty + EnforceEgress=true
	// => default-deny everything (locked-down system).
	AllowFQDNs []string `json:"allow_fqdns"`
	AllowCIDRs []string `json:"allow_cidrs"`

	// EnforceEgress: false (default) => audit mode (log, don't block —
	// the Phase 3 default that hedges CDN churn); true => enforce
	// (default-deny). Flipped per system by the customer after the
	// audit log stabilizes.
	EnforceEgress bool `json:"enforce_egress"`

	// VaultRefs are the ONLY vault secret names this system's sandbox
	// may have injected (per-system credential scope: a bookkeeping
	// system can't read the marketing system's keys).
	VaultRefs []string `json:"vault_refs"`

	// Mounts are extra host->sandbox binds beyond the system's own dir
	// (e.g. a shared read-only utility dir). Validated like any mount.
	Mounts []Mount `json:"mounts"`
}

var systemNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,48}$`)

// SystemManifestPath is ~/systems/<name>/manifest.json.
func SystemManifestPath(systemsRoot, name string) string {
	return filepath.Join(systemsRoot, name, "manifest.json")
}

// LoadSystemManifest reads + validates a system's manifest. A missing
// file is NOT an error — it returns (nil, nil) so the caller can apply
// discovery mode (permissive + connection logging) for unmanifested
// legacy systems, exactly the Phase 3 migration path.
func LoadSystemManifest(systemsRoot, name string) (*SystemManifest, error) {
	if !systemNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid system name %q", name)
	}
	path := SystemManifestPath(systemsRoot, name)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil // unmanifested => discovery mode (caller decides)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var m SystemManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Validate rejects manifests with malformed egress/refs/mounts.
func (m *SystemManifest) Validate() error {
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
		// Vault refs are uppercase-ASCII (vault.ReferencePattern); a
		// per-system manifest must not smuggle arbitrary env names.
		if !vaultRefName.MatchString(r) {
			return fmt.Errorf("invalid vault ref %q (must be UPPER_SNAKE)", r)
		}
	}
	for i, mt := range m.Mounts {
		if !strings.HasPrefix(mt.HostPath, "/") || !strings.HasPrefix(mt.SandboxPath, "/") {
			return fmt.Errorf("mount[%d]: paths must be absolute", i)
		}
		if isForbiddenHostMount(mt.HostPath) {
			return fmt.Errorf("mount[%d]: %q is never bind-mountable", i, mt.HostPath)
		}
	}
	return nil
}

var vaultRefName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// EgressMode maps the manifest's EnforceEgress to the sandbox enum.
func (m *SystemManifest) Mode() EgressMode {
	if m.EnforceEgress {
		return EgressEnforce
	}
	return EgressAudit
}

// ToSandboxManifest builds the System-type sandbox for this system.
// systemDir (~/systems/<name>) is bound at the SAME path (identity map,
// like the agent-shell) so the manager's absolute ~/systems/<name>
// paths resolve in and out. resolvedEnv is the vault values for the
// declared VaultRefs (only those — per-system credential scope), built
// by the caller from the vault.
func (m *SystemManifest) ToSandboxManifest(name, systemDir string, resolvedEnv map[string]string) *SandboxManifest {
	mounts := append([]Mount{{HostPath: systemDir, SandboxPath: systemDir}}, m.Mounts...)
	return &SandboxManifest{
		ID:       "system-" + name,
		Type:     TypeSystem,
		Lifetime: Persistent,
		Hostname: "system-" + name,
		Mounts:   mounts,
		Env:      resolvedEnv,
		Egress:   EgressPolicy{AllowFQDNs: m.AllowFQDNs, AllowCIDRs: m.AllowCIDRs},
		XDisplay: XDisplayShared,
		HomeDir:  systemDir,
	}
}
