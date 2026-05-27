// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquireConfigLock returns a function that releases the lock. Uses
// flock(2) (POSIX advisory). Blocks up to deadline for the lock; if the
// deadline expires the caller gets an error.
func acquireConfigLock(lockPath string, deadline time.Duration) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}

	start := time.Now()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			_ = f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		if time.Since(start) > deadline {
			_ = f.Close()
			return nil, fmt.Errorf("timed out waiting for lock at %s", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
