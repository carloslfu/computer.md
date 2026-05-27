// SPDX-License-Identifier: Apache-2.0

package core

import (
	"fmt"
	"syscall"
)

// DefaultDiskFreeRatio reports the fraction of free space at path (0.0–1.0).
// Works on both Linux (the production target) and Darwin (developer
// machines running tests). Returns an error if statfs fails or the
// reported block count is zero.
func DefaultDiskFreeRatio(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if stat.Blocks == 0 {
		return 0, fmt.Errorf("statfs %s reported zero blocks", path)
	}
	free := float64(stat.Bavail) * float64(stat.Bsize)
	total := float64(stat.Blocks) * float64(stat.Bsize)
	return free / total, nil
}
