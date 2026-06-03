// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/schema"
)

// API key management (vc_machine_* programmatic keys) is intentionally
// NOT a CLI operation on any surface the CLI can reach:
//
//   - The daemon's /keys endpoints require an RS256 JWT and explicitly
//     reject vc_machine_* API keys ("API key management requires JWT
//     authentication", 403). The CLI authenticates to the daemon with a
//     brokered vc_machine_* key, so it can never call them.
//   - The platform's key CRUD (app/api/keys) is browser-session authed
//     (WorkOS), not account-key (vc_account_*) authed, and there is no
//     account-key /api/v1/keys route for the CLI to use.
//
// So there is no path from the CLI to mint, list, or revoke a key today.
// Day-to-day the CLI does not need one: `vibecraft auth login` brokers a
// per-machine daemon key automatically. Rather than omit the verbs (an
// agent enumerating capabilities would see "unknown command" and guess),
// we surface them as discoverable commands that point at the dashboard —
// the only place key management is wired. This is the "say so precisely"
// path; if account-key key CRUD is added to the platform later, these
// stubs become the natural home for it.

var keysCmd = &cobra.Command{
	Use:   "keys",
	Short: "Manage API keys (dashboard-only today)",
	Long: `API keys (vc_machine_*) are managed from the dashboard, not the CLI.

The CLI does not need a key to operate day-to-day: 'vibecraft auth login'
brokers a per-machine daemon key for you automatically. To explicitly
create, list, or revoke a programmatic key, use the dashboard:

  Dashboard -> Settings -> CLI

There is no daemon or account-key API the CLI can use for key management
(the daemon's key endpoints are JWT-only and reject the CLI's key), so
these subcommands explain where to go rather than failing obscurely.`,
}

func keysDashboardOnly(verb string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		return schema.Newf(schema.CodeValidationError,
			"'keys %s' is not available from the CLI: API key management is dashboard-only",
			verb).
			WithHint("manage keys in the dashboard: Settings -> CLI")
	}
}

var keysCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create an API key (use the dashboard)",
	RunE:  keysDashboardOnly("create"),
}

var keysListCmd = &cobra.Command{
	Use:   "list",
	Short: "List API keys (use the dashboard)",
	RunE:  keysDashboardOnly("list"),
}

var keysRevokeCmd = &cobra.Command{
	Use:   "revoke <id>",
	Short: "Revoke an API key (use the dashboard)",
	RunE:  keysDashboardOnly("revoke"),
}

func init() {
	rootCmd.AddCommand(keysCmd)
	keysCmd.AddCommand(keysCreateCmd)
	keysCmd.AddCommand(keysListCmd)
	keysCmd.AddCommand(keysRevokeCmd)
}
