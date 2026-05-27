// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Regression guards for the v0.27–v0.36 production brick.
//
// Two stacked defects, both invisible to the integration suite (it runs
// privileged from a full-PATH shell with tmpfs homes):
//  1. the netns wrapper was the BARE name "ip" → exec.LookPath failed
//     under systemd's PATH-less root daemon → empty output, every cmd;
//  2. the agent-shell binds the REAL /home/<user> (uid 1000, 0750) but
//     `bwrap --unshare-user` ran as ROOT (maps only 0→0) → `--chdir`
//     into that home EPERMs (`bwrap: Can't chdir … Permission denied`).
//
// Fix: resolveBin() → absolute ip/bwrap/setpriv; agent-shell and system
// sandboxes run bwrap AS the host user (rootless userns maps the home owner) via
// `ip netns exec <ns> setpriv --reuid=… --regid=… --init-groups bwrap`.
// Ephemeral workers/customer apps keep the root+tmpfs path.
//
// These tests are HERMETIC — no dependency on ip/bwrap/the vibecraft
// user existing on the runner; the agent user is overridden to a user
// that always exists.

func TestResolveBin_AbsoluteFallbackWhenNotInPATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "vc-fake-ip")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	t.Setenv("PATH", "") // LookPath cannot find it — only the abs fallback can

	got := resolveBin("vc-fake-ip", "/nonexistent/a", fake, "/nonexistent/b")
	if got != fake || !filepath.IsAbs(got) {
		t.Fatalf("resolveBin must return the existing absolute fallback %q "+
			"(the exact v0.27–v0.35 brick was this not happening); got %q", fake, got)
	}
	if lr := resolveBin("vc-fake-ip", "/nonexistent/x"); lr != "vc-fake-ip" {
		t.Fatalf("documented last resort is the bare name, got %q", lr)
	}
}

func TestResolveBin_PrefersPATHWhenResolvable(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH (unexpected)")
	}
	got := resolveBin("sh", "/should/not/be/used")
	if !filepath.IsAbs(got) || filepath.Base(got) != "sh" {
		t.Fatalf("resolveBin(sh) must be the absolute PATH resolution, got %q", got)
	}
}

// systemManifest never triggers the agent-shell user drop, so its argv
// shape is invariant regardless of the host — ideal to assert that
// ip/bwrap are never invoked by bare name.
func systemManifest() *SandboxManifest {
	return &SandboxManifest{
		ID:       "sys-a",
		Type:     TypeSystem,
		Lifetime: Persistent,
		HomeDir:  "/home/agent",
	}
}

func TestSpawnCmd_NeverBareIpOrBwrap(t *testing.T) {
	m := systemManifest()

	cmd, err := SpawnCmd(context.Background(), m, SpawnOpts{
		NetnsName: "vcns1", Cmd: []string{"/bin/bash", "-lc", "echo hi"},
	})
	if err != nil {
		t.Fatalf("SpawnCmd(netns): %v", err)
	}
	if cmd.Path != ipPath || cmd.Args[0] != ipPath {
		t.Fatalf("netns wrapper must be the resolved ipPath %q (never literal "+
			"\"ip\"), got %q / %v", ipPath, cmd.Path, cmd.Args)
	}
	want := []string{ipPath, "netns", "exec", "vcns1", bwrapPath}
	for i, w := range want {
		if i >= len(cmd.Args) || cmd.Args[i] != w {
			t.Fatalf("system netns argv prefix must be %v, got %v", want, cmd.Args)
		}
	}

	cmd2, err := SpawnCmd(context.Background(), m, SpawnOpts{
		Cmd: []string{"/bin/bash", "-lc", "echo hi"},
	})
	if err != nil {
		t.Fatalf("SpawnCmd(no-netns): %v", err)
	}
	if cmd2.Path != bwrapPath {
		t.Fatalf("no-netns program must be the resolved bwrapPath %q, got %q",
			bwrapPath, cmd2.Path)
	}
	if !strings.Contains(strings.Join(cmd2.Args, " "), "--unshare-user") {
		t.Fatalf("BwrapArgs missing --unshare-user: %v", cmd2.Args)
	}
}

// TestSpawnCmd_AgentShellDropsToHostUser deterministically verifies the
// chdir fix: the agent-shell must exec bwrap THROUGH
// `setpriv --reuid=<uid> --regid=<gid> --init-groups`, after the
// root-only `ip netns exec`. Without this, bwrap-as-root cannot enter
// the bound real home (the v0.27–v0.36 brick).
func TestSpawnCmd_HomeBoundSandboxesDropToHostUser(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skipf("user.Current: %v", err)
	}
	// Override the agent user to one guaranteed to exist on this runner.
	t.Setenv("VIBECRAFT_SANDBOX_USER", cur.Username)

	ash := AgentShellManifest("/home/vibecraft", nil, EgressPolicy{})
	cmd, err := SpawnCmd(context.Background(), ash, SpawnOpts{
		NetnsName: "vcns1", Cmd: []string{"/bin/bash", "-lc", "echo hi"},
	})
	if err != nil {
		t.Fatalf("SpawnCmd(agent-shell): %v", err)
	}
	want := []string{
		ipPath, "netns", "exec", "vcns1",
		setprivPath, "--reuid=" + cur.Uid, "--regid=" + cur.Gid, "--init-groups",
		bwrapPath,
	}
	for i, w := range want {
		if i >= len(cmd.Args) || cmd.Args[i] != w {
			t.Fatalf("agent-shell argv must drop to the host user before bwrap.\n"+
				"want prefix %v\n got %v", want, cmd.Args)
		}
	}

	sys := &SandboxManifest{
		ID:       "system-status",
		Type:     TypeSystem,
		Lifetime: Persistent,
		HomeDir:  "/home/vibecraft/systems/status-watcher",
		Mounts: []Mount{{
			HostPath:    "/home/vibecraft/systems/status-watcher",
			SandboxPath: "/home/vibecraft/systems/status-watcher",
		}},
	}
	cmd, err = SpawnCmd(context.Background(), sys, SpawnOpts{
		NetnsName: "vcns1", Cmd: []string{"/bin/bash", "-lc", "echo hi"},
	})
	if err != nil {
		t.Fatalf("SpawnCmd(system): %v", err)
	}
	for i, w := range want {
		if i >= len(cmd.Args) || cmd.Args[i] != w {
			t.Fatalf("system argv must drop to the host user before bwrap.\n"+
				"want prefix %v\n got %v", want, cmd.Args)
		}
	}
}
