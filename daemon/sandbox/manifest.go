// SPDX-License-Identifier: Apache-2.0

// Package sandbox is the Phase 1 sandboxd primitive from
// plans/per-workload-sandboxing.md: the daemon spawns each workload
// (worker, system, customer app, the agent's own shell) inside its own
// Linux namespaces via bubblewrap, with a daemon-managed credential
// view and a per-sandbox egress allowlist.
//
// This file and spawn.go / network.go are the PURE-LOGIC layer the
// plan's testing strategy isolates ("manifest validation, bind-mount
// path construction, nftables rule generation — no real bwrap; mock
// the spawn call"). They are unit-tested on any OS. The privileged
// execution layer (real bwrap exec, veth, cgroup, nftables apply) is
// Linux-only and gated behind build tags / integration tests that run
// on a privileged Linux CI runner — it is NOT exercised here.
package sandbox

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SandboxType is the workload class. It selects defaults (egress
// breadth, display mode) but every concrete policy still comes from the
// manifest — the type is a label, not a hidden ruleset.
type SandboxType int

const (
	TypeWorker SandboxType = iota
	TypeSystem
	TypeCustomerApp
	TypeAgentShell
)

func (t SandboxType) String() string {
	switch t {
	case TypeWorker:
		return "worker"
	case TypeSystem:
		return "system"
	case TypeCustomerApp:
		return "customer-app"
	case TypeAgentShell:
		return "agent-shell"
	default:
		return "unknown"
	}
}

// Lifetime distinguishes ephemeral (torn down at task end) from
// persistent (survives across tasks; daemon owns restart).
type Lifetime int

const (
	Ephemeral Lifetime = iota
	Persistent
)

// XDisplayMode is the Pattern A → Pattern B knob from the plan.
type XDisplayMode int

const (
	XDisplayNone   XDisplayMode = iota // no display
	XDisplayShared                     // Pattern A — bind-mount the shared Xvfb :1 socket
	XDisplayOwned                      // Pattern B (Phase 5) — daemon spawns a sibling Xvfb
)

func (m XDisplayMode) String() string {
	switch m {
	case XDisplayNone:
		return "none"
	case XDisplayShared:
		return "shared"
	case XDisplayOwned:
		return "owned"
	default:
		return "unknown"
	}
}

// Mount is a single bind-mount (host path → sandbox path).
type Mount struct {
	HostPath    string
	SandboxPath string
	ReadOnly    bool
}

// EgressPolicy is the per-sandbox outbound allowlist. Empty lists with
// default-deny semantics mean "no egress". The FQDN list is resolved by
// the daemon DNS proxy at runtime; the IP/CIDR list is static.
type EgressPolicy struct {
	AllowFQDNs []string // e.g. "api.openai.com", "api.anthropic.com", "*.npmjs.org"
	AllowCIDRs []string // e.g. "10.0.0.0/8"
}

// SandboxManifest is the full declarative description of one sandbox.
// It is the only input to the pure argv/ruleset builders, which keeps
// them deterministic and unit-testable.
type SandboxManifest struct {
	ID       string
	Type     SandboxType
	Lifetime Lifetime
	Hostname string // UTS namespace hostname; defaults to ID if empty
	Mounts   []Mount
	Env      map[string]string
	Egress   EgressPolicy
	XDisplay XDisplayMode
	ParentID string // workers: their parent system's sandbox ID

	// HomeDir is the sandbox-internal $HOME (default /home/agent).
	HomeDir string
}

// sandboxIDPattern bounds the ID to what is safe to interpolate into a
// cgroup path, an nftables set name, a unix socket filename, and a UTS
// hostname without escaping any of them. This is a security boundary,
// not cosmetic: the ID flows into root-privileged operations.
var sandboxIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Validate rejects manifests that would produce an unsafe or
// nonsensical sandbox. It is intentionally strict — every field that
// reaches a privileged operation is checked here so the spawn path can
// assume a clean manifest.
func (m *SandboxManifest) Validate() error {
	if !sandboxIDPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid sandbox id %q: must match %s", m.ID, sandboxIDPattern)
	}
	switch m.Type {
	case TypeWorker, TypeSystem, TypeCustomerApp, TypeAgentShell:
	default:
		return fmt.Errorf("invalid sandbox type %d", m.Type)
	}
	if m.Type == TypeWorker && m.ParentID == "" {
		return fmt.Errorf("worker sandbox %q must declare a ParentID", m.ID)
	}
	if m.Type != TypeWorker && m.ParentID != "" {
		return fmt.Errorf("only worker sandboxes may set ParentID (%q is %s)", m.ID, m.Type)
	}
	for i, mt := range m.Mounts {
		if !strings.HasPrefix(mt.HostPath, "/") {
			return fmt.Errorf("mount[%d]: host path %q must be absolute", i, mt.HostPath)
		}
		if !strings.HasPrefix(mt.SandboxPath, "/") {
			return fmt.Errorf("mount[%d]: sandbox path %q must be absolute", i, mt.SandboxPath)
		}
		// Canonicalize before any path-based check. A raw textual prefix
		// match is bypassable via "/etc/x/../vibecraft" traversal or a
		// symlink whose target is a forbidden dir; resolving symlinks +
		// cleaning the path first closes both. Fail closed: if the path
		// cannot be resolved (does not exist yet), fall back to the
		// lexically-cleaned form so traversal is still defeated.
		canon := canonicalHostPath(mt.HostPath)
		// A bind-mount of the daemon's secret dirs into a sandbox would
		// defeat the entire plan; reject it structurally.
		if isForbiddenHostMount(canon) {
			return fmt.Errorf("mount[%d]: host path %q is never bind-mountable into a sandbox", i, mt.HostPath)
		}
		// Per-system credential scoping: a system (or customer-app)
		// manifest must not bind-mount another system's dir, the
		// credential dirs (~/.claude, ~/.codex), or the whole host home —
		// each would defeat the per-system scope the plan establishes. The
		// only home-tree path a scoped sandbox may mount is its own dir.
		if !mountWithinScope(m, canon) {
			return fmt.Errorf("mount[%d]: host path %q is outside the sandbox's own scope", i, mt.HostPath)
		}
	}
	for k := range m.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("invalid env var name %q", k)
		}
	}
	for _, f := range m.Egress.AllowFQDNs {
		if !isPlausibleFQDNPattern(f) {
			return fmt.Errorf("invalid egress FQDN pattern %q", f)
		}
	}
	for _, c := range m.Egress.AllowCIDRs {
		if !cidrPattern.MatchString(c) {
			return fmt.Errorf("invalid egress CIDR %q", c)
		}
	}
	switch m.XDisplay {
	case XDisplayNone, XDisplayShared, XDisplayOwned:
	default:
		return fmt.Errorf("invalid XDisplay mode %d", m.XDisplay)
	}
	return nil
}

