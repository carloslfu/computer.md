// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import "testing"

// Regression guard for the v0.34.0 brick: the residual-cap keep-set was
// {DAC_OVERRIDE,NET_ADMIN,SYS_ADMIN} only, which made every per-command
// `bwrap --unshare-user` re-spawn EPERM on its uid_map/gid_map write
// (no CAP_SETUID/CAP_SETGID), so the agent bash tool returned
// `stdout: (empty) exit code: 1` for literally every command in
// production. These caps are load-bearing for a PER-REQUEST privileged
// action; this test fails the moment someone trims one back out.
//
// See daemon/SECURITY.md finding #7 and caps_linux.go.
func TestResidualKeepSet_RetainsBwrapRequiredCaps(t *testing.T) {
	required := map[string]int{
		"CAP_CHOWN":        capChown,       // per-request sandbox/crontab chown
		"CAP_DAC_OVERRIDE": capDACOverride, // read /etc/vibecraft/* 0600
		"CAP_FOWNER":       capFowner,      // sandbox-prep metadata ops
		"CAP_SETGID":       capSetGID,      // bwrap --unshare-user gid_map
		"CAP_SETUID":       capSetUID,      // bwrap --unshare-user uid_map
		"CAP_NET_ADMIN":    capNetAdmin,    // per-sandbox veth/netns/nft
		"CAP_SYS_CHROOT":   capSysChroot,   // bwrap chroot/pivot_root
		"CAP_SYS_ADMIN":    capSysAdmin,    // mount ns / bwrap spawn
	}
	for name, bit := range required {
		if keepWord0&(1<<uint(bit)) == 0 {
			t.Fatalf("keepWord0 is missing %s (cap %d) — this is the exact "+
				"v0.34.0 regression that bricked the agent bash tool; "+
				"do not trim the residual keep-set without a real-kernel "+
				"post-drop bwrap --unshare-user E2E", name, bit)
		}
	}

	// CAP_SETUID + CAP_SETGID are the two that specifically caused the
	// brick; assert them explicitly and loudly.
	if keepWord0&(1<<uint(capSetUID)) == 0 || keepWord0&(1<<uint(capSetGID)) == 0 {
		t.Fatal("CAP_SETUID/CAP_SETGID dropped: bwrap --unshare-user " +
			"cannot write its id maps post-drop; every agent bash command " +
			"will return exit 1 with empty stdout (the v0.34.0 prod brick)")
	}

	// The hardening intent must still hold: the genuinely dangerous caps
	// stay dropped. (SYS_ADMIN is retained by necessity — bwrap/mounts —
	// so the residual drop's value is shedding these.)
	danger := map[string]int{
		"CAP_SYS_MODULE": 16,
		"CAP_SYS_RAWIO":  17,
		"CAP_SYS_PTRACE": 19,
		"CAP_SYS_BOOT":   22,
		"CAP_NET_RAW":    13,
		"CAP_MKNOD":      27,
	}
	for name, bit := range danger {
		if keepWord0&(1<<uint(bit)) != 0 {
			t.Fatalf("keepWord0 unexpectedly retains dangerous %s (cap %d) — "+
				"the residual drop must still shed it", name, bit)
		}
	}
}
