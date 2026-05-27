// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
)

// Systems — the persistent, scheduled workflows the manager has
// authored on the machine (the "system, not task" unit of work:
// "every Monday at 9am ..."). Distinct from `vibecraft system`, which
// is the machine's hardware/agent info.

var systemsCmd = &cobra.Command{
	Use:   "systems",
	Short: "List the systems the manager has built on the machine",
	Long: `A "system" is a persistent, usually scheduled workflow the manager
authored — the unit of work behind "every Monday at 9am ...". This is
the same view as the dashboard's Systems panel.

  vibecraft systems list

(For machine hardware/agent info, that's 'vibecraft system' — singular.)`,
}

var systemsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List authored systems",
	RunE:  runSystemsList,
}

func init() {
	rootCmd.AddCommand(systemsCmd)
	systemsCmd.AddCommand(systemsListCmd)
}

func runSystemsList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	data, err := c.ListSystems()
	if err != nil {
		return mapDaemonError(err, "listing systems")
	}
	return output.Emit(data)
}