// forbiddenHostPrefixes are host paths that must never appear as a
// bind-mount source — they are exactly the secrets/state the plan moves
// the trust boundary to protect. Mirrors the "What is not reachable
// from the sandbox" list in the plan's filesystem-layout section.
var forbiddenHostPrefixes = []string{
	"/etc/vibecraft",
	"/var/lib/vibecraft",
	"/home/vibecraft/.bashrc",
}

// hostHomeRoot is the host user's home. Scoped sandboxes (systems,
// customer-apps) may bind-mount their own dir under it but nothing else
// inside it — not the credential dirs (~/.claude, ~/.codex), not a
// sibling system's dir, not the whole home. Mirrors the host layout the
// plan and cloud-init establish.
const hostHomeRoot = "/home/vibecraft"

func isForbiddenHostMount(host string) bool {
	// Lexically clean (and trim a trailing slash) so a raw caller still
	// gets traversal defense; manifest.go additionally resolves symlinks
	// before calling this. filepath.Clean already strips trailing
	// slashes and resolves ".."/"." segments.
	clean := filepath.Clean(host)
	for _, p := range forbiddenHostPrefixes {
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return true
		}
	}
	return false
}

// canonicalHostPath resolves symlinks and cleans a host path so the
// path-based security checks operate on the real target, not a string
// that resolves elsewhere at mount time. EvalSymlinks requires the path
// to exist; when it does not (a not-yet-created mount source, or a test
// fixture path), we fall back to the lexically-cleaned form so traversal
// is still defeated. Fail closed by construction: the returned path is
// never "more permissive" than the input.
func canonicalHostPath(host string) string {
	if resolved, err := filepath.EvalSymlinks(host); err == nil {
		return resolved
	}
	return filepath.Clean(host)
}

