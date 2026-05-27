// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// bootstrapTools installs critical packages the agent assumes are on the
// machine. Runs at daemon startup, as root (the daemon's systemd unit
// has no User= directive). The bash tool drops privileges to the
// vibecraft user, which has no sudo — so the agent itself cannot
// apt-install. This block is the system-level escape hatch.
//
// Scope is deliberately narrow: only tools that materially break the
// agent's core workflow if missing, and that the install paths
// (cloud-init.ts / install.sh) already install at provisioning time.
// This is the existing-machine-rollout safety net, not a place to add
// new dependencies.
//
// Why xterm matters specifically: see commit a213d5e ("fix(byom+prompt):
// kill the zutty-vs-Claude-Code rendering trap"). Older BYOM installs
// shipped without xterm; Ubuntu sometimes pulled zutty in as a
// transitive dep; the agent then had no working TUI surface and Claude
// Code's UI painted as garbled color bars.
//
// Non-fatal by design. apt failures (no internet, broken cache, not
// Debian-family) log a warning and the daemon keeps starting — the
// agent has a (degraded) prompt fallback for the missing-xterm case.
func bootstrapTools() {
	// Skip outside Linux — the daemon also builds on macOS/dev but apt
	// only exists on Debian-family Linux. Saves a noisy log line on dev.
	if runtime.GOOS != "linux" {
		return
	}

	// Only operate when the daemon actually has root. In a normal
	// production install it does (systemd ExecStart with no User=). If
	// someone is running the daemon non-root for dev, skip silently.
	if os.Geteuid() != 0 {
		return
	}

	// apt-get must exist. Non-Debian Linux (Alpine, Arch, …) is out of
	// scope for the BYOM install script anyway.
	if _, err := exec.LookPath("apt-get"); err != nil {
		return
	}

	ensureSpawnWorkerHelper()
	ensureDockerRemoved()
	ensureUnprivilegedUsernsAllowed()
	ensureChromeInstalled()
	ensureChromeLaunchWrappers()
	ensureXtermResources()

	// Worker-agent CLIs (Claude Code + Codex) are the manager's
	// fan-out fleet. Self-heal them on machines whose frozen
	// vibecraft-update.sh predates the rollout (see ensureWorkerAgents).
	// Async: a cold `npm i -g` can take a minute apiece and, unlike
	// xterm, no first-agent-call races on either binary.
	go ensureWorkerAgents()

	missing := missingTools()
	if len(missing) == 0 {
		return
	}

	log.Printf("bootstrap: installing missing tools: %v", missing)

	// 30s hard cap. apt on a healthy mirror finishes well inside this;
	// a stalled apt-update or wedged dpkg lock should not block daemon
	// startup indefinitely.
	deadline := time.Now().Add(30 * time.Second)

	// Skip apt-get update — it's slow (5-15s) and not needed if the
	// machine's apt cache has the package indexed at all. If install
	// fails with "Unable to locate", a later updater-timer tick on the
	// host (which DOES run update) will succeed.
	args := append([]string{"install", "-y", "-qq"}, missing...)
	cmd := exec.Command("apt-get", args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("bootstrap: apt-get install %v failed: %v (continuing; agent will degrade gracefully)", missing, err)
			return
		}
		log.Printf("bootstrap: installed %v", missing)
	case <-time.After(time.Until(deadline)):
		_ = cmd.Process.Kill()
		log.Printf("bootstrap: apt-get install %v timed out after 30s (continuing)", missing)
	}
}

