// SPDX-License-Identifier: Apache-2.0

package computer

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultTimeout is the maximum time a shell command can run.
	DefaultTimeout = 120 * time.Second

	// MaxOutputSize is the maximum output size in bytes (1 MB).
	MaxOutputSize = 1024 * 1024
)

// Shell executes bash commands with timeouts and output limits.
// When configured with a run-as user, commands are executed with
// dropped privileges for defense-in-depth security.
type Shell struct {
	timeout    time.Duration
	workDir    string
	credential *syscall.Credential
	env        []string
}

// NewShell creates a Shell with the given working directory.
// If runAs is non-empty, commands will be executed as that user
// (dropping privileges from the root daemon process). If the user
// is not found (e.g. local development), commands run as the
// daemon's own user with a warning.
func NewShell(workDir string, runAs string) *Shell {
	s := &Shell{
		timeout: DefaultTimeout,
		workDir: workDir,
	}

	if runAs == "" {
		return s
	}

	u, err := user.Lookup(runAs)
	if err != nil {
		log.Printf("warning: user %q not found, shell commands will run as daemon user: %v", runAs, err)
		return s
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		log.Printf("warning: invalid uid for user %q: %v", runAs, err)
		return s
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		log.Printf("warning: invalid gid for user %q: %v", runAs, err)
		return s
	}

	// Collect supplementary groups the run-as user belongs to.
	var groups []uint32
	groupIDs, _ := u.GroupIds()
	for _, g := range groupIDs {
		gidNum, err := strconv.ParseUint(g, 10, 32)
		if err == nil {
			groups = append(groups, uint32(gidNum))
		}
	}

	s.credential = &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    uint32(gid),
		Groups: groups,
	}

	// Include ~/.npm-global/bin so user-installed npm globals (Claude Code,
	// any other CLI the customer wants their manager to use) resolve in
	// every shell command — including `xterm -e <cmd>` where xterm execs
	// the command directly without going through a login shell. Without
	// this, `claude` is "command not found" on fresh machines and the
	// worker fan-out fails silently.
	//
	// Provider keys are intentionally not exported here. Workers and
	// customer shells authenticate through subscription/session state or
	// customer-owned vault/env choices; the hosted manager key remains a
	// root daemon secret.
	pathEntries := []string{
		u.HomeDir + "/.npm-global/bin",
		u.HomeDir + "/.local/bin",
		"/usr/local/sbin",
		"/usr/local/bin",
		"/usr/sbin",
		"/usr/bin",
		"/sbin",
		"/bin",
	}
	s.env = []string{
		"HOME=" + u.HomeDir,
		"USER=" + runAs,
		"LOGNAME=" + runAs,
		"SHELL=/bin/bash",
		"DISPLAY=:1",
		"PATH=" + strings.Join(pathEntries, ":"),
		"LANG=en_US.UTF-8",
		"XDG_RUNTIME_DIR=/run/user/" + u.Uid,
	}
	// Codex (the peer worker) authenticates through `codex login`
	// (ChatGPT subscription); the token lands in ~/.codex/ and is
	// picked up automatically — we don't inject an OpenAI key into
	// the shell. Same posture Claude Code uses on BYOM subscription
	// auth.
	// Local daemon access for the manager should go through the
	// agent-shell's per-sandbox /run/vibecraft.sock channel. Do not inject
	// VIBECRAFT_LOCAL_TOKEN into normal shell/worker env.

	log.Printf("shell: commands will run as user %q (uid=%s gid=%s groups=%d)", runAs, u.Uid, u.Gid, len(groups))
	return s
}

// procAttr returns a SysProcAttr with process group isolation
// and optional credential for privilege dropping.
func (s *Shell) procAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if s.credential != nil {
		attr.Credential = s.credential
	}
	return attr
}

