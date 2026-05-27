// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Hosted apps — the web apps the manager has deployed on the machine,
// each on a registered route. The agent side of the route table is
// localhost-only; this is the operator-facing view + SSO toggle.

var appsCmd = &cobra.Command{
	Use:   "apps",
	Short: "List and manage deployed apps",
	Long:  `List the web apps deployed on the machine, toggle their SSO protection, or remove a route.`,
}

var appsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List deployed apps",
	RunE:  runAppsList,
}

var (
	flagAppSSOOn  bool
	flagAppSSOOff bool
)

var appsSSOCmd = &cobra.Command{
	Use:   "sso <name>",
	Short: "Turn SSO protection on or off for a deployed app",
	Long: `Toggle whether a deployed app sits behind VibeCraft SSO.

  vibecraft apps sso my-app --on
  vibecraft apps sso my-app --off`,
	Args: cobra.ExactArgs(1),
	RunE: runAppsSSO,
}

var appsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Unregister a deployed app's route",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppsRemove,
}

var appsRestartCmd = &cobra.Command{
	Use:   "restart <name>",
	Short: "Restart a deployed app's service",
	Long: `Restart a deployed app's host-side service and verify that the process
actually cycled. The result includes old_pid, new_pid, and whether the route
port is listening after restart.`,
	Args: cobra.ExactArgs(1),
	RunE: runAppsRestart,
}

func init() {
	appsSSOCmd.Flags().BoolVar(&flagAppSSOOn, "on", false, "Enable SSO protection")
	appsSSOCmd.Flags().BoolVar(&flagAppSSOOff, "off", false, "Disable SSO protection")

	rootCmd.AddCommand(appsCmd)
	appsCmd.AddCommand(appsListCmd)
	appsCmd.AddCommand(appsSSOCmd)
	appsCmd.AddCommand(appsRemoveCmd)
	appsCmd.AddCommand(appsRestartCmd)
}

func runAppsList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	apps, err := c.ListHostedApps()
	if err != nil {
		return mapDaemonError(err, "listing apps")
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

func runAppsSSO(cmd *cobra.Command, args []string) error {
	if flagAppSSOOn == flagAppSSOOff {
		return schema.Newf(schema.CodeValidationError,
			"pass exactly one of --on or --off")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.SetHostedAppSSO(args[0], flagAppSSOOn); err != nil {
		return mapDaemonError(err, "setting app SSO")
	}
	status := "sso disabled"
	if flagAppSSOOn {
		status = "sso enabled"
	}
	return output.Emit(schema.AppActionData{Name: args[0], Status: status})
}

func runAppsRemove(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.RemoveHostedApp(args[0]); err != nil {
		return mapDaemonError(err, "removing app")
	}
	return output.Emit(schema.AppActionData{Name: args[0], Status: "removed"})
}

func runAppsRestart(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	result, err := c.RestartHostedApp(args[0])
	if err != nil {
		return mapDaemonError(err, "restarting app")
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
