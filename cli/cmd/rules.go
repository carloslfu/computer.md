// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Guardrail rules — the policy layer that decides whether an agent
// action is allowed, needs confirmation, or is blocked.

var rulesCmd = &cobra.Command{
	Use:   "rules",
	Short: "Manage guardrail rules",
	Long: `List, add, and remove the machine's guardrail rules. A rule matches a
command/action pattern and decides: allow, confirm, or block.`,
}

var rulesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List guardrail rules",
	RunE:  runRulesList,
}

var (
	flagRuleName    string
	flagRulePattern string
	flagRuleAction  string
	flagRuleDesc    string
)

var rulesAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a guardrail rule",
	Long: `Add a guardrail rule.

  vibecraft rules add --name "block rm -rf" --pattern "rm -rf" --action block
  vibecraft rules add --name "confirm deploys" --pattern "deploy" --action confirm

--action is one of: allow, confirm, block.`,
	RunE: runRulesAdd,
}

var rulesDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Remove a guardrail rule",
	Args:  cobra.ExactArgs(1),
	RunE:  runRulesDelete,
}

func init() {
	rulesAddCmd.Flags().StringVar(&flagRuleName, "name", "", "Rule name (required)")
	rulesAddCmd.Flags().StringVar(&flagRulePattern, "pattern", "", "Match pattern (required)")
	rulesAddCmd.Flags().StringVar(&flagRuleAction, "action", "", "allow | confirm | block (required)")
	rulesAddCmd.Flags().StringVar(&flagRuleDesc, "description", "", "Optional description")

	rootCmd.AddCommand(rulesCmd)
	rulesCmd.AddCommand(rulesListCmd)
	rulesCmd.AddCommand(rulesAddCmd)
	rulesCmd.AddCommand(rulesDeleteCmd)
}

func runRulesList(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	rules, err := c.ListRules()
	if err != nil {
		return mapDaemonError(err, "listing rules")
	}
	out := schema.RulesListData{Rules: make([]schema.RuleData, 0, len(rules))}
	for _, r := range rules {
		out.Rules = append(out.Rules, schema.RuleData{
			ID:          r.ID,
			Name:        r.Name,
			Pattern:     r.Pattern,
			Action:      r.Action,
			Description: r.Description,
			Enabled:     r.Enabled,
			Priority:    r.Priority,
		})
	}
	return output.Emit(out)
}

func runRulesAdd(cmd *cobra.Command, args []string) error {
	if flagRuleName == "" || flagRulePattern == "" || flagRuleAction == "" {
		return schema.Newf(schema.CodeValidationError,
			"--name, --pattern, and --action are all required")
	}
	switch flagRuleAction {
	case "allow", "confirm", "block":
	default:
		return schema.Newf(schema.CodeValidationError,
			"--action must be allow, confirm, or block (got %q)", flagRuleAction)
	}

	c, err := newClient()
	if err != nil {
		return err
	}
	created, err := c.AddRule(client.Rule{
		Name:        flagRuleName,
		Pattern:     flagRulePattern,
		Action:      flagRuleAction,
		Description: flagRuleDesc,
		Enabled:     true,
	})
	if err != nil {
		return mapDaemonError(err, "adding rule")
	}
	return output.Emit(schema.RuleData{
		ID:          created.ID,
		Name:        created.Name,
		Pattern:     created.Pattern,
		Action:      created.Action,
		Description: created.Description,
		Enabled:     created.Enabled,
		Priority:    created.Priority,
	})
}

func runRulesDelete(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.DeleteRule(args[0]); err != nil {
		return mapDaemonError(err, "deleting rule")
	}
	return output.Emit(schema.RuleDeleteData{ID: args[0], Status: "deleted"})
}
