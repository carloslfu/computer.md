// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"sort"
	"strings"
)

// SpawnOpts carries the runtime-resolved values the daemon supplies at
// spawn time (kept separate from the manifest so BwrapArgs stays a pure,
// deterministic, unit-testable function).
type SpawnOpts struct {
	// HostSocketPath, if set, is bind-mounted to /run/vibecraft.sock so
	// the in-sandbox agent can reach the daemon over the per-sandbox
	// socket instead of host loopback.
	HostSocketPath string

	// XSocketPath, if set (Pattern A + XDisplayShared), is the host
	// /tmp/.X11-unix/X<n> socket to bind in so xterm can draw on the
	// shared Xvfb.
	XSocketPath string

	// ResolvConf, if set, is the host file bound in as the sandbox's
	// resolv.conf (points at the per-sandbox gateway DNS proxy). The
	// host's /etc/resolv.conf is a systemd-resolved symlink that bwrap
	// cannot overlay and 127.0.0.53 is unreachable from the netns — a
	// real-kernel finding. ResolvConfDest is WHERE to bind it (the
	// symlink-resolved real path, from sandbox.ResolvConfDest());
	// defaults to /etc/resolv.conf when empty.
	ResolvConf     string
	ResolvConfDest string

	// NetnsName, if set, means the daemon already created a network
	// namespace with a veth into the bridge; the caller runs bwrap via
	// `ip netns exec <NetnsName>` and BwrapArgs therefore must NOT add
	// --unshare-net (the prepared netns IS the isolation + the only
	// egress path, governed by the per-sandbox nftables rules). If
	// empty, BwrapArgs adds --unshare-net (fully isolated: lo only).
	NetnsName string

	// ExtraEnv is appended after the manifest env (e.g. customer-owned app
	// credentials). Delivered via --setenv, never argv.
	ExtraEnv map[string]string

	// Cmd is the entrypoint argv to run inside the sandbox.
	Cmd []string

	// Stdin, if non-empty, is fed to the sandboxed process's stdin
	// (used by the text_editor path's ExecuteInteractive — content via
	// stdin, never argv, matching Phase 0a's secret discipline).
	Stdin string
}

// Base system directories bind-mounted read-only from the host image.
// /etc is bound ro then /etc/vibecraft is masked with a tmpfs (see
// below) — a wholesale /etc ro-bind WOULD expose the daemon's secrets,
// which a real-kernel smoke test caught. /var/lib/vibecraft and
// /home/vibecraft are never bound at all.
var roSystemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}