// spawnWorkerHelper is the D3 explicit worker-spawn helper. Single
// source of truth (cloud-init.ts / install.sh ship the same script at
// provisioning; this is the existing-machine retroactive path so the
// fleet gains it on the next daemon update). It POSTs to the daemon's
// localhost spawn-worker endpoint, which sandboxes or runs legacy based
// on VIBECRAFT_SANDBOXED_WORKERS — inert until that flag flips.
const spawnWorkerHelper = `#!/bin/bash
set -euo pipefail
NAME=""; SYS=""; ALLOW=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --system) SYS="$2"; shift 2;;
    --allow) ALLOW="$2"; shift 2;;
    --) shift; break;;
    *) echo "vc-spawn-worker: unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$NAME" ] || { echo "vc-spawn-worker: --name required" >&2; exit 2; }
[ $# -gt 0 ] || { echo "vc-spawn-worker: command required after --" >&2; exit 2; }
CMD_JSON=$(printf '%s\n' "$@" | jq -R . | jq -s .)
ALLOW_JSON=$(printf '%s' "$ALLOW" | jq -R 'split(",")|map(select(length>0))')
BODY=$(jq -nc --arg n "$NAME" --arg s "$SYS" --argjson c "$CMD_JSON" --argjson a "$ALLOW_JSON" '{name:$n,system:$s,cmd:$c,allow_fqdns:$a}')
curl -fsS -X POST --unix-socket /run/vibecraft.sock http://daemon/api/daemon/spawn-worker -H 'Content-Type: application/json' -d "$BODY"
`

// xtermResources is the X resource block that pins xterm to a font + size
// the vision model can actually read.
//
// Why this exists: xterm's compile-time default `faceName` is a tiny
// bitmap face. At the daemon's 1024×768 X server resolution, the cyan
// `M` and `H` glyphs render to pixel-identical bitmaps (verified
// against the actual screenshots from session 621bf73e). Sonnet 4.6,
// Opus 4.7, and gpt-5.4-mini ALL hallucinate the same way — none can
// disambiguate them from pixels alone, because the discriminating
// information isn't in the image. DejaVu Sans Mono Bold at 12pt is
// the smallest size where `M`/`H`/`U`/`0`/`V` render distinguishably
// AND a 120×36 worker xterm still fits inside the 1024-wide screen.
// gpt-5.4-mini reads Codex device codes 7-8/8 with this font; 0/8 with
// the default. See plans/font-fix-and-screenshot-pipeline.md if it
// exists, otherwise commit message.
const xtermResources = `! Managed by VibeCraft daemon (see ensureXtermResources in
! daemon/bootstrap.go). Do not edit by hand — daemon overwrites on
! every restart.
XTerm*faceName: DejaVu Sans Mono:bold
XTerm*faceSize: 12
XTerm*foreground: #ffffff
XTerm*background: #000000
XTerm*saveLines: 4096
`

