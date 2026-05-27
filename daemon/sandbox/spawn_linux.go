// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// resolveBin finds an external binary the privileged daemon must exec.
//
// A systemd service with no `Environment=PATH=` inherits systemd's
// DefaultPath. Whether that includes the sbin dirs is distro/version
// dependent — and `ip` lives in /usr/sbin. v0.27.0–v0.35.0 invoked the
// netns wrapper as the BARE name "ip"; on a daemon whose service PATH
// lacked /usr/sbin, `exec.LookPath("ip")` failed and EVERY agent bash
// command came back as empty output + a generic exec error (the process
// never started, so there was no stderr to capture). It was
// cap-independent and invisible to the integration suite, which runs
// from a shell with a full PATH. The daemon must therefore NEVER rely on
// ambient PATH for the binaries it shells out to — resolve to an
// absolute path once, preferring PATH but falling back to the known
// install locations, and never silently degrade to a bare name.
func resolveBin(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil && p != "" {
		return p
	}
	for _, f := range fallbacks {
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			return f
		}
	}
	// Last resort: return the bare name so the error (if any) names it.
	// The diagnostic wrapper in Run() makes the failure self-describing.
	return name
}

var (
	// Resolved once at package init. iproute2 ships /usr/sbin/ip (often
	// /sbin/ip too via usrmerge); bubblewrap ships /usr/bin/bwrap; the
	// Ubuntu tini package ships /usr/bin/tini. tini is also hardcoded
	// absolute in BwrapArgs — kept consistent here. setpriv ships in
	// util-linux (always present on Ubuntu) and drops the agent-shell to
	// the host agent user after the root-only `ip netns exec`.
	ipPath      = resolveBin("ip", "/usr/sbin/ip", "/sbin/ip", "/bin/ip")
	bwrapPath   = resolveBin("bwrap", "/usr/bin/bwrap", "/bin/bwrap", "/usr/local/bin/bwrap")
	setprivPath = resolveBin("setpriv", "/usr/bin/setpriv", "/bin/setpriv", "/usr/local/bin/setpriv")
)

// hostAgentCreds resolves the unprivileged host user the agent-shell
// sandbox must run as.
//
// Why this exists (the v0.27–v0.36 brick, finally surfaced by the
// self-describing Run()): the agent-shell binds the REAL /home/<user>
// (owned by that user, mode 0750). `bwrap --unshare-user` run as ROOT
// maps only 0→0, so that home is owned by an unmapped/overflow uid
// inside the userns and `--chdir` into it returns EPERM
// (`bwrap: Can't chdir to /home/vibecraft: Permission denied`) —
// breaking every agent bash command. Running bwrap AS the user makes
// its rootless userns map host-<uid> → sandbox, so the real home is
// accessible. That is the standard bubblewrap model and exactly what
// the proven legacy `computer.Shell` path does via SysProcAttr creds.
//
// ok=false on a host where the user doesn't exist (macOS dev / CI
// runner): callers degrade to the prior root behavior rather than
// break — correctness on real machines, no build/test breakage off
// them. Overridable via VIBECRAFT_SANDBOX_USER (default "vibecraft").
func hostAgentCreds() (uid, gid string, ok bool) {
	name := os.Getenv("VIBECRAFT_SANDBOX_USER")
	if name == "" {
		name = "vibecraft"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", "", false
	}
	return u.Uid, u.Gid, true
}

// SpawnCmd builds (does not start) the *exec.Cmd that launches the
// sandbox: `bwrap <BwrapArgs>` — or, when the daemon has prepared a veth
// network namespace, `ip netns exec <ns> bwrap <BwrapArgs>` so the
// sandbox lands in that netns (its only egress path, policed by the
// per-sandbox nftables rules). The manifest is validated first; an
// invalid manifest never reaches a privileged exec. Both `ip` and
// `bwrap` are absolute (resolveBin) — a daemon must not depend on the
// systemd service PATH including /usr/sbin.
func SpawnCmd(ctx context.Context, m *SandboxManifest, opts SpawnOpts) (*exec.Cmd, error) {
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox %q: invalid manifest: %w", m.ID, err)
	}
	if len(opts.Cmd) == 0 {
		return nil, fmt.Errorf("sandbox %q: empty Cmd", m.ID)
	}
	args := BwrapArgs(m, opts)

	// The agent-shell and system sandboxes bind real /home/<user> paths;
	// bwrap must run AS that user so its rootless userns maps the home
	// owner (otherwise `--chdir` into the bound home EPERMs — see
	// hostAgentCreds). The drop happens AFTER the root-only `ip netns exec`
	// (which performs the netns setns) and BEFORE bwrap, via setpriv.
	// Ephemeral workers and customer apps keep the root path unchanged.
	var dropPrefix []string
	if m.Type == TypeAgentShell || m.Type == TypeSystem {
		if uid, gid, okc := hostAgentCreds(); okc {
			dropPrefix = []string{setprivPath, "--reuid=" + uid, "--regid=" + gid, "--init-groups"}
		}
	}

	// bwrapAndArgs = [<setpriv drop...>] bwrap <BwrapArgs...>
	bwrapAndArgs := make([]string, 0, len(dropPrefix)+1+len(args))
	bwrapAndArgs = append(bwrapAndArgs, dropPrefix...)
	bwrapAndArgs = append(bwrapAndArgs, bwrapPath)
	bwrapAndArgs = append(bwrapAndArgs, args...)

	var name string
	var argv []string
	if opts.NetnsName != "" {
		name = ipPath
		argv = append([]string{"netns", "exec", opts.NetnsName}, bwrapAndArgs...)
	} else {
		name = bwrapAndArgs[0]
		argv = bwrapAndArgs[1:]
	}
	return exec.CommandContext(ctx, name, argv...), nil
}

// Run spawns the sandbox, waits for it to exit, and returns combined
// stdout+stderr. Used by the integration tests to run a probe command
// inside a real sandbox and assert on what it can/cannot see.
//
// On failure the error is wrapped with the program, a short argv head,
// the captured output, and whether the program even resolved — and the
// same line is logged to the journal. The opaque "empty output, exit
// status 1" that hid the v0.27–v0.35 PATH brick must never recur: a
// spawn failure has to name itself.
func Run(ctx context.Context, m *SandboxManifest, opts SpawnOpts) (string, error) {
	cmd, err := SpawnCmd(ctx, m, opts)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}
	runErr := cmd.Run()
	out := buf.String()
	if runErr != nil {
		prog := cmd.Path
		resolved := "resolved"
		if !filepath.IsAbs(prog) {
			if _, le := exec.LookPath(prog); le != nil {
				resolved = "NOT FOUND in PATH"
			}
		} else if _, se := os.Stat(prog); se != nil {
			resolved = "absolute path missing"
		}
		head := cmd.Args
		if len(head) > 8 {
			head = head[:8]
		}
		diag := fmt.Sprintf("sandbox %q spawn failed: %v | prog=%s (%s) | argv[0:8]=%v | output=%q",
			m.ID, runErr, prog, resolved, head, truncate(out, 800))
		log.Print("sandbox: " + diag)
		// Surface a non-empty, self-describing result so the failure is
		// visible in the chat UI without root journal access.
		if out == "" {
			out = diag
		}
		return out, fmt.Errorf("%s", diag)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
