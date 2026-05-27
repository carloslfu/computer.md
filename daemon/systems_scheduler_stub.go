// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"net/http"

	"github.com/carloslfu/computer.md/daemon/sandbox"
	"github.com/carloslfu/computer.md/daemon/vault"
)

func newSystemSandboxStarter(_ *vault.Store, _ http.Handler) systemSchedulerStarter {
	return func(string) error {
		return sandbox.ErrUnsupported
	}
}
