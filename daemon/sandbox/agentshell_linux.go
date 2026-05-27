// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	agentShellRunDir       = "/run/vibecraft-agent-shell"
	agentShellDefaultLimit = 120 * time.Second
	agentShellMaxOutput    = 1024 * 1024
)

// AgentShell is the Phase 2 long-lived agent-shell sandbox presented as
// a drop-in for the engine's bash surface (core.ShellExecutor). The
// agent's entire world — every bash command, every text_editor write,
// the xterm/Chrome it drives — runs inside ONE persistent sandbox:
//   - $HOME backed by the real /home/vibecraft (systems, ~/.claude,
//     ~/inbox, tools persist across commands),
//   - shared Xvfb socket bound (Pattern A) so xterm/Chrome still draw,
//   - audit-mode egress (full connectivity for the trusted-ish agent,
//     logged) while the netns still severs host 127.0.0.1:8420,
//   - /etc/vibecraft masked, no host .bashrc key, no host PID view.
//
// The agent reaches the daemon via the per-sandbox /run/vibecraft.sock
// (handler passed in by main.go = the daemon mux; socket-implicit auth)
// instead of host loopback.
type AgentShell struct {
	sb      *Sandbox
	cancel  context.CancelFunc // stops the catch-all cron
	runHost string
	super   *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
}

// NewAgentShell creates the persistent agent-shell sandbox. homeHost is
// the host dir backing $HOME (/home/vibecraft in production). xSocket is
// the shared Xvfb socket (/tmp/.X11-unix/X1). env is the base
// environment (PATH, DISPLAY, etc.). Hosted provider keys and host-loopback
// daemon tokens are not part of this environment. handler serves the
// per-sandbox unix socket.
func NewAgentShell(homeHost, xSocket string, extraMounts []Mount, env map[string]string, handler http.Handler) (*AgentShell, error) {
	// The agent-shell is the catch-all scheduler: host crontab entries
	// migrate into its crontab file (Phase 3) and the manager edits it
	// for its own scheduled work. The file lives in $HOME, already bound
	// at the same path — no spool dir / extra mount.
	if err := ensureCrontab(homeHost); err != nil {
		return nil, err
	}
	runHost, err := prepareAgentShellRunDir()
	if err != nil {
		return nil, err
	}
	extraMounts = append(extraMounts, Mount{HostPath: runHost, SandboxPath: agentShellRunDir})
	// Google Chrome's wrapper lives in /usr/bin, but the actual binary
	// and resources live under /opt/google. Bind only that vendor-owned
	// subtree when present; do not expose arbitrary /opt contents.
	extraMounts = appendExistingReadOnlyMounts(extraMounts, "/opt/google")
	m := AgentShellManifest(homeHost, extraMounts, EgressPolicy{})
	m.Env = env
	// Audit mode = log-and-accept: the agent (trusted-ish, needs the
	// open web for Chrome) gets full egress with an audit trail; the
	// per-sandbox netns still makes host loopback unreachable, which is
	// the threat that matters for the agent's own bash (Threat #2).
	sb, err := Create(m, EgressAudit, handler)
	if err != nil {
		_ = os.RemoveAll(runHost)
		return nil, err
	}
	sb.SetXSocket(xSocket)
	super, done, err := startAgentShellSupervisor(sb, runHost)
	if err != nil {
		sb.Destroy()
		_ = os.RemoveAll(runHost)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go superviseCron(ctx, sb, homeHost, "agent-shell")
	return &AgentShell{sb: sb, cancel: cancel, runHost: runHost, super: super, done: done}, nil
}

// Path is the host-side per-sandbox socket path (for diagnostics).
func (a *AgentShell) Path() string {
	if a.sb.sock != nil {
		return a.sb.sock.Path()
	}
	return ""
}

// Close tears the agent-shell sandbox down (daemon shutdown only — it
// is long-lived for the daemon's lifetime).
func (a *AgentShell) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	if a.super != nil && a.super.Process != nil {
		_ = a.super.Process.Signal(syscall.SIGTERM)
		select {
		case <-a.done:
		case <-time.After(2 * time.Second):
			_ = a.super.Process.Kill()
			<-a.done
		}
	}
	a.sb.Destroy()
	if a.runHost != "" {
		_ = os.RemoveAll(a.runHost)
	}
}

// --- core.ShellExecutor ---

func (a *AgentShell) Execute(ctx context.Context, command string) (string, error) {
	return a.run(ctx, command, nil, "")
}

func (a *AgentShell) ExecuteWithEnv(ctx context.Context, command string, extraEnv []string) (string, error) {
	return a.run(ctx, command, kvSliceToMap(extraEnv), "")
}

func (a *AgentShell) ExecuteInteractive(ctx context.Context, command, input string) (string, error) {
	return a.run(ctx, command, nil, input)
}

