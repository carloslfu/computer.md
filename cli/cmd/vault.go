// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagVaultValue    string
	flagVaultFromFile string
	flagVaultLabel    string
)

var vaultCmd = &cobra.Command{
	Use:   "vault",
	Short: "Manage encrypted secrets on the machine",
	Long: `Read, write, list, and delete secrets stored in the machine's vault.

Secrets are encrypted at rest with a machine-local key and made available to
tasks the manager runs (e.g. an API key the agent needs to call a vendor).

The CLI cannot READ values back — the daemon never returns plaintext over the
network. Listing returns metadata only. If you need to inspect a secret, do
so from the dashboard (which is on the same machine and can decrypt locally).`,
}

var vaultListCmd = &cobra.Command{
	Use:   "list",
	Short: "List stored secrets (metadata only — never values)",
	RunE:  runVaultList,
}

var vaultSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Store a secret",
	Long: `Write or overwrite a secret. The value comes from one of:

  --value <v>          literal (warning: ends up in shell history)
  --value -            read from stdin (keeps it out of history)
  --from-file <path>   read from a file

  --label <text>       optional human-readable label`,
	Args: cobra.ExactArgs(1),
	RunE: runVaultSet,
}

var vaultDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Remove a secret",
	Args:  cobra.ExactArgs(1),
	RunE:  runVaultDelete,
}

func init() {
	rootCmd.AddCommand(vaultCmd)

	vaultSetCmd.Flags().StringVar(&flagVaultValue, "value", "", "Secret value (use '-' to read from stdin)")
	vaultSetCmd.Flags().StringVar(&flagVaultFromFile, "from-file", "", "Read value from this file")
	vaultSetCmd.Flags().StringVar(&flagVaultLabel, "label", "", "Optional human-readable label")

	vaultCmd.AddCommand(vaultListCmd)
	vaultCmd.AddCommand(vaultSetCmd)
	vaultCmd.AddCommand(vaultDeleteCmd)
}

func runVaultList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	secrets, err := c.ListVault()
	if err != nil {
		return mapDaemonError(err, "listing vault")
	}
	out := schema.VaultListData{Secrets: make([]schema.VaultSecretData, 0, len(secrets))}
	for _, s := range secrets {
		out.Secrets = append(out.Secrets, schema.VaultSecretData{
			Name:      s.Name,
			Label:     s.Label,
			CreatedAt: s.CreatedAt,
			UpdatedAt: s.UpdatedAt,
		})
	}
	return output.Emit(out)
}

func runVaultSet(cmd *cobra.Command, args []string) error {
	name := args[0]
	if name == "" {
		return schema.Newf(schema.CodeValidationError, "secret name cannot be empty")
	}

	if flagVaultValue == "" && flagVaultFromFile == "" {
		return schema.Newf(schema.CodeValidationError,
			"--value or --from-file is required").
			WithHint("use --value - to read from stdin")
	}
	if flagVaultValue != "" && flagVaultFromFile != "" {
		return schema.Newf(schema.CodeValidationError,
			"--value and --from-file are mutually exclusive")
	}

	value, err := resolveVaultValue()
	if err != nil {
		return err
	}
	if value == "" {
		return schema.Newf(schema.CodeValidationError, "secret value cannot be empty")
	}

	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.SetVault(name, value, flagVaultLabel); err != nil {
		return mapDaemonError(err, "setting vault secret")
	}
	return output.Emit(schema.VaultSetData{Name: name, Status: "stored"})
}

func runVaultDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.DeleteVault(name); err != nil {
		return mapDaemonError(err, "deleting vault secret")
	}
	return output.Emit(schema.VaultDeleteData{Name: name, Status: "deleted"})
}

func resolveVaultValue() (string, error) {
	if flagVaultFromFile != "" {
		b, err := os.ReadFile(flagVaultFromFile)
		if err != nil {
			return "", schema.Newf(schema.CodePathNotFound,
				"reading value file: %s", err.Error())
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	if flagVaultValue == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", schema.Newf(schema.CodeInternal,
				"reading value from stdin: %s", err.Error())
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return flagVaultValue, nil
}
