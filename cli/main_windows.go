// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// completeWindowsUpdate is called from main() before flag parsing on
// Windows. If a "<exe>.pending-update" sentinel exists alongside the
// running binary AND a "<exe>.new" was staged, swap them and re-exec
// so the user's command runs against the updated binary in one step.
// D7.
//
// Failure modes are intentionally non-fatal — if the swap can't
// complete we leave the sentinel in place and proceed with the old
// binary (the user can retry `vibecraft update` later). This avoids a
// catastrophic boot loop if the new binary is somehow corrupt.
func completeWindowsUpdate() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	sentinel := exe + ".pending-update"
	newBin := exe + ".new"

	if _, err := os.Stat(sentinel); err != nil {
		return
	}
	if _, err := os.Stat(newBin); err != nil {
		// Sentinel without the staged binary — clean up and move on.
		_ = os.Remove(sentinel)
		return
	}
	// On Windows, renaming over a running .exe fails. But we're hitting
	// this path from a FRESH invocation (Windows hasn't started running
	// the new binary yet — we're still in the old one), so the old exe
	// IS held open. The trick: rename old → old.bak, then new → old,
	// then re-exec new.
	bak := exe + ".bak"
	_ = os.Remove(bak)
	if err := os.Rename(exe, bak); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot rename %s to %s: %v\n", exe, bak, err)
		return
	}
	if err := os.Rename(newBin, exe); err != nil {
		// Rollback.
		_ = os.Rename(bak, exe)
		return
	}
	_ = os.Remove(sentinel)
	_ = os.Remove(bak) // best-effort cleanup; Windows may hold it briefly

	// Re-exec the new binary with our original arguments. syscall.Exec
	// doesn't exist on Windows, so spawn + exit.
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Inherit env.
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
		}
		os.Exit(1)
	}
	os.Exit(0)
}