// buildEnv composes the child process environment: the shell's base env
// (the dropped-privilege env when a run-as user is configured, otherwise the
// daemon's inherited environment) plus any per-command extra entries.
//
// extraEnv carries vault secrets. It is appended last (a later duplicate key
// wins in execve) and only ever reaches the child via envp[], never argv —
// that is the whole point of the env-not-argv secret path (Phase 0a).
//
// When there are no extra entries the original "nil Env means inherit" path
// is preserved exactly, so non-secret commands behave identically to before.
func (s *Shell) buildEnv(extraEnv []string) []string {
	if len(extraEnv) == 0 {
		if s.env == nil {
			return nil // exec inherits the daemon's environment (unchanged)
		}
		return s.env
	}
	var base []string
	if s.env != nil {
		base = s.env
	} else {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+len(extraEnv))
	out = append(out, base...)
	out = append(out, extraEnv...)
	return out
}

// Execute runs a bash command and returns its combined stdout/stderr output.
// Commands are run with a timeout. If the output exceeds MaxOutputSize, it is truncated.
func (s *Shell) Execute(ctx context.Context, command string) (string, error) {
	return s.executeWithEnv(ctx, command, nil)
}

// ExecuteWithEnv is Execute with additional environment entries appended for
// this command only (used to deliver vault secrets via envp[] instead of
// substituting them into the command string). The command runs verbatim;
// bash expands $NAME from the injected environment at exec time.
func (s *Shell) ExecuteWithEnv(ctx context.Context, command string, extraEnv []string) (string, error) {
	return s.executeWithEnv(ctx, command, extraEnv)
}

func (s *Shell) executeWithEnv(ctx context.Context, command string, extraEnv []string) (string, error) {
	// Create a timeout context if parent doesn't already have a shorter deadline.
	execCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-c", command)
	cmd.Dir = s.workDir
	cmd.SysProcAttr = s.procAttr()
	cmd.Env = s.buildEnv(extraEnv)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// WaitDelay prevents cmd.Run() from hanging when the command spawns
	// background processes (e.g. `xterm &`) that inherit stdout/stderr.
	// Without this, cmd.Wait() blocks forever waiting for those inherited
	// file descriptors to close, even after bash itself has exited.
	// With WaitDelay, the pipes are forcibly closed 5 seconds after the
	// direct child exits, and any remaining children are killed.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()

	// Combine output.
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	// Truncate if too large (UTF-8 safe).
	if len(output) > MaxOutputSize {
		truncated := output[:MaxOutputSize]
		// Walk back to avoid splitting a multi-byte UTF-8 character.
		for i := len(truncated) - 1; i >= len(truncated)-4 && i >= 0; i-- {
			if truncated[i] < 0x80 || truncated[i] >= 0xC0 {
				truncated = truncated[:i+1]
				break
			}
		}
		output = truncated + "\n... [output truncated]"
	}

	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			// Kill the process group on timeout.
			if cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return output, fmt.Errorf("command timed out after %s", s.timeout)
		}

		// Include exit code in error.
		if exitErr, ok := err.(*exec.ExitError); ok {
			return output, fmt.Errorf("exit code %d", exitErr.ExitCode())
		}
		return output, fmt.Errorf("command failed: %w", err)
	}

	return output, nil
}

// ExecuteWithTimeout runs a command with a specific timeout override.
func (s *Shell) ExecuteWithTimeout(ctx context.Context, command string, timeout time.Duration) (string, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.Execute(execCtx, command)
}

// ExecuteInteractive starts a command and returns after sending input.
// Used for commands that need stdin input.
func (s *Shell) ExecuteInteractive(ctx context.Context, command, input string) (string, error) {
	execCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-c", command)
	cmd.Dir = s.workDir
	cmd.SysProcAttr = s.procAttr()

	if s.env != nil {
		cmd.Env = s.env
	}

	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// See Execute() for explanation — prevents hangs on background processes
	// that inherit stdout/stderr.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if len(output) > MaxOutputSize {
		output = output[:MaxOutputSize] + "\n... [output truncated]"
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return output, fmt.Errorf("exit code %d", exitErr.ExitCode())
		}
		return output, err
	}

	return output, nil
}

// IsCommandAvailable checks if a command exists on the system.
func IsCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
