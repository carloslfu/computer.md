// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Hosted tools — the web tools the manager has deployed on the machine,
// each on a registered route. This is the operator-/agent-facing view and
// the deterministic update loop: see what's running (status), apply a
// code/env change and confirm it serves (deploy), read logs, inspect env.
//
// The build itself runs in the task shell (which shares the host
// filesystem); `deploy` cycles the host service so the new build/env
// actually serves. See the daemon side in daemon/app_service.go and the
// plan plans/seamless-tool-deploy.md.
//
// `apps` is a deprecated alias for `tools` (the codebase is migrating off
// the word "app"; the backend endpoints still use /hosted-apps).

var toolsCmd = &cobra.Command{
	Use:     "tools",
	Aliases: []string{"apps"},
	Short:   "List and manage the hosted tools deployed on the machine",
	Long: `Hosted tools are the web tools the manager has deployed on the machine,
each on a registered route.

  vibecraft tools list                  # what's deployed
  vibecraft tools status <name>         # what's actually running (build, env keys, port)
  vibecraft tools logs <name>           # tail the tool's journal
  vibecraft tools deploy <name>         # apply a code/env change and verify it serves
  vibecraft tools env <name>            # environment variable keys (never values)
  vibecraft tools restart <name>        # cycle the process (does NOT rebuild)
  vibecraft tools sso <name> --on|--off
  vibecraft tools remove <name>

'apps' is a deprecated alias for 'tools' and still works.`,
}

var toolsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List deployed tools",
	RunE:  runToolsList,
}

var (
	flagToolSSOOn  bool
	flagToolSSOOff bool
	flagToolEnv    []string
	flagToolPort   int
	flagToolLines  int
)

var toolsStatusCmd = &cobra.Command{
	Use:   "status <name>",
	Short: "Show what a deployed tool is actually running",
	Long: `Show the running truth for a deployed tool: the systemd unit state and
MainPID, whether the host port is listening, the stored ExecStart and
working directory, the environment variable KEYS (never values), and the
working copy's git commit + dirty flag (so you can tell whether the running
build is current).`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsStatus,
}

var toolsLogsCmd = &cobra.Command{
	Use:   "logs <name>",
	Short: "Tail a deployed tool's logs (journalctl)",
	Long: `Print the tail of a deployed tool's journal (journalctl --user -u <name>).

  vibecraft tools logs my-tool
  vibecraft tools logs my-tool --lines 200`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsLogs,
}

var toolsDeployCmd = &cobra.Command{
	Use:   "deploy <name>",
	Short: "Apply a code/env change to a deployed tool and verify it serves",
	Long: `Redeploy an already-deployed tool: re-render its systemd unit (reusing the
stored command), re-inject environment, restart it on the host, prove the
process cycled (old_pid != new_pid), and wait for the port to listen. This
is the deterministic "now serving X" step after a code or config change.

Typical loop — the build runs in the task shell, which shares the host
filesystem, so the edited/built files are already in place:

  vibecraft task submit "cd ~/systems/my-tool && npm run build"
  vibecraft tools deploy my-tool

Update or add environment variables (a value may reference a vault secret
as $SECRET_NAME, resolved on the machine so plaintext never touches your
shell history):

  vibecraft tools deploy my-tool --env LOG_LEVEL=debug --env STRIPE_KEY='$STRIPE_LIVE_KEY'

deploy operates on EXISTING tools only. Creating a brand-new tool (choosing
the command it runs) is the manager's job — ask it in chat.`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsDeploy,
}

var toolsEnvCmd = &cobra.Command{
	Use:   "env <name>",
	Short: "List a deployed tool's environment variable keys (never values)",
	Long: `List the environment variable KEYS a deployed tool runs with. Values are
never returned — the daemon does not expose them over the network. Set or
change a value with 'vibecraft tools deploy <name> --env KEY=VALUE' (use
$SECRET_NAME to pull from the vault).`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsEnv,
}

var toolsSSOCmd = &cobra.Command{
	Use:   "sso <name>",
	Short: "Turn SSO protection on or off for a deployed tool",
	Long: `Toggle whether a deployed tool sits behind VibeCraft SSO.

  vibecraft tools sso my-tool --on
  vibecraft tools sso my-tool --off`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsSSO,
}

var toolsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Unregister a deployed tool's route",
	Args:  cobra.ExactArgs(1),
	RunE:  runToolsRemove,
}

var toolsRestartCmd = &cobra.Command{
	Use:   "restart <name>",
	Short: "Restart a deployed tool's process (does NOT rebuild or re-read source)",
	Long: `Cycle a deployed tool's host-side service: stop the current process and
start it again from the SAME unit and the SAME build artifact. It verifies
the process actually cycled (old_pid != new_pid) and that the port is
listening afterward.

restart does NOT rebuild from source, pick up changed source for compiled/
bundled tools, or change environment. After editing/rebuilding code, or to
change env, use 'vibecraft tools deploy <name>' instead — that re-renders
the unit, re-injects env, and verifies the new build serves.`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsRestart,
}