// ensureXtermResources writes /home/vibecraft/.Xresources and merges
// it into the running X server (DISPLAY=:1, owned by the vibecraft
// user). Best-effort: a failure to xrdb-merge — X not up yet, missing
// auth cookie, no xrdb installed — leaves the file in place and logs
// a warning. The next daemon restart retries the merge, and freshly-
// spawned xterms read ~/.Xresources at startup anyway (xterm's own
// fallback when no resource is in the running database). Idempotent.
func ensureXtermResources() {
	const path = "/home/vibecraft/.Xresources"
	rewrite := true
	if cur, err := os.ReadFile(path); err == nil && string(cur) == xtermResources {
		rewrite = false
	}
	if rewrite {
		if err := os.WriteFile(path, []byte(xtermResources), 0644); err != nil {
			log.Printf("bootstrap: could not write %s: %v (continuing)", path, err)
			return
		}
		if err := exec.Command("chown", "vibecraft:vibecraft", path).Run(); err != nil {
			log.Printf("bootstrap: could not chown %s: %v (continuing)", path, err)
		}
		log.Printf("bootstrap: installed %s", path)
	}

	// Merge into the live X server so already-warm sessions pick it
	// up without waiting for a worker relaunch. The daemon is root;
	// drop to vibecraft for xrdb so it uses the right $XAUTHORITY /
	// $DISPLAY (user-unit Xvfb sets those in the user's env).
	cmd := exec.Command("su", "-", "vibecraft", "-c",
		"DISPLAY=:1 xrdb -merge /home/vibecraft/.Xresources 2>&1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("bootstrap: xrdb merge failed: %v (output: %q) (continuing; file is in place for next xterm)", err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("bootstrap: xrdb merged xterm resources")
}

// ensureSpawnWorkerHelper writes /usr/local/bin/vc-spawn-worker if it
// is missing or out of date. Idempotent; root-only (checked by caller).
func ensureSpawnWorkerHelper() {
	const path = "/usr/local/bin/vc-spawn-worker"
	if cur, err := os.ReadFile(path); err == nil && string(cur) == spawnWorkerHelper {
		return
	}
	if err := os.WriteFile(path, []byte(spawnWorkerHelper), 0755); err != nil {
		log.Printf("bootstrap: could not write %s: %v (continuing)", path, err)
		return
	}
	log.Printf("bootstrap: installed %s", path)
}

// ensureDockerRemoved retroactively strips rootless Docker from existing
// machines. Docker is no longer part of the product: customer apps run
// as ordinary processes inside bwrap sandboxes (per-workload-sandboxing,
// D5 = bwrap, not Docker-as-unit). The install paths (cloud-init.ts /
// install.sh) no longer install it; this is the existing-machine
// retroactive path so the fleet sheds it on the next daemon update —
// the same model as ensureSpawnWorkerHelper. The daemon binary auto-
// updates fleet-wide via the 5-min manifest timer and restarts, so this
// runs on every machine within one update cycle of the release.
//
// SAFE by construction: if any rootless container is still running it
// skips and logs (exit 10) so a live customer app is never killed — a
// later daemon restart retries. Idempotent: a fast no-op once Docker is
// gone. Deliberately does NOT touch `loginctl enable-linger` or the
// user-slice cgroup `Delegate=` — both are load-bearing for the
// xvfb/fluxbox user units and the per-sandbox nftables egress filter
// (see lib/cloud-init.ts), despite the legacy "for Docker" comments.
func ensureDockerRemoved() {
	// Fast idempotent exit: no docker binary, no rootless setup tool,
	// and no rootless data dir means there is nothing to remove.
	_, dockerErr := exec.LookPath("docker")
	_, toolErr := exec.LookPath("dockerd-rootless-setuptool.sh")
	if dockerErr != nil && toolErr != nil {
		if _, err := os.Stat("/home/vibecraft/.local/share/docker"); err != nil {
			return
		}
	}

	const script = `set -u
RUNNING=$(su - vibecraft -c 'DOCKER_HOST=unix:///run/user/$(id -u)/docker.sock docker ps -q 2>/dev/null' 2>/dev/null | grep -c . || true)
if [ "${RUNNING:-0}" -ne 0 ]; then echo "deferred: ${RUNNING} running container(s)"; exit 10; fi
su - vibecraft -c 'dockerd-rootless-setuptool.sh uninstall -f' >/dev/null 2>&1 || true
su - vibecraft -c 'rm -rf ~/.docker ~/.local/share/docker ~/.config/docker' >/dev/null 2>&1 || true
systemctl disable --now docker.service docker.socket >/dev/null 2>&1 || true
DEBIAN_FRONTEND=noninteractive apt-get purge -y -qq docker-ce docker-ce-cli docker-ce-rootless-extras containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null 2>&1 || true
rm -f /etc/apt/sources.list.d/docker.list /etc/apt/keyrings/docker.asc >/dev/null 2>&1 || true
DEBIAN_FRONTEND=noninteractive apt-get autoremove -y -qq >/dev/null 2>&1 || true
echo removed
`

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		b, err := cmd.CombinedOutput()
		done <- res{b, err}
	}()

	select {
	case r := <-done:
		msg := strings.TrimSpace(string(r.out))
		if r.err != nil {
			if ee, ok := r.err.(*exec.ExitError); ok && ee.ExitCode() == 10 {
				log.Printf("bootstrap: Docker teardown deferred — %s (retries next daemon start)", msg)
				return
			}
			log.Printf("bootstrap: Docker teardown error: %v (%s) (continuing)", r.err, msg)
			return
		}
		log.Printf("bootstrap: rootless Docker removed (unused; apps run in bwrap sandboxes)")
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		log.Printf("bootstrap: Docker teardown timed out after 90s (continuing; retries next start)")
	}
}

