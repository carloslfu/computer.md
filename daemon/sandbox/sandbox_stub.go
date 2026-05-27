// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"net/http"
)

// Non-Linux stub so the daemon (and any engine call site) builds on the
// macOS dev host. Real sandboxing needs Linux namespaces/nftables/veth;
// off Linux SpawnWorker is unavailable and the caller falls back to the
// legacy path (the VIBECRAFT_SANDBOXED_WORKERS flag is effectively off).

// ErrUnsupported is returned by SpawnWorker on non-Linux builds.
var ErrUnsupported = errors.New("sandboxing unsupported on this OS (Linux only)")

// Supported reports false on non-Linux builds.
func Supported() bool { return false }

// SpawnWorker is unavailable on non-Linux builds.
func SpawnWorker(ctx context.Context, name, parentSystem string, egress EgressPolicy, cmd []string, env map[string]string) (string, error) {
	return "", ErrUnsupported
}

// AgentShell stub so main.go references compile on the macOS dev build.
// NewAgentShell errors there → the engine keeps host bash (unchanged).
type AgentShell struct{}

func NewAgentShell(homeHost, xSocket string, extraMounts []Mount, env map[string]string, handler http.Handler) (*AgentShell, error) {
	return nil, ErrUnsupported
}

func (a *AgentShell) Execute(ctx context.Context, command string) (string, error) {
	return "", ErrUnsupported
}
func (a *AgentShell) ExecuteWithEnv(ctx context.Context, command string, extraEnv []string) (string, error) {
	return "", ErrUnsupported
}
func (a *AgentShell) ExecuteInteractive(ctx context.Context, command, input string) (string, error) {
	return "", ErrUnsupported
}
func (a *AgentShell) Close()       {}
func (a *AgentShell) Path() string { return "" }

// SystemEgress stub: discovery mode on non-Linux (no enforcement).
func SystemEgress(systemsRoot, name string) (EgressPolicy, EgressMode, error) {
	return EgressPolicy{}, EgressAudit, nil
}

// StartApp/StopApp stubs (non-Linux dev build).
func StartApp(appsRoot, name string, resolveVault func([]string) map[string]string, handler http.Handler) error {
	return ErrUnsupported
}
func StopApp(name string) {}

// EnsureCrontabOwnership is a no-op on non-Linux: the file lives on
// the dev host's normal user account, no namespace mapping to repair.
func EnsureCrontabOwnership(homeDir string) {}

// DestroySystemSandbox is a no-op on non-Linux: there is no per-system
// sandbox running on the dev host (the real implementation tears down
// bwrap/cron/supervisors that only exist under Linux namespaces).
func DestroySystemSandbox(name string) {}
