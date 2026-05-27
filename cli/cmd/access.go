// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Per-machine control grants. A team member with a grant can drive that
// machine. Account-level, via the platform with the account key. All
// reversible, so no --confirm gate. Only the machine owner can manage
// grants for it.

var (
	flagAccessMachine string
	flagAccessEmail   string
)

var accessCmd = &cobra.Command{
	Use:   "access",
	Short: "Grant or revoke a team member's control of a machine",
	Long: `Manage per-machine control grants. A team member needs a grant to
operate a machine they don't own.

  vibecraft access list   --machine vc-abc
  vibecraft access grant  --machine vc-abc --email teammate@co.com
  vibecraft access revoke --machine vc-abc --email teammate@co.com`,
}

var accessListCmd = &cobra.Command{
	Use:   "list",
	Short: "List who has control of a machine",
	RunE:  runAccessList,
}

var accessGrantCmd = &cobra.Command{
	Use:   "grant",
	Short: "Grant a team member control of a machine",
	RunE:  runAccessGrant,
}

var accessRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke a team member's control of a machine",
	RunE:  runAccessRevoke,
}

func init() {
	for _, c := range []*cobra.Command{accessListCmd, accessGrantCmd, accessRevokeCmd} {
		c.Flags().StringVar(&flagAccessMachine, "machine", "", "Machine id (required)")
	}
	accessGrantCmd.Flags().StringVar(&flagAccessEmail, "email", "", "Team member's email (required)")
	accessRevokeCmd.Flags().StringVar(&flagAccessEmail, "email", "", "Team member's email (required)")

	rootCmd.AddCommand(accessCmd)
	accessCmd.AddCommand(accessListCmd)
	accessCmd.AddCommand(accessGrantCmd)
	accessCmd.AddCommand(accessRevokeCmd)
}

// accessMachineID resolves the target machine. --machine on the access
// subcommand takes priority; it falls back to the global --machine.
func accessMachineID() (string, error) {
	id := flagAccessMachine
	if id == "" {
		id = flagMachineID
	}
	if id == "" {
		return "", schema.Newf(schema.CodeValidationError,
			"--machine <id> is required")
	}
	if isFanoutSelector(id) {
		return "", schema.Newf(schema.CodeValidationError,
			"access verbs target one machine; --machine cannot be a list/glob/all")
	}
	return id, nil
}

func runAccessList(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	machineID, err := accessMachineID()
	if err != nil {
		return err
	}
	data, err := platformGetJSON(key, "/api/v1/machines/"+url.PathEscape(machineID)+"/access")
	if err != nil {
		return err
	}
	return output.Emit(data)
}

func runAccessGrant(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	machineID, err := accessMachineID()
	if err != nil {
		return err
	}
	if flagAccessEmail == "" {
		return schema.Newf(schema.CodeValidationError, "--email is required")
	}
	payload, _ := json.Marshal(map[string]string{"email": flagAccessEmail})
	data, err := platformMutate(key, "POST",
		"/api/v1/machines/"+url.PathEscape(machineID)+"/access", payload)
	if err != nil {
		return err
	}
	return output.Emit(data)
}

func runAccessRevoke(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	machineID, err := accessMachineID()
	if err != nil {
		return err
	}
	if flagAccessEmail == "" {
		return schema.Newf(schema.CodeValidationError, "--email is required")
	}
	q := url.Values{}
	q.Set("email", flagAccessEmail)
	data, err := platformMutate(key, "DELETE",
		"/api/v1/machines/"+url.PathEscape(machineID)+"/access?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return output.Emit(data)
}