func init() {
	toolsSSOCmd.Flags().BoolVar(&flagToolSSOOn, "on", false, "Enable SSO protection")
	toolsSSOCmd.Flags().BoolVar(&flagToolSSOOff, "off", false, "Disable SSO protection")
	toolsDeployCmd.Flags().StringArrayVar(&flagToolEnv, "env", nil,
		"Set an environment variable KEY=VALUE (repeatable; VALUE may use $SECRET_NAME from the vault)")
	toolsDeployCmd.Flags().IntVar(&flagToolPort, "port", 0,
		"Override the port to verify after deploy (default: the tool's registered port)")
	toolsLogsCmd.Flags().IntVar(&flagToolLines, "lines", 100, "Number of log lines to tail (max 1000)")

	rootCmd.AddCommand(toolsCmd)
	toolsCmd.AddCommand(toolsListCmd)
	toolsCmd.AddCommand(toolsStatusCmd)
	toolsCmd.AddCommand(toolsLogsCmd)
	toolsCmd.AddCommand(toolsDeployCmd)
	toolsCmd.AddCommand(toolsEnvCmd)
	toolsCmd.AddCommand(toolsSSOCmd)
	toolsCmd.AddCommand(toolsRemoveCmd)
	toolsCmd.AddCommand(toolsRestartCmd)
}

func runToolsList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	apps, err := c.ListHostedApps()
	if err != nil {
		return mapDaemonError(err, "listing tools")
	}
	out := schema.AppsListData{Apps: make([]schema.HostedAppData, 0, len(apps))}
	for _, a := range apps {
		out.Apps = append(out.Apps, schema.HostedAppData{
			Name:       a.Name,
			Port:       a.Port,
			URL:        a.URL,
			SSOEnabled: a.SSOEnabled,
			CreatedAt:  a.CreatedAt,
		})
	}
	return output.Emit(out)
}

func runToolsStatus(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	st, err := c.GetHostedAppStatus(args[0])
	if err != nil {
		return mapDaemonError(err, "reading tool status")
	}
	return output.Emit(schema.ToolStatusData{
		Name:             st.Name,
		Unit:             st.Unit,
		Port:             st.Port,
		URL:              st.URL,
		SSOEnabled:       st.SSOEnabled,
		CreatedAt:        st.CreatedAt,
		UnitPresent:      st.UnitPresent,
		ExecStart:        st.ExecStart,
		WorkingDirectory: st.WorkingDirectory,
		Description:      st.Description,
		EnvKeys:          st.EnvKeys,
		ActiveState:      st.ActiveState,
		SubState:         st.SubState,
		MainPID:          st.MainPID,
		Listening:        st.Listening,
		Since:            st.Since,
		GitCommit:        st.GitCommit,
		GitDirty:         st.GitDirty,
		Detail:           st.Detail,
	})
}

func runToolsLogs(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	lg, err := c.GetHostedAppLogs(args[0], flagToolLines)
	if err != nil {
		return mapDaemonError(err, "reading tool logs")
	}
	return output.Emit(schema.ToolLogsData{
		Name:   lg.Name,
		Unit:   lg.Unit,
		Lines:  lg.Lines,
		Detail: lg.Detail,
	})
}

func runToolsDeploy(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var port *int
	if cmd.Flags().Changed("port") {
		p := flagToolPort
		port = &p
	}
	res, err := c.DeployHostedApp(args[0], flagToolEnv, port)
	if err != nil {
		return mapDaemonError(err, "deploying tool")
	}
	return output.Emit(schema.ToolDeployData{
		OK:             res.OK,
		Name:           res.Name,
		Unit:           res.Unit,
		Status:         res.Status,
		OldPID:         res.OldPID,
		NewPID:         res.NewPID,
		Port:           res.Port,
		ListeningAfter: res.ListeningAfter,
		UnitPath:       res.UnitPath,
		Detail:         res.Detail,
	})
}

func runToolsEnv(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ev, err := c.GetHostedAppEnv(args[0])
	if err != nil {
		return mapDaemonError(err, "reading tool env")
	}
	out := schema.ToolEnvData{Name: ev.Name, Unit: ev.Unit, Detail: ev.Detail}
	out.Keys = make([]schema.ToolEnvKeyData, 0, len(ev.Keys))
	for _, k := range ev.Keys {
		out.Keys = append(out.Keys, schema.ToolEnvKeyData{Key: k.Key, Reserved: k.Reserved})
	}
	return output.Emit(out)
}

func runToolsSSO(cmd *cobra.Command, args []string) error {
	if flagToolSSOOn == flagToolSSOOff {
		return schema.Newf(schema.CodeValidationError,
			"pass exactly one of --on or --off")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.SetHostedAppSSO(args[0], flagToolSSOOn); err != nil {
		return mapDaemonError(err, "setting tool SSO")
	}
	status := "sso disabled"
	if flagToolSSOOn {
		status = "sso enabled"
	}
	return output.Emit(schema.AppActionData{Name: args[0], Status: status})
}

func runToolsRemove(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.RemoveHostedApp(args[0]); err != nil {
		return mapDaemonError(err, "removing tool")
	}
	return output.Emit(schema.AppActionData{Name: args[0], Status: "removed"})
}

func runToolsRestart(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	result, err := c.RestartHostedApp(args[0])
	if err != nil {
		return mapDaemonError(err, "restarting tool")
	}
	return output.Emit(schema.AppRestartData{
		OK:             result.OK,
		Name:           result.Name,
		Unit:           result.Unit,
		Status:         result.Status,
		OldPID:         result.OldPID,
		NewPID:         result.NewPID,
		Port:           result.Port,
		ListeningAfter: result.ListeningAfter,
		Detail:         result.Detail,
	})
}
