// SPDX-License-Identifier: Apache-2.0

//go:build windows

package cmd

import (
	"fmt"
	"os"
	"time"
)

// acquireConfigLock on Windows uses O_EXCL semantics on a fresh lock file.
// Concurrent writers see an "already exists" error and back off.
//
// We don't depend on golang.org/x/sys/windows here because the file-based
// fallback is good enough for our two-process worst case (concurrent auth
// login attempts) and keeps the binary statically linked.
func acquireConfigLock(lockPath string, deadline time.Duration) (func(), error) {
	start := time.Now()
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			return func() {
				_ = f.Close()
				_ = os.Remove(lockPath)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("creating lock file: %w", err)
		}
		if time.Since(start) > deadline {
			return nil, fmt.Errorf("timed out waiting for lock at %s", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
