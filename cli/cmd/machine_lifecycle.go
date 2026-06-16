// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Machine lifecycle verbs — provision, connect, terminate. These hit
// the platform (not a daemon) with the account key, so the CLI can
// manage machines end to end. No payment ever flows through the CLI:
// `create` reserves a managed-resource commitment against funded usage
// credit and account metered spend terms before provisioning.
//
// Only `terminate` is gated — by --confirm — because it is the one
// irreversible action (it destroys the customer's computer).

var flagMachineConfirm bool

var machineCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Provision a new managed (cloud) machine",
	Long: `Provision a managed VibeCraft machine — VibeCraft runs it on AWS.

Reserves the machine's monthly resource cost against funded usage credit
and account metered spend terms before provisioning. If the request does
not fit, this returns 'limit_reached' so you can add credit, raise the
spend limit, or choose a smaller resource from the dashboard.

  vibecraft machine create
  vibecraft machine create "Ops box"`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMachineCreate,
}

var machineConnectCmd = &cobra.Command{
	Use:   "connect <name>",
	Short: "Register a BYOM machine (your own hardware)",
	Long: `Start a bring-your-own-machine registration. Returns a one-hour
registration token and the install command to run on your Linux box.
Free — no cloud machine is created.

  vibecraft machine connect "My server"`,
	Args: cobra.ExactArgs(1),
	RunE: runMachineConnect,
}

var machineTerminateCmd = &cobra.Command{
	Use:   "terminate <machine-id>",
	Short: "Permanently terminate a machine (requires --confirm)",
	Long: `Terminate a machine. Managed machines are snapshotted then torn down;
BYOM machines are unregistered. This is irreversible from the customer's
side — the computer, its files, systems, and vault are gone.

Because it cannot be undone, this verb refuses to run without --confirm:

  vibecraft machine terminate vc-abc            # → confirmation_required
  vibecraft machine terminate vc-abc --confirm  # actually terminates`,
	Args: cobra.ExactArgs(1),
	RunE: runMachineTerminate,
}

func init() {
	machineTerminateCmd.Flags().BoolVar(&flagMachineConfirm, "confirm", false,
		"Confirm this irreversible action")
	machineCmd.AddCommand(machineCreateCmd)
	machineCmd.AddCommand(machineConnectCmd)
	machineCmd.AddCommand(machineTerminateCmd)
}

func runMachineCreate(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	body := map[string]string{}
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		body["name"] = strings.TrimSpace(args[0])
	}
	payload, _ := json.Marshal(body)
	data, err := platformMutate(key, "POST", "/api/v1/machines/provision", payload)
	if err != nil {
		return err
	}
	return output.Emit(data)
}

func runMachineConnect(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	name := strings.TrimSpace(args[0])
	if name == "" {
		return schema.Newf(schema.CodeValidationError, "machine name cannot be empty")
	}
	payload, _ := json.Marshal(map[string]string{"name": name})
	data, err := platformMutate(key, "POST", "/api/v1/machines/connect", payload)
	if err != nil {
		return err
	}
	return output.Emit(data)
}

func runMachineTerminate(cmd *cobra.Command, args []string) error {
	id := args[0]
	if !flagMachineConfirm {
		return schema.Newf(schema.CodeConfirmationRequired,
			"terminating %s permanently destroys the machine and all its data", id).
			WithHint("re-run with --confirm if you are sure: vibecraft machine terminate " + id + " --confirm")
	}
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	data, err := platformMutate(key, "DELETE", "/api/v1/machines/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return output.Emit(data)
}
