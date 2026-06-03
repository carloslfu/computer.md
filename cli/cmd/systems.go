// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
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

Systems are AUTHORED by the manager, not by the CLI: the manager writes
the files (run.sh, manifest.json, crontab) under ~/systems/<name>/ and
the per-machine scheduler picks them up. To create, change the schedule
of, run on demand, pause, or delete a system, direct the manager in
natural language ("vibecraft task 'pause the morning-brief system'").
The CLI exposes the read + lifecycle verbs that the daemon backs:

  vibecraft systems list            # what's authored (and what's running)

(For machine hardware/agent info, that's 'vibecraft system' — singular.)`,
}

var systemsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List authored systems",
	RunE:  runSystemsList,
}

// The lifecycle verbs below are deliberately NOT wired to daemon
// endpoints: the daemon exposes no system create/enable/disable/delete/run
// API on the remote (vc_machine_*) surface. Authoring a system is the
// manager's job (it writes ~/systems/<name>/ files directly; the
// scheduler is plain crontab + supercronic — there is no daemon cron
// primitive to drive from here), and the only mutating system endpoint
// the daemon has (uninstall) is localhost-only, reached by the manager
// over the loopback socket, not by a remote CLI key.
//
// Rather than omit the verbs (an agent enumerating `--help` would just
// see "unknown command" and guess), we surface them as discoverable
// commands that return a precise, machine-readable validation_error
// pointing at the manager. This is the "say so precisely" path.

func systemsManagerOnly(verb, example string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		return schema.Newf(schema.CodeValidationError,
			"'systems %s' is not a CLI operation: systems are authored and managed by the manager, not over the daemon API",
			verb).
			WithHint("direct the manager in natural language, e.g. vibecraft task \"" + example + "\"")
	}
}

var systemsCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Author a new system (manager-only)",
	Long: `Authoring a system is the manager's job — it writes
~/systems/<name>/ (run.sh, manifest.json, crontab) directly and the
scheduler picks it up. There is no daemon API to create one from the
CLI. Ask the manager instead.`,
	RunE: systemsManagerOnly("create", "build a system that emails me a sales summary every Monday at 9am"),
}

var systemsEnableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Resume a paused system (manager-only)",
	RunE:  systemsManagerOnly("enable", "resume the morning-brief system"),
}

var systemsDisableCmd = &cobra.Command{
	Use:   "disable <name>",
	Short: "Pause a system (manager-only)",
	RunE:  systemsManagerOnly("disable", "pause the morning-brief system"),
}

var systemsRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Run a system once, now (manager-only)",
	RunE:  systemsManagerOnly("run", "run the morning-brief system now"),
}

var systemsDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Uninstall a system (manager-only)",
	Long: `Removing a system (scrubbing its crontab line and deleting
~/systems/<name>/) is done by the manager over the machine's loopback
socket; the daemon does not expose this on the remote CLI surface. Ask
the manager to uninstall it.`,
	RunE: systemsManagerOnly("delete", "uninstall the morning-brief system"),
}

func init() {
	rootCmd.AddCommand(systemsCmd)
	systemsCmd.AddCommand(systemsListCmd)
	systemsCmd.AddCommand(systemsCreateCmd)
	systemsCmd.AddCommand(systemsEnableCmd)
	systemsCmd.AddCommand(systemsDisableCmd)
	systemsCmd.AddCommand(systemsRunCmd)
	systemsCmd.AddCommand(systemsDeleteCmd)
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
