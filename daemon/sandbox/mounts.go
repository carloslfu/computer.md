// SPDX-License-Identifier: Apache-2.0

package sandbox

import "os"

func appendExistingReadOnlyMounts(mounts []Mount, paths ...string) []Mount {
	for _, path := range paths {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			mounts = append(mounts, Mount{
				HostPath:    path,
				SandboxPath: path,
				ReadOnly:    true,
			})
		}
	}
	return mounts
}