// ensureUnprivilegedUsernsAllowed clears the Ubuntu 24.04 AppArmor
// restriction that blocks bwrap's uid_map setup
// (kernel.apparmor_restrict_unprivileged_userns=1 by default). Without
// this, every sandbox spawn EPERMs with "bwrap: setting up uid map:
// Permission denied" and no agent bash command can run.
//
// Placed here (daemon bootstrap) rather than cloud-init runcmd because
// the cloud-init user-data is already against the 16 KB EC2 cap; this
// also covers BYOM hosts where the operator may not have set it. Runs
// on every daemon startup, before any sandbox is spawned. Persists via
// /etc/sysctl.d/ so reboot survives.
func ensureUnprivilegedUsernsAllowed() {
	const path = "/etc/sysctl.d/99-vibecraft-userns.conf"
	const content = "kernel.apparmor_restrict_unprivileged_userns = 0\n"

	// Fast idempotent exit: already-correct file on disk.
	if cur, err := os.ReadFile(path); err == nil && string(cur) == content {
		return
	}

	// Write the sysctl.d drop-in (persists across reboots).
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		log.Printf("bootstrap: could not write %s: %v (continuing)", path, err)
		return
	}

	// Apply immediately (don't wait for the next reboot — the daemon is
	// about to start spawning sandboxes).
	cmd := exec.Command("sysctl", "-p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("bootstrap: sysctl -p %s failed: %v (%s)", path, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("bootstrap: set %s (allows bwrap unprivileged userns)", path)
}

// missingTools returns the subset of bootstrap-required binaries that
// are not currently on PATH. Keep this list minimal — every entry costs
// startup latency on a fresh install.
func missingTools() []string {
	// binary -> apt package name (usually the same)
	required := []struct {
		bin, pkg string
	}{
		{bin: "xterm", pkg: "xterm"},
		// Phase 1 per-workload sandboxing. The install paths
		// (cloud-init.ts / install.sh) install these at provisioning
		// time; this is the existing-machine safety net so the daemon
		// can sandbox once VIBECRAFT_SANDBOXED_WORKERS flips. Real-
		// kernel-proven (D5: bwrap+tini).
		{bin: "bwrap", pkg: "bubblewrap"},
		{bin: "tini", pkg: "tini"},
		{bin: "nft", pkg: "nftables"},
		{bin: "newuidmap", pkg: "uidmap"},
	}

	var missing []string
	for _, r := range required {
		if _, err := exec.LookPath(r.bin); err != nil {
			missing = append(missing, r.pkg)
		}
	}
	return missing
}

// claudeCodeBin / codexBin are the canonical install locations for
// each worker-agent CLI. Both provisioning paths (lib/cloud-init.ts,
// public/install.sh) pin the vibecraft user's npm prefix to
// ~/.npm-global, so the binaries always land here. One constant per
// agent, shared by the idempotency check and the install routine,
// keeps them from drifting apart.
const claudeCodeBin = "/home/vibecraft/.npm-global/bin/claude"
const codexBin = "/home/vibecraft/.npm-global/bin/codex"

// claudeInstallScript installs Claude Code for the vibecraft user. The
// same routine public/install.sh and lib/cloud-init.ts run at
// provisioning: a per-user npm prefix so ~/.npm-global and the
// ~/.claude session survive daemon restarts and reboots. Run as the
// vibecraft user (the caller invokes via `su - vibecraft -c`; the
// daemon is root so no password is needed). The
// `~/.npm-global/bin/npm || npm` fallback tolerates a machine whose
// npm prefix was not set on a prior partial install.
const claudeInstallScript = `set -e
mkdir -p ~/.npm-global ~/.claude
npm config set prefix ~/.npm-global
grep -q '.npm-global/bin' ~/.bashrc 2>/dev/null || echo 'export PATH=$HOME/.npm-global/bin:$PATH' >> ~/.bashrc
~/.npm-global/bin/npm i -g @anthropic-ai/claude-code 2>/dev/null || npm i -g @anthropic-ai/claude-code`

