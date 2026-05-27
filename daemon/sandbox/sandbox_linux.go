// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// sandbox_linux.go composes the proven pieces — netns+veth+nft
// (network_linux.go), the filtering DNS proxy (dnsproxy.go), the
// per-sandbox unix socket (socket.go), and the bwrap spawn
// (spawn_linux.go) — into one Create / Spawn / Destroy lifecycle. This
// is the daemon-facing primitive; everything under it is real-kernel
// integration-tested.

// idxAllocator hands out the 1..63 /30 slots, reusing freed ones.
type idxAllocator struct {
	mu   sync.Mutex
	used map[int]bool
}

var idxAlloc = &idxAllocator{used: map[int]bool{}}

func (a *idxAllocator) take() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 1; i <= 63; i++ {
		if !a.used[i] {
			a.used[i] = true
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free sandbox network slot (max 63 concurrent)")
}

func (a *idxAllocator) free(i int) {
	a.mu.Lock()
	delete(a.used, i)
	a.mu.Unlock()
}

// Sandbox is a live, isolated workload with controlled egress and a
// daemon channel. Build with Create, run commands with Run, tear down
// with Destroy (idempotent).
type Sandbox struct {
	Manifest  *SandboxManifest
	Net       *NetConfig
	idx       int
	sock      *SandboxServer
	resolv    string
	xSocket   string
	xvfb      *xvfbProc
	dns       *DNSProxy
	dnsCancel context.CancelFunc
	destroyed bool
	mu        sync.Mutex
}

// DisplayNum is the sandbox's private Xvfb display number (Pattern B),
// or 0 if it draws on the shared :1 (Pattern A). The daemon resolves a
// sandbox ID to this and builds computer.NewControllerForDisplay /
// NewScreenshotServiceForDisplay to drive/observe an owned-display
// workload on its own X server — without the same-shape agent tool
// surface changing. The agent-shell is Pattern A, so this is 0 for it
// (the live vision path stays on :1, unchanged).
func (s *Sandbox) DisplayNum() int {
	if s.xvfb == nil {
		return 0
	}
	return s.xvfb.display
}

// ObservedFQDNs is the set of out-of-policy names this sandbox reached
// in audit/discovery mode — the basis of the Phase 3 manifest proposal.
// Empty for an enforce-mode sandbox.
func (s *Sandbox) ObservedFQDNs() []string {
	if s.dns == nil {
		return nil
	}
	return s.dns.ObservedFQDNs()
}

// Create allocates a network slot, sets up the netns+veth+nft egress,
// starts the per-sandbox filtering DNS proxy, writes the sandbox
// resolv.conf, and binds the per-sandbox daemon socket served by
// handler (identity is the socket; see socket.go). Nothing is spawned
// yet — call Run/SpawnCmd. On any failure everything is rolled back.
func Create(m *SandboxManifest, mode EgressMode, handler http.Handler) (*Sandbox, error) {
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox %q: %w", m.ID, err)
	}
	idx, err := idxAlloc.take()
	if err != nil {
		return nil, err
	}
	s := &Sandbox{Manifest: m, idx: idx}

	s.Net, err = SetupNetwork(m.ID, idx, m.Egress, mode)
	if err != nil {
		idxAlloc.free(idx)
		return nil, err
	}

	dnsCtx, cancel := context.WithCancel(context.Background())
	s.dnsCancel = cancel
	if s.dns, err = StartDNSProxy(dnsCtx, m.ID, s.Net.HostIP, m.Egress, mode); err != nil {
		cancel()
		TeardownNetwork(m.ID, idx)
		idxAlloc.free(idx)
		return nil, fmt.Errorf("dns proxy: %w", err)
	}
	if s.resolv, err = WriteResolvConf(m.ID, s.Net.HostIP); err != nil {
		s.rollback()
		return nil, err
	}
	// Phase 5 Pattern B: a private Xvfb :N for this sandbox. Only its
	// own X<N> socket is bound in (BwrapArgs) — it cannot see :1.
	if m.XDisplay == XDisplayOwned {
		x, xerr := startXvfb()
		if xerr != nil {
			s.rollback()
			return nil, fmt.Errorf("owned display: %w", xerr)
		}
		s.xvfb = x
		s.xSocket = x.socket
		if m.Env == nil {
			m.Env = map[string]string{}
		}
		m.Env["DISPLAY"] = fmt.Sprintf(":%d", x.display)
	}
	if handler != nil {
		if s.sock, err = NewSandboxServer(m.ID, handler); err != nil {
			s.rollback()
			return nil, err
		}
		go s.sock.Serve()
	}
	return s, nil
}