// mountWithinScope enforces per-system/per-app credential scoping. For a
// system or customer-app sandbox, a mount that lands inside the host home
// tree is only allowed if it is the sandbox's own dir (HomeDir) or a path
// within it. This blocks a manifest from mounting ~/.claude, ~/.codex,
// another system's dir, or the whole home. Worker and agent-shell
// sandboxes are intentionally exempt: workers inherit their parent
// system's already-scoped dir, and the agent shell legitimately owns the
// whole home. host is expected to be canonicalized already.
func mountWithinScope(m *SandboxManifest, host string) bool {
	switch m.Type {
	case TypeSystem, TypeCustomerApp:
	default:
		return true
	}
	if !pathWithin(host, hostHomeRoot) {
		return true // outside the home tree (e.g. /opt/shared) — allowed
	}
	own := canonicalHostPath(m.HomeOrDefault())
	return pathWithin(host, own)
}

// pathWithin reports whether path is root itself or nested under root.
// Both are compared after a lexical clean so a trailing slash or a "."
// segment does not change the answer.
func pathWithin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+"/")
}

var (
	// A single FQDN label or a leading "*." wildcard, dotted.
	fqdnPattern = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	cidrPattern = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$`)
)

func isPlausibleFQDNPattern(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	return fqdnPattern.MatchString(s)
}

// HostnameOrID returns the UTS hostname to use (explicit Hostname, else
// the sandbox ID). Centralized so spawn.go and tests agree.
func (m *SandboxManifest) HostnameOrID() string {
	if m.Hostname != "" {
		return m.Hostname
	}
	return m.ID
}

// HomeOrDefault returns the sandbox $HOME (explicit HomeDir else the
// plan's /home/agent default).
func (m *SandboxManifest) HomeOrDefault() string {
	if m.HomeDir != "" {
		return m.HomeDir
	}
	return "/home/agent"
}

// AgentShellManifest builds the Phase 2 long-lived agent-shell sandbox:
// persistent (one per machine), $HOME backed by a host dir so state
// survives across the many commands executeBashTool runs, broad egress
// (the web is the agent's working surface), plus the system registry /
// tools bind-mounts. Still NO /etc/vibecraft, NO host loopback, NO
// host-bashrc key — Validate() enforces the forbidden set. extraMounts
// adds ~/systems, tool dirs, etc.
func AgentShellManifest(homeHostDir string, extraMounts []Mount, egress EgressPolicy) *SandboxManifest {
	// Bind the host home at the SAME path inside the sandbox. The daemon
	// hands the agent absolute paths (uploaded attachments at
	// /home/vibecraft/inbox/<conv>/<file>, ~/systems, ~/.claude); if the
	// sandbox mounted it elsewhere those would dangle and break file
	// recipes — a real prod-brick the empty-tmpdir EC2 test missed.
	// Identity mapping = every absolute path resolves identically in
	// and out of the sandbox.
	mounts := append([]Mount{{HostPath: homeHostDir, SandboxPath: homeHostDir}}, extraMounts...)
	return &SandboxManifest{
		ID:       "agent-shell",
		Type:     TypeAgentShell,
		Lifetime: Persistent,
		Mounts:   mounts,
		Egress:   egress,
		XDisplay: XDisplayShared, // the agent drives xterm/Chrome on the shared Xvfb (Pattern A)
		HomeDir:  homeHostDir,
	}
}

// SortedEnvKeys returns env var names in a stable order so the argv
// builder is deterministic (Go map iteration is randomized; an
// unstable argv would make the build untestable and harder to audit).
func (m *SandboxManifest) SortedEnvKeys() []string {
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