func (a *AgentShell) run(ctx context.Context, command string, extraEnv map[string]string, stdin string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := os.Stat(filepath.Join(a.runHost, "ready")); err != nil {
		return "", fmt.Errorf("agent-shell supervisor not ready: %w", err)
	}

	runCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		runCtx, cancel = context.WithTimeout(ctx, agentShellDefaultLimit+5*time.Second)
	}
	defer cancel()

	id, err := randomAgentShellID()
	if err != nil {
		return "", err
	}
	req := agentShellRequestPaths(a.runHost, id)
	defer cleanupAgentShellRequest(req)

	if err := writeAgentShellRequestFile(req.cmd, []byte(command)); err != nil {
		return "", err
	}
	if stdin != "" {
		if err := writeAgentShellRequestFile(req.stdin, []byte(stdin)); err != nil {
			return "", err
		}
	}
	if len(extraEnv) > 0 {
		if err := writeAgentShellRequestFile(req.env, []byte(renderShellEnv(extraEnv))); err != nil {
			return "", err
		}
	}
	if err := signalAgentShellSupervisor(a.runHost, id); err != nil {
		return "", err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return readAgentShellOutput(req.out), runCtx.Err()
		case <-ticker.C:
			if _, err := os.Stat(req.done); err == nil {
				out := readAgentShellOutput(req.out)
				codeBytes, _ := os.ReadFile(req.done)
				code := strings.TrimSpace(string(codeBytes))
				if code != "" && code != "0" {
					return out, fmt.Errorf("exit status %s", code)
				}
				return out, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return readAgentShellOutput(req.out), err
			}
		}
	}
}

type agentShellRequest struct {
	cmd   string
	env   string
	stdin string
	out   string
	done  string
}

func agentShellRequestPaths(root, id string) agentShellRequest {
	base := filepath.Join(root, id)
	return agentShellRequest{
		cmd:   base + ".sh",
		env:   base + ".env",
		stdin: base + ".stdin",
		out:   base + ".out",
		done:  base + ".done",
	}
}

func cleanupAgentShellRequest(req agentShellRequest) {
	_ = os.Remove(req.cmd)
	_ = os.Remove(req.env)
	_ = os.Remove(req.stdin)
	_ = os.Remove(req.out)
	_ = os.Remove(req.done)
}

func writeAgentShellRequestFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return chownAgentShellRequestFile(path)
}

func chownAgentShellRequestFile(path string) error {
	uidStr, gidStr, ok := hostAgentCreds()
	if !ok {
		return nil
	}
	uid, uerr := strconv.Atoi(uidStr)
	gid, gerr := strconv.Atoi(gidStr)
	if uerr != nil || gerr != nil {
		return nil
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return err
	}
	if int(st.Uid) == uid && int(st.Gid) == gid {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown agent-shell request file %s: %w", path, err)
	}
	return nil
}

func randomAgentShellID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func prepareAgentShellRunDir() (string, error) {
	dir, err := os.MkdirTemp("/run", "vibecraft-agent-shell-*")
	if err != nil {
		return "", err
	}
	if uid, gid, ok := hostAgentCreds(); ok {
		uidNum, uerr := strconv.Atoi(uid)
		gidNum, gerr := strconv.Atoi(gid)
		if uerr == nil && gerr == nil {
			if err := os.Chown(dir, uidNum, gidNum); err != nil {
				_ = os.RemoveAll(dir)
				return "", err
			}
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func startAgentShellSupervisor(sb *Sandbox, runHost string) (*exec.Cmd, chan struct{}, error) {
	logFile, err := os.OpenFile(filepath.Join(runHost, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	cmd, err := SpawnCmd(context.Background(), sb.Manifest, sb.SpawnOptsFor([]string{"/bin/bash", "-lc", agentShellSupervisorScript()}, nil))
	if err != nil {
		_ = logFile.Close()
		return nil, nil, err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Printf("agent-shell supervisor exited: %v", err)
		}
		_ = logFile.Close()
		close(done)
	}()

	ready := filepath.Join(runHost, "ready")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return cmd, done, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	<-done
	return nil, nil, fmt.Errorf("agent-shell supervisor did not become ready")
}

func agentShellSupervisorScript() string {
	return `set -euo pipefail
run_dir=` + shellQuote(agentShellRunDir) + `
queue="$run_dir/queue"
rm -f "$queue"
mkfifo "$queue"
chmod 600 "$queue"
: > "$run_dir/ready"
while IFS= read -r id < "$queue"; do
  (
    cmd_file="$run_dir/$id.sh"
    env_file="$run_dir/$id.env"
    stdin_file="$run_dir/$id.stdin"
    out_file="$run_dir/$id.out"
    done_file="$run_dir/$id.done"
    code=0
    cd "$HOME"
    if [ -f "$env_file" ]; then
      . "$env_file"
    fi
    if [ -f "$stdin_file" ]; then
      timeout --preserve-status 120s /bin/bash "$cmd_file" < "$stdin_file" > "$out_file" 2>&1 || code=$?
    else
      timeout --preserve-status 120s /bin/bash "$cmd_file" < /dev/null > "$out_file" 2>&1 || code=$?
    fi
    printf '%s' "$code" > "$done_file"
  )
done
`
}

func signalAgentShellSupervisor(root, id string) error {
	queue, err := os.OpenFile(filepath.Join(root, "queue"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer queue.Close()
	_, err = queue.WriteString(id + "\n")
	return err
}

func renderShellEnv(env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(env[k]))
		b.WriteByte('\n')
	}
	return b.String()
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readAgentShellOutput(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf, _ := io.ReadAll(io.LimitReader(f, agentShellMaxOutput+1))
	out := string(buf[:min(len(buf), agentShellMaxOutput)])
	if len(buf) > agentShellMaxOutput {
		out += "\n[output truncated]"
	}
	return out
}

func kvSliceToMap(kv []string) map[string]string {
	if len(kv) == 0 {
		return nil
	}
	m := make(map[string]string, len(kv))
	for _, e := range kv {
		if i := strings.IndexByte(e, '='); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}