// BwrapArgs builds the full bubblewrap argv for a (validated) manifest.
// Pure and deterministic — same inputs produce a byte-identical argv, so
// it is unit-tested on any OS and auditable in review. The privileged
// exec of this argv lives in spawn_linux.go.
//
// Real-kernel learnings baked in (proven on an EC2 Ubuntu 24.04 / kernel
// 6.17 box, bubblewrap 0.9.0, tini 0.19.0):
//   - tini must be PID 1 AND a subreaper or zombie reaping is silently
//     off (tini prints a warning otherwise). So: `--as-pid-1 -- tini -s
//     -- <cmd>`.
//   - `--ro-bind /etc /etc` exposes /etc/vibecraft; mask it with an
//     overlaid `--tmpfs /etc/vibecraft`.
//   - `--clearenv` first so the sandbox never inherits the daemon's
//     environment (which may hold daemon/provider secrets); only explicitly
//     --setenv'd vars are visible — the same env discipline as Phase 0a.
//   - `--unshare-user` is the isolation CORNERSTONE and is non-negotiable:
//     even run as root, bwrap 0.9.0 writes setgroups=deny for the new
//     user namespace. A consequence (proven on the real kernel) is that
//     Debian/Vixie `cron`'s per-job setgid/initgroups/setuid drop hits
//     EPERM on setgroups and the job dies before exec. The fix is NOT to
//     drop `--unshare-user` (that makes a compromised sandbox real host
//     root behind a fragile cap blacklist) — it is to schedule with
//     `supercronic`, which runs jobs as the sandbox identity with no
//     privilege-drop. See cron_linux.go + daemon/SECURITY.md.
func BwrapArgs(m *SandboxManifest, opts SpawnOpts) []string {
	a := []string{
		"--unshare-user",
		"--unshare-ipc",
		"--unshare-pid",
		"--unshare-uts",
		"--unshare-cgroup",
	}
	if opts.NetnsName == "" {
		// No prepared veth netns → fully isolated network (lo only).
		a = append(a, "--unshare-net")
	}
	a = append(a, "--hostname", m.HostnameOrID())
	a = append(a, "--die-with-parent")
	a = append(a, "--clearenv")

	// Read-only system image.
	for _, d := range roSystemDirs {
		a = append(a, "--ro-bind", d, d)
	}
	a = append(a, "--ro-bind", "/etc", "/etc")
	// Mask the daemon's on-host secret/state locations. The tmpfs wins
	// over the /etc ro-bind because it is applied after it.
	a = append(a, "--tmpfs", "/etc/vibecraft")
	// Working resolver for the sandbox netns (host stub is unreachable).
	// Bind to the symlink-resolved real path so bwrap doesn't choke on
	// Ubuntu's /etc/resolv.conf -> /run/systemd/resolve/... symlink.
	if opts.ResolvConf != "" {
		dest := opts.ResolvConfDest
		if dest == "" {
			dest = "/etc/resolv.conf"
		}
		a = append(a, "--ro-bind", opts.ResolvConf, dest)
	}

	// Kernel/process surfaces.
	a = append(a, "--proc", "/proc")
	a = append(a, "--dev", "/dev")
	a = append(a, "--tmpfs", "/tmp")

	// Sandbox $HOME. Ephemeral workers get a private tmpfs; a persistent
	// sandbox (agent-shell, Phase 2) declares a bind-mount AT the home
	// path so state survives across commands — in that case skip the
	// tmpfs so the bind is the home (not hidden-but-shadowed layering).
	home := m.HomeOrDefault()
	homeBound := false
	for _, mt := range m.Mounts {
		if mt.SandboxPath == home {
			homeBound = true
			break
		}
	}
	if !homeBound {
		a = append(a, "--tmpfs", home)
	}

	// Manifest bind-mounts (project dirs, ~/.claude views, …). Validate()
	// rejects forbidden host SOURCES, but it cannot defend the bind
	// DESTINATION: a mount whose SandboxPath is /etc/vibecraft (or /etc
	// itself) would be applied AFTER the --tmpfs /etc/vibecraft mask and
	// the --ro-bind /etc above and SHADOW them — re-exposing the daemon
	// secret/state locations inside the sandbox. Validate() also string-
	// matches HostPath, which a symlinked source can slip past (the
	// resolved source is what bwrap actually binds). So the builder
	// fails CLOSED here: any mount whose source OR destination resolves
	// onto a forbidden path is DROPPED, never bound — defense in depth
	// independent of Validate(). (Symlink resolution of the source is a
	// filesystem op and must also happen in Validate(); see manifest.go.)
	for _, mt := range m.Mounts {
		if isForbiddenHostMount(mt.HostPath) || isForbiddenSandboxDest(mt.SandboxPath) {
			continue
		}
		if mt.ReadOnly {
			a = append(a, "--ro-bind", mt.HostPath, mt.SandboxPath)
		} else {
			a = append(a, "--bind", mt.HostPath, mt.SandboxPath)
		}
	}

	// Per-sandbox daemon socket.
	if opts.HostSocketPath != "" {
		a = append(a, "--bind", opts.HostSocketPath, "/run/vibecraft.sock")
	}
	// X socket: Pattern A (shared :1) OR Pattern B (this sandbox's own
	// private Xvfb :N). Either way ONLY the one socket is bound in — a
	// Pattern-B sandbox literally cannot see /tmp/.X11-unix/X1.
	if m.XDisplay != XDisplayNone && opts.XSocketPath != "" {
		a = append(a, "--ro-bind", opts.XSocketPath, opts.XSocketPath)
	}

	// Environment: manifest env first, then opts.ExtraEnv (so a
	// per-spawn secret can override), both via --setenv (never argv).
	a = append(a, "--setenv", "HOME", home)
	for _, k := range m.SortedEnvKeys() {
		a = append(a, "--setenv", k, m.Env[k])
	}
	for _, k := range sortedKeys(opts.ExtraEnv) {
		a = append(a, "--setenv", k, opts.ExtraEnv[k])
	}

	a = append(a, "--chdir", home)

	// tini as real PID 1 + subreaper (the reaping fix).
	a = append(a, "--as-pid-1", "--", "/usr/bin/tini", "-s", "--")
	a = append(a, opts.Cmd...)
	return a
}

// forbiddenSandboxDests are bind-mount DESTINATIONS that must never be
// declared by a manifest: binding onto them inside the sandbox would
// shadow a protection BwrapArgs sets up earlier — the --tmpfs mask over
// /etc/vibecraft, the --ro-bind /etc base, or the read-only system image
// (which carries /usr/bin/tini, the sandbox's PID 1). A mount targeting
// any of these is dropped rather than bound (fail closed).
var forbiddenSandboxDests = []string{
	"/etc",
	"/etc/vibecraft",
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
}

func isForbiddenSandboxDest(dest string) bool {
	clean := strings.TrimRight(dest, "/")
	for _, p := range forbiddenSandboxDests {
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
