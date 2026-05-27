// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/carloslfu/computer.md/cli/cmd"
	"github.com/carloslfu/computer.md/cli/exit"
)

var version = "dev"

func main() {
	// D7: on Windows, if a previous `vibecraft update` left a sentinel
	// file, finish the swap before parsing flags so the user's command
	// runs against the new binary on this very invocation. No-op on Unix.
	completeWindowsUpdate()

	cmd.SetVersion(version)
	err := cmd.Execute()
	os.Exit(exit.FromError(err))
}
