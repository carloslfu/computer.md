// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"net/http"

	"github.com/carloslfu/computer.md/daemon/sandbox"
	"github.com/carloslfu/computer.md/daemon/vault"
)

func newSystemSandboxStarter(vaultStore *vault.Store, handler http.Handler) systemSchedulerStarter {
	return func(name string) error {
		_, err := sandbox.GetOrCreateSystemSandbox(systemsRoot, name, func(refs []string) map[string]string {
			resolved := make(map[string]string, len(refs))
			for _, ref := range refs {
				if value, ok := vaultStore.Get(ref); ok {
					resolved[ref] = value
				}
			}
			return resolved
		}, handler)
		return err
	}
}
