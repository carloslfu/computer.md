// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

// completeWindowsUpdate is a no-op on Unix — the POSIX rename in
// `vibecraft update` is atomic and the running process inherits the
// old inode, so no second-launch dance is needed.
func completeWindowsUpdate() {}
