// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Team management — account-level, via the platform with the account
// key. All reversible (a removed member can be re-added), so no
// --confirm gate.

var teamCmd = &cobra.Command{
	Use:   "team",
	Short: "Manage who is on your VibeCraft account team",
	Long: `List, add, and remove team members. Team members can be granted
control of specific machines with 'vibecraft access grant'.`,
}

var teamListCmd = &cobra.Command{
	Use:   "list",
	Short: "List team members",
	RunE:  runTeamList,
}

var teamAddCmd = &cobra.Command{
	Use:   "add <email>",
	Short: "Add a team member by email",
	Long: `Add a team member. If they don't have a VibeCraft account yet, a
pending invite is created and an email is sent. New members start with
no machine access — grant it with 'vibecraft access grant'.`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamAdd,
}

var teamRemoveCmd = &cobra.Command{
	Use:   "remove <member-id>",
	Short: "Remove a team member",
	Long: `Remove a team member by their member id (from 'vibecraft team list').
Their machine access, API keys, and live sessions are all revoked.`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamRemove,
}

func init() {
	rootCmd.AddCommand(teamCmd)
	teamCmd.AddCommand(teamListCmd)
	teamCmd.AddCommand(teamAddCmd)
	teamCmd.AddCommand(teamRemoveCmd)
}

func runTeamList(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	data, err := platformGetJSON(key, "/api/v1/team")
	if err != nil {
		return err
	}
	return output.Emit(data)
}

func runTeamAdd(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"email": args[0]})
	data, err := platformMutate(key, "POST", "/api/v1/team", payload)
	if err != nil {
		return err
	}
	return output.Emit(data)
}

func runTeamRemove(cmd *cobra.Command, args []string) error {
	key, err := loadAccountKey()
	if err != nil {
		return err
	}
	if args[0] == "" {
		return schema.Newf(schema.CodeValidationError, "member id cannot be empty")
	}
	data, err := platformMutate(key, "DELETE", "/api/v1/team/"+url.PathEscape(args[0]), nil)
	if err != nil {
		return err
	}
	return output.Emit(data)
}
