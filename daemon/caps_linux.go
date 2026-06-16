// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Phase 6 residual capability drop. After per-workload sandboxing the
// daemon is the only privileged process on the host, so it should hold
// exactly the caps it still needs and nothing else. It runs as root
// (full caps) at startup; once the long-lived listeners are about to
// start we drop everything except the set below.
//
// CRITICAL — every cap here is load-bearing for a PER-REQUEST privileged
// action; do NOT trim this set without a real-kernel E2E (the daemon
// boot path + a post-drop `bwrap --unshare-user` spawn). The original
// {DAC_OVERRIDE,NET_ADMIN,SYS_ADMIN}-only set shipped in v0.34.0 and
// BRICKED the agent bash tool in production: `Sandbox.Run` re-spawns a
// fresh `bwrap --unshare-user …` for EVERY command (spawn_linux.go), and
// a root process writing a child user-ns uid_map/gid_map that maps ids
// other than its own euid needs CAP_SETUID/CAP_SETGID — which the narrow
// set dropped. Result: the startup agent-shell spawn (pre-drop, full
// caps) worked, but every subsequent per-command re-spawn EPERM'd on the
// id-map write and exited non-zero with no stdout, so the agent saw
// `stdout: (empty) exit code: 1` for literally every command. The
// real-kernel integration tests missed it because they spawn bwrap as
// full-cap root and never run dropResidualCaps() first. See
// daemon/SECURITY.md finding #7.
//
//   - CAP_CHOWN       (0)  — per-request sandbox dir / crontab chown to
//     the agent uid (worker/system spawns chown at request time)
//   - CAP_DAC_OVERRIDE (1) — read /etc/vibecraft/* (0600 root) and bind
//     the per-sandbox sockets regardless of path perms
//   - CAP_FOWNER      (3)  — metadata ops on per-request sandbox-prep
//     dirs not owned by euid
//   - CAP_SETGID      (6)  — `bwrap --unshare-user` gid_map + setgroups
//   - CAP_SETUID      (7)  — `bwrap --unshare-user` uid_map
//   - CAP_NET_ADMIN  (12)  — per-sandbox veth + netns + nftables egress
//   - CAP_SYS_CHROOT (18)  — bwrap chroot/pivot_root
//   - CAP_SYS_ADMIN  (21)  — mount namespaces / bwrap spawn, per request
//
// Retaining SETUID/SETGID/SYS_CHROOT/CHOWN/FOWNER alongside the already-
// retained SYS_ADMIN does not materially widen the daemon's power —
// CAP_SYS_ADMIN is already the "do anything" cap. The hardening value of
// the residual drop is shedding the ~50 OTHER caps (SYS_MODULE,
// SYS_RAWIO, SYS_BOOT, SYS_PTRACE, NET_RAW, BPF, PERFMON, MKNOD,
// AUDIT_*, MAC_*, …), which this still does. The set is retained for the
// whole process lifetime because sandbox spawns happen per request, not
// just at startup. This is the unconditional residual-drop fallback (it
// does not depend on the systemd service-user transition).
//
// Failure policy: best-effort. A kernel that rejects the capset/prctl
// dance leaves the daemon exactly as privileged as it has always been —
// no worse than the pre-hardening status quo — so we log+audit and
// continue rather than refuse to boot and brick a machine over a
// hardening nicety. Recorded in daemon/SECURITY.md.

const (
	capChown       = 0
	capDACOverride = 1
	capFowner      = 3
	capSetGID      = 6
	capSetUID      = 7
	capNetAdmin    = 12
	capSysChroot   = 18
	capSysAdmin    = 21

	linuxCapabilityVersion3 = 0x20080522
	prCapBSetDrop           = 24
	prCapAmbient            = 47
	prCapAmbientClearAll    = 4
)

type capUserHeader struct {
	version uint32
	pid     int32
}

type capUserData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// keepWord0 is the bitmask (caps 0..31) of the retained caps. All are
// < 32 so word 1 (caps 32..63) is always zero.
const keepWord0 = (1 << capChown) | (1 << capDACOverride) | (1 << capFowner) |
	(1 << capSetGID) | (1 << capSetUID) | (1 << capNetAdmin) |
	(1 << capSysChroot) | (1 << capSysAdmin)

func capRetained() []string {
	return []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_SETGID",
		"CAP_SETUID", "CAP_NET_ADMIN", "CAP_SYS_CHROOT", "CAP_SYS_ADMIN",
	}
}

// dropResidualCaps drops the bounding set and shrinks the process's
// permitted/effective/inheritable sets to exactly the retained caps
// (the eight in capRetained()). Must run while the process still holds
// CAP_SETPCAP (true for a root-launched daemon): the bounding-set drops
// need it, and the final capset removes it.
func dropResidualCaps() error {
	keep := map[int]bool{
		capChown: true, capDACOverride: true, capFowner: true,
		capSetGID: true, capSetUID: true, capNetAdmin: true,
		capSysChroot: true, capSysAdmin: true,
	}

	// 1. Bounding set: drop every cap we are not keeping. cap numbers the
	// running kernel doesn't know return EINVAL — benign, skip. 0..63
	// covers every cap any kernel defines.
	var lastErr error
	for c := 0; c <= 63; c++ {
		if keep[c] {
			continue
		}
		_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prCapBSetDrop, uintptr(c), 0, 0, 0, 0)
		if errno != 0 && errno != syscall.EINVAL {
			lastErr = fmt.Errorf("PR_CAPBSET_DROP cap %d: %w", c, errno)
		}
	}

	// 2. Clear the ambient set (best-effort; not all kernels have it).
	syscall.Syscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0)

	// 3. capset: permitted/effective = the retained caps (keepWord0),
	// inheritable = none (children get nothing by inheritance). Done LAST —
	// it removes CAP_SETPCAP from permitted, after which step 1 would no
	// longer be allowed.
	hdr := capUserHeader{version: linuxCapabilityVersion3, pid: 0} // 0 = self
	data := [2]capUserData{
		{effective: keepWord0, permitted: keepWord0, inheritable: 0},
		{effective: 0, permitted: 0, inheritable: 0},
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return fmt.Errorf("capset: %w", errno)
	}
	return lastErr
}
