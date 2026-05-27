// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"sort"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var machineCmd = &cobra.Command{
	Use:   "machine",
	Short: "List and select the machines on your account",
	Long: `List every machine on your VibeCraft account, set the default one, or
drop a machine from local config.`,
}

var machineListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every machine on your account",
	Long: `List every machine your account controls — a live query to the
platform when you are account-logged-in, so machines provisioned since
login show up. The CLI brokers each machine's key on first use.`,
	RunE: runMachineList,
}

var machineSelectCmd = &cobra.Command{
	Use:   "select <machine-id>",
	Short: "Set the default machine for commands without --machine",
	Args:  cobra.ExactArgs(1),
	RunE:  runMachineSelect,
}

var machineRemoveCmd = &cobra.Command{
	Use:   "remove <machine-id>",
	Short: "Remove a machine from local config",
	Args:  cobra.ExactArgs(1),
	RunE:  runMachineRemove,
}

func init() {
	rootCmd.AddCommand(machineCmd)
	machineCmd.AddCommand(machineListCmd)
	machineCmd.AddCommand(machineSelectCmd)
	machineCmd.AddCommand(machineRemoveCmd)
}

func runMachineList(cmd *cobra.Command, args []string) error {
	// Resolve the account key the same way every other account-scoped
	// verb does: VIBECRAFT_ACCOUNT_KEY first, then the stored config.
	// Gating on cfg.AccountKey directly (as this used to) silently
	// skipped the live fleet query under headless / CI auth.
	accountKey, keyErr := loadAccountKey()
	if keyErr != nil {
		if se, ok := keyErr.(*schema.Error); ok && se.Code == schema.CodeAuthRequired {
			// Not account-logged-in — fall back to the local config.
			cfg, _ := loadConfig()
			return output.Emit(machinesToWhoami(cfg))
		}
		return keyErr
	}

	// Account login: ask the platform for the live fleet — every machine
	// the account controls, not just the ones the CLI has touched.
	machineList, listErr := listAccountMachines(accountKey)
	if listErr != nil {
		return listErr
	}

	// Refresh the local config as a side effect so offline `machine list`
	// and fan-out stay current — but only when there is a config file
	// (VIBECRAFT_ACCOUNT_KEY / headless auth has none).
	cfg, _ := loadConfig()
	activeMachine := ""
	if cfg != nil {
		known := map[string]bool{}
		rebuilt := map[string]MachineConfig{}
		for _, m := range machineList {
			known[m.ID] = true
			prev := cfg.Machines[m.ID]
			rebuilt[m.ID] = MachineConfig{
				URL:    "https://" + m.Host,
				APIKey: prev.APIKey, // keep a brokered key if we have one
				Name:   m.Name,
			}
		}
		cfg.Machines = rebuilt
		if cfg.ActiveMachine != "" && !known[cfg.ActiveMachine] {
			cfg.ActiveMachine = ""
		}
		if cfg.ActiveMachine == "" && len(machineList) == 1 {
			cfg.ActiveMachine = machineList[0].ID
		}
		_ = saveConfig(cfg)
		activeMachine = cfg.ActiveMachine
	}

	data := schema.AuthWhoamiData{ActiveMachine: activeMachine}
	for _, m := range machineList {
		data.Machines = append(data.Machines, schema.AuthMachineData{
			ID:     m.ID,
			URL:    "https://" + m.Host,
			Name:   m.Name,
			Access: m.Access,
			Active: m.ID == activeMachine,
		})
	}
	return output.Emit(data)
}

func runMachineSelect(cmd *cobra.Command, args []string) error {
	id := args[0]

	cfg, err := loadConfig()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "%s", err.Error())
	}
	if cfg == nil || len(cfg.Machines) == 0 {
		return schema.Newf(schema.CodeAuthRequired, "no machines configured").
			WithHint("run 'vibecraft auth login' first")
	}

	if _, ok := cfg.Machines[id]; !ok {
		return schema.Newf(schema.CodeMachineNotFound,
			"machine %q not found in config", id).
			WithHint("run 'vibecraft machine list' to see available machines")
	}

	cfg.ActiveMachine = id
	if err := saveConfig(cfg); err != nil {
		return schema.Newf(schema.CodeInternal, "saving config: %s", err.Error())
	}

	return output.Emit(schema.MachineSelectData{ActiveMachine: id})
}

func runMachineRemove(cmd *cobra.Command, args []string) error {
	id := args[0]

	cfg, err := loadConfig()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "%s", err.Error())
	}
	if cfg == nil || len(cfg.Machines) == 0 {
		return schema.Newf(schema.CodeAuthRequired, "no machines configured")
	}

	if _, ok := cfg.Machines[id]; !ok {
		return schema.Newf(schema.CodeMachineNotFound,
			"machine %q not found in config", id)
	}

	delete(cfg.Machines, id)

	// If we removed the active machine, pick another (deterministic by sort).
	if cfg.ActiveMachine == id {
		cfg.ActiveMachine = ""
		ids := make([]string, 0, len(cfg.Machines))
		for remaining := range cfg.Machines {
			ids = append(ids, remaining)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			cfg.ActiveMachine = ids[0]
		}
	}

	if err := saveConfig(cfg); err != nil {
		return schema.Newf(schema.CodeInternal, "saving config: %s", err.Error())
	}

	return output.Emit(schema.MachineRemoveData{
		Removed:       id,
		ActiveMachine: cfg.ActiveMachine,
	})
}
