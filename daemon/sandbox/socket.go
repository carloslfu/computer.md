// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

// socket.go is the per-sandbox daemon channel. The daemon binds one
// unix socket per sandbox at /run/vibecraft/<id>.sock and bwrap maps it
// to /run/vibecraft.sock inside (SpawnOpts.HostSocketPath). The daemon
// "knows which sandbox is calling" structurally — each socket is served
// by a handler closed over that sandbox's id, so identity is the socket,
// not a forgeable header. This replaces the host-loopback path the
// network namespace deliberately severs.
//
// stdlib net/http over a unix listener — no privileged calls — so it is
// unit-testable on any OS; the in-sandbox curl round-trip is covered by
// the Linux integration suite.

const socketDir = "/run/vibecraft"

// AgentShellID is the single long-lived sandbox that represents the
// manager's own shell. It is trusted to use socket-implicit localhost
// auth because the daemon creates it as the manager's computer-use
// surface. System/tool sandboxes get their own socket identity but not
// this privilege.
const AgentShellID = "agent-shell"

// SocketPath is the host-side per-sandbox socket path. Deterministic so
// the spawner (SpawnOpts.HostSocketPath) and the server agree.
func SocketPath(id string) string {
	return filepath.Join(socketDir, id+".sock")
}

// SandboxServer serves a sandbox-scoped HTTP API on the per-sandbox
// unix socket. The daemon constructs one per live sandbox.
type SandboxServer struct {
	id   string
	ln   net.Listener
	srv  *http.Server
	path string
}

// SandboxContextKey injects the calling sandbox id into the request
// context so scoped handlers can authorize by sandbox without trusting
// any client-supplied value.
type sandboxCtxKey struct{}

// SandboxID returns the calling sandbox id bound to this request's
// socket (always present for requests served by SandboxServer).
func SandboxID(r *http.Request) string {
	if v, ok := r.Context().Value(sandboxCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// ContextWithSandboxID returns a context tagged the same way SandboxServer
// tags requests accepted on a per-sandbox unix socket. It is exported for
// middleware tests; production callers should come through SandboxServer.
func ContextWithSandboxID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sandboxCtxKey{}, id)
}

// agentSockUser resolves the unprivileged user the agent-shell sandbox
// runs as (default "vibecraft", overridable via VIBECRAFT_SANDBOX_USER —
// kept in sync with spawn_linux.go hostAgentCreds). Cross-platform
// (os/user works everywhere); ok=false off a real machine so the chown
// is skipped and behavior is unchanged for tests / macOS dev.
func agentSockUser() (uid, gid int, ok bool) {
	name := os.Getenv("VIBECRAFT_SANDBOX_USER")
	if name == "" {
		name = "vibecraft"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, false
	}
	ui, e1 := strconv.Atoi(u.Uid)
	gi, e2 := strconv.Atoi(u.Gid)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return ui, gi, true
}

// NewSandboxServer creates (does not start) the per-sandbox socket
// server.
//
// Permissions: since v0.37.0 the agent-shell sandbox runs bwrap AS the
// host agent user (so its rootless userns can reach the bound real
// home). bwrap (now unprivileged) must therefore be able to traverse to
// AND open the per-sandbox socket to bind-mount it inside. So:
//   - the dir is 0755 (traversable). This does NOT widen the trust
//     boundary: a sandbox only ever gets its OWN socket FILE bind-mounted
//     in (never the dir), so it cannot enumerate or reach peers; the
//     real boundary is the netns + per-sandbox socket + socket-implicit
//     auth, and per the plan there are no other untrusted host users.
//   - the socket node is chowned to the agent user and kept 0600, so
//     only that user (the agent-shell identity) can connect. Root-run
//     sandboxes (workers/systems) bypass DAC, so this is universal.
//   - the daemon's already-open listener fd is unaffected by the chown.
func NewSandboxServer(id string, handler http.Handler) (*SandboxServer, error) {
	if !sandboxIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid sandbox id %q", id)
	}
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", socketDir, err)
	}
	// Tighten the dir to exactly 0755 even if a prior 0700 dir exists
	// (MkdirAll is a no-op on an existing dir, keeping its old mode).
	_ = os.Chmod(socketDir, 0755)
	path := SocketPath(id)
	_ = os.Remove(path) // clear a stale socket from a crashed prior run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	_ = os.Chmod(path, 0600)
	if uid, gid, okc := agentSockUser(); okc {
		// Owner = the agent-shell identity → it can connect to its own
		// socket; root-run sandboxes bypass perms anyway.
		_ = os.Chown(path, uid, gid)
	}

	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), sandboxCtxKey{}, id)
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	return &SandboxServer{
		id:   id,
		ln:   ln,
		path: path,
		srv:  &http.Server{Handler: wrapped, ReadHeaderTimeout: 10 * time.Second},
	}, nil
}

// Serve blocks serving the socket until Close. Run in a goroutine.
func (s *SandboxServer) Serve() error {
	err := s.srv.Serve(s.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the server and unlinks the socket.
func (s *SandboxServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

// Path is the host-side socket path (what SpawnOpts.HostSocketPath
// should be set to so bwrap maps it to /run/vibecraft.sock).
func (s *SandboxServer) Path() string { return s.path }