// codexInstallScript installs OpenAI's Codex CLI for the vibecraft
// user. Structure mirrors claudeInstallScript exactly — same npm
// prefix, same PATH line in ~/.bashrc, same `user-prefix-npm || system-
// npm` fallback. The two scripts could share a helper, but keeping
// them parallel + verbatim makes "what runs at install time" trivial
// to audit and to diff against the cloud-init.ts / install.sh
// counterparts.
//
// ~/.codex (lowercase) is the session/credential store Codex creates
// on `codex login`; pre-creating it here keeps the directory's owner
// vibecraft (rather than whatever uid the daemon used when codex was
// first invoked).
const codexInstallScript = `set -e
mkdir -p ~/.npm-global ~/.codex
npm config set prefix ~/.npm-global
grep -q '.npm-global/bin' ~/.bashrc 2>/dev/null || echo 'export PATH=$HOME/.npm-global/bin:$PATH' >> ~/.bashrc
~/.npm-global/bin/npm i -g @openai/codex 2>/dev/null || npm i -g @openai/codex`

const chromeWrapperBin = "/usr/bin/google-chrome-stable"
const chromePayloadBin = "/opt/google/chrome/google-chrome"
const chromeLaunchWrapper = `#!/bin/bash
exec /usr/bin/google-chrome-stable --no-first-run --no-default-browser-check --disable-default-apps --disable-infobars "$@"
`

var chromeLaunchWrapperBins = []string{
	"/usr/local/bin/chrome",
	"/usr/local/bin/google-chrome",
	"/usr/local/bin/google-chrome-stable",
}

const chromeInstallScript = `set -e
apt-get update -qq || true
apt-get install -y -qq ca-certificates wget gnupg
install -d -m 0755 /usr/share/keyrings /etc/apt/sources.list.d
wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
apt-get update -qq
apt-get install -y -qq google-chrome-stable`

func chromeInstalled() bool {
	return executableFile(chromeWrapperBin) && executableFile(chromePayloadBin)
}

// ensureChromeInstalled retroactively fixes machines whose original
// provisioning missed Chrome. The manager promise is a real browser on
// the desktop, so this is a core dependency like xterm rather than a
// convenience package. Non-fatal: if Google's repo or apt is unhealthy,
// the daemon still starts and the next daemon update retries.
func ensureChromeInstalled() {
	if chromeInstalled() {
		return
	}
	log.Printf("bootstrap: Google Chrome missing — installing")
	cmd := exec.Command("bash", "-c", chromeInstallScript)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if err := runBounded(cmd, 300*time.Second); err != nil {
		log.Printf("bootstrap: Google Chrome install failed: %v (retry next daemon update)", err)
		return
	}
	if chromeInstalled() {
		log.Printf("bootstrap: Google Chrome installed")
	} else {
		log.Printf("bootstrap: Google Chrome install ran but binary still absent (retry next daemon update)")
	}
}

func ensureChromeLaunchWrappers() {
	for _, path := range chromeLaunchWrapperBins {
		if cur, err := os.ReadFile(path); err == nil && string(cur) == chromeLaunchWrapper {
			continue
		}
		if err := os.WriteFile(path, []byte(chromeLaunchWrapper), 0755); err != nil {
			log.Printf("bootstrap: could not write %s: %v (continuing)", path, err)
			continue
		}
		log.Printf("bootstrap: installed %s", path)
	}
}