func (s *Sandbox) rollback() {
	if s.dnsCancel != nil {
		s.dnsCancel()
	}
	if s.sock != nil {
		s.sock.Close()
	}
	if s.xvfb != nil {
		s.xvfb.stop()
	}
	// TeardownNetwork also removes /run/vc-resolv-<id>.conf.
	TeardownNetwork(s.Manifest.ID, s.idx)
	idxAlloc.free(s.idx)
}

// XSocket is the host X11 socket (/tmp/.X11-unix/X1) bound in when the
// manifest's XDisplay is Shared (Pattern A) so xterm/Chrome draw on the
// shared Xvfb. Set by NewAgentShell before first use.
func (s *Sandbox) SetXSocket(p string) { s.xSocket = p }

// SpawnOptsFor assembles the SpawnOpts wiring this sandbox's netns,
// resolv.conf, daemon socket, X socket, stdin, and extra env.
func (s *Sandbox) SpawnOptsFor(cmd []string, extraEnv map[string]string) SpawnOpts {
	o := SpawnOpts{
		NetnsName:      s.Net.NetnsName,
		ResolvConf:     s.resolv,
		ResolvConfDest: ResolvConfDest(),
		ExtraEnv:       extraEnv,
		Cmd:            cmd,
		XSocketPath:    s.xSocket,
	}
	if s.sock != nil {
		o.HostSocketPath = s.sock.Path()
	}
	return o
}

// Run spawns a command in this sandbox and waits, returning combined
// output (convenience for short commands / tests).
func (s *Sandbox) Run(ctx context.Context, cmd []string, extraEnv map[string]string) (string, error) {
	return Run(ctx, s.Manifest, s.SpawnOptsFor(cmd, extraEnv))
}

// RunStdin is Run with data fed to the process's stdin (text_editor
// ExecuteInteractive: secret-resolved content via stdin, never argv).
func (s *Sandbox) RunStdin(ctx context.Context, cmd []string, extraEnv map[string]string, stdin string) (string, error) {
	o := s.SpawnOptsFor(cmd, extraEnv)
	o.Stdin = stdin
	return Run(ctx, s.Manifest, o)
}

// Supported reports whether real sandboxing is available on this build/
// OS. Linux: true. Other OSes (the macOS dev host): false (stub).
func Supported() bool { return true }

// SpawnWorker is the one-call daemon-facing entrypoint: build a Worker
// manifest, Create the sandbox, run cmd inside it, and tear it down when
// cmd exits. parentSystem scopes the worker to its system's policy
// (inherited egress). This is what `vc-spawn-worker` / the engine call
// behind the VIBECRAFT_SANDBOXED_WORKERS flag (D3: explicit helper).
//
// NOTE: wiring this into engine.go's live worker fan-out is the
// plan-gated step (feature flag default off + the mandated production
// soak before the legacy `xterm -e claude` path is removed). This API +
// the flag make that swap a one-liner when the gate opens; it is
// deliberately NOT called from the hot path yet.
func SpawnWorker(ctx context.Context, name, parentSystem string, egress EgressPolicy, cmd []string, env map[string]string) (string, error) {
	m := &SandboxManifest{
		ID:       name,
		Type:     TypeWorker,
		Lifetime: Ephemeral,
		ParentID: parentSystem,
		Egress:   egress,
		XDisplay: XDisplayShared, // workers draw their xterm on the shared Xvfb (Pattern A)
	}
	sb, err := Create(m, EgressEnforce, nil)
	if err != nil {
		return "", err
	}
	defer sb.Destroy()
	return sb.Run(ctx, cmd, env)
}

// Destroy tears down everything Create built. Idempotent.
func (s *Sandbox) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return
	}
	s.destroyed = true
	s.rollback()
}
