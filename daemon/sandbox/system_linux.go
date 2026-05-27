// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
)

// system_linux.go is the Phase 3 per-system runtime: one persistent
// sandbox per system the manager authored (~/systems/<name>/), created
// on first use, reused after, scoped to the system's manifest (egress
// allowlist + declared vault refs only). Workers spawned "within" a
// system inherit that system's egress/credentials, never the agent's
// broader scope. Builds on the real-kernel-proven Create/Run/Destroy.

type systemEntry struct {
	sb     *Sandbox
	cancel context.CancelFunc // stops cron + discovery watchers
}

var systemReg = struct {
	mu sync.Mutex
	m  map[string]*systemEntry
}{m: map[string]*systemEntry{}}

// SystemEgress returns the egress policy + mode a system's workloads
// must obey. Unmanifested system => discovery mode (empty allowlist,
// audit so nothing breaks while we observe — the Phase 3 default).
func SystemEgress(systemsRoot, name string) (EgressPolicy, EgressMode, error) {
	m, err := LoadSystemManifest(systemsRoot, name)
	if err != nil {
		return EgressPolicy{}, EgressAudit, err
	}
	if m == nil {
		return EgressPolicy{}, EgressAudit, nil // discovery mode
	}
	return EgressPolicy{AllowFQDNs: m.AllowFQDNs, AllowCIDRs: m.AllowCIDRs}, m.Mode(), nil
}

// GetOrCreateSystemSandbox returns the live persistent sandbox for a
// system, creating it from its manifest on first use. resolveVault maps
// the manifest's declared refs to values (per-system credential scope —
// ONLY declared secrets are injected). handler serves the system's
// per-sandbox /run/vibecraft.sock. Reused across tasks; survives until
// DestroySystemSandbox / daemon shutdown.
func GetOrCreateSystemSandbox(systemsRoot, name string, resolveVault func([]string) map[string]string, handler http.Handler) (*Sandbox, error) {
	systemReg.mu.Lock()
	defer systemReg.mu.Unlock()
	if e, ok := systemReg.m[name]; ok && !e.sb.destroyed {
		return e.sb, nil
	}
	dir := filepath.Join(systemsRoot, name)
	m, err := LoadSystemManifest(systemsRoot, name)
	if err != nil {
		return nil, err
	}
	// Per-sandbox crontab file lives in the system dir, which is already
	// identity-bound into the sandbox — no spool dir / extra mount.
	if cerr := ensureCrontab(dir); cerr != nil {
		return nil, cerr
	}
	var sm *SandboxManifest
	mode := EgressAudit
	discovery := false
	if m == nil {
		// Discovery: filesystem/PID/secret isolation still apply; egress
		// is permissive+logged (audit) until the customer locks it.
		discovery = true
		sm = &SandboxManifest{
			ID: "system-" + name, Type: TypeSystem, Lifetime: Persistent,
			Hostname: "system-" + name,
			Mounts:   []Mount{{HostPath: dir, SandboxPath: dir}},
			XDisplay: XDisplayShared, HomeDir: dir,
		}
	} else {
		var env map[string]string
		if resolveVault != nil {
			env = resolveVault(m.VaultRefs)
		}
		sm = m.ToSandboxManifest(name, dir, env)
		mode = m.Mode()
	}
	sb, err := Create(sm, mode, handler)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	systemReg.m[name] = &systemEntry{sb: sb, cancel: cancel}
	// The system's own scheduler (Phase 3): scheduled work runs inside
	// this sandbox's restrictions, not on the host.
	go superviseCron(ctx, sb, dir, "system-"+name)
	// Unmanifested system → propose an egress manifest after the window.
	if discovery {
		go watchDiscovery(ctx, sb, name)
	}
	return sb, nil
}

// RunInSystem runs a command inside the system's persistent sandbox
// (creating it if needed).
func RunInSystem(ctx context.Context, systemsRoot, name string, resolveVault func([]string) map[string]string, handler http.Handler, cmd []string, extraEnv map[string]string) (string, error) {
	sb, err := GetOrCreateSystemSandbox(systemsRoot, name, resolveVault, handler)
	if err != nil {
		return "", err
	}
	return sb.Run(ctx, cmd, extraEnv)
}

// DestroySystemSandbox tears down a system's sandbox (uninstall).
func DestroySystemSandbox(name string) {
	systemReg.mu.Lock()
	e := systemReg.m[name]
	delete(systemReg.m, name)
	systemReg.mu.Unlock()
	if e != nil {
		e.cancel() // stop cron + discovery watchers
		e.sb.Destroy()
	}
}