// binaryInstalled reports whether a binary is present at the given
// path (regular file, not a directory). Split out so the idempotency
// check is unit-testable without a live machine and shared by both
// worker-agent self-heal paths.
func binaryInstalled(binPath string) bool {
	fi, err := os.Stat(binPath)
	return err == nil && !fi.IsDir()
}

func executableFile(binPath string) bool {
	fi, err := os.Stat(binPath)
	return err == nil && !fi.IsDir() && fi.Mode()&0111 != 0
}

// ensureWorkerAgents installs the worker-agent CLIs (Claude Code +
// Codex) for the vibecraft user if they are missing. These are the
// manager's interactive workers; without them, worker fan-out (E2E
// F.1/F.2 — the meta-agent bedrock) cannot run.
//
// Why this lives in the daemon and not only in the install paths:
// cloud-init.ts and install.sh install both workers at provisioning,
// and the vibecraft-update.sh they write self-heals them — but that
// script is written ONCE at install time and never rolls itself
// forward. Only the daemon binary self-updates. A machine provisioned
// before either worker rollout therefore has a frozen updater with no
// install block for that worker and no path to one. This self-heal
// ships INSIDE the daemon binary, so it reaches every lagging machine
// on the next daemon update — the same propagation guarantee the
// xterm self-heal above relies on.
//
// Idempotent (no-op once both binaries exist), non-fatal, time-
// bounded. The caller has already checked euid==0 + linux + apt-get,
// and runs this in a goroutine. Installs are sequenced (not parallel)
// so a single npm/nodejs apt-install covers both, and so two
// concurrent `npm i -g` runs don't race the same prefix.
func ensureWorkerAgents() {
	claudeOK := binaryInstalled(claudeCodeBin)
	codexOK := binaryInstalled(codexBin)
	if claudeOK && codexOK {
		return
	}

	// nodejs + npm are the worker runtime, not PATH tools the agent
	// shells out to, so missingTools()/the apt block deliberately omit
	// them. Pull them here if a machine never had them — once, shared
	// by both worker installs.
	if _, err := exec.LookPath("npm"); err != nil {
		log.Printf("bootstrap: npm missing — installing nodejs npm for worker agents")
		ac := exec.Command("apt-get", "install", "-y", "-qq", "nodejs", "npm")
		ac.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		if err := runBounded(ac, 90*time.Second); err != nil {
			log.Printf("bootstrap: nodejs/npm install failed: %v (retry next daemon update)", err)
			return
		}
	}

	if !claudeOK {
		installWorkerAgent("Claude Code", claudeCodeBin, claudeInstallScript)
	}
	if !codexOK {
		installWorkerAgent("Codex", codexBin, codexInstallScript)
	}
}

// installWorkerAgent runs an install script as the vibecraft user and
// reports the post-install state. Shared by ensureWorkerAgents so the
// "log start → run bounded → chown → log result" sequence stays
// identical across agents.
func installWorkerAgent(name, bin, script string) {
	log.Printf("bootstrap: %s missing — installing for vibecraft user", name)
	cmd := exec.Command("su", "-", "vibecraft", "-c", script)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if err := runBounded(cmd, 180*time.Second); err != nil {
		log.Printf("bootstrap: %s install failed: %v (retry next daemon update)", name, err)
		return
	}
	// Appended to ~/.bashrc as the vibecraft user via `su -`, so
	// ownership is already correct; chown defensively against a prior
	// root-run install having left it root-owned.
	_ = exec.Command("chown", "vibecraft:vibecraft", "/home/vibecraft/.bashrc").Run()
	if binaryInstalled(bin) {
		log.Printf("bootstrap: %s installed", name)
	} else {
		log.Printf("bootstrap: %s install ran but binary still absent (retry next daemon update)", name)
	}
}

// runBounded runs cmd with a hard timeout, killing it on overrun.
// Same select/deadline shape bootstrapTools uses for its apt install.
func runBounded(cmd *exec.Cmd, d time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		return fmt.Errorf("timed out after %s", d)
	}
}
