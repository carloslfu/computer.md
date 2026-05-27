// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
)

// Read-only daemon inspection verbs: system, audit, usage, config.
// These pass the daemon's JSON through unchanged — the CLI's job here
// is the envelope + exit code, not reshaping. In --text mode the
// output package pretty-prints the JSON (no per-field formatter — these
// are inspection commands an agent reads as JSON anyway).

var systemCmd = &cobra.Command{
	Use:   "system",
	Short: "Show machine system info (CPU, memory, kernel, installed agents)",
	Long: `Print the machine's system information: version, CPU, memory, kernel,
uptime, and the worker agents the manager can spawn (Claude Code, Codex).`,
	RunE: runSystem,
}

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Read the machine's audit log",
	Long: `Print audit log entries — every action the manager and the platform
took on this machine.

  vibecraft audit                       # most recent 100
  vibecraft audit --limit 20
  vibecraft audit --task <task-id>      # entries for one task
  vibecraft audit --category guardrail  # entries in one category`,
	RunE: runAudit,
}

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show AI usage and spend",
	Long: `Print the machine's AI usage summary (tokens, cost) for a period.
Defaults to the current month.

  vibecraft usage
  vibecraft usage --start 2026-05-01 --end 2026-05-31 --top 10`,
	RunE: runUsage,
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show the machine's daemon configuration",
	Long:  `Print the machine's daemon configuration (machine id, host, ports, data dirs, routes).`,
	RunE:  runConfig,
}

var (
	flagAuditLimit    int
	flagAuditTask     string
	flagAuditCategory string
	flagUsageStart    string
	flagUsageEnd      string
	flagUsageTop      int
)

func init() {
	rootCmd.AddCommand(systemCmd)

	auditCmd.Flags().IntVar(&flagAuditLimit, "limit", 0, "Max entries (default 100)")
	auditCmd.Flags().StringVar(&flagAuditTask, "task", "", "Filter to one task id")
	auditCmd.Flags().StringVar(&flagAuditCategory, "category", "", "Filter to one category")
	rootCmd.AddCommand(auditCmd)

	usageCmd.Flags().StringVar(&flagUsageStart, "start", "", "Start date YYYY-MM-DD")
	usageCmd.Flags().StringVar(&flagUsageEnd, "end", "", "End date YYYY-MM-DD")
	usageCmd.Flags().IntVar(&flagUsageTop, "top", 0, "How many top line-items to include")
	rootCmd.AddCommand(usageCmd)

	rootCmd.AddCommand(configCmd)
}

func runSystem(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	data, err := c.GetJSON("/system")
	if err != nil {
		return mapDaemonError(err, "fetching system info")
	}
	return output.Emit(data)
}

func runAudit(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	q := url.Values{}
	if flagAuditLimit > 0 {
		q.Set("limit", strconv.Itoa(flagAuditLimit))
	}
	if flagAuditTask != "" {
		q.Set("task_id", flagAuditTask)
	}
	if flagAuditCategory != "" {
		q.Set("category", flagAuditCategory)
	}
	path := "/audit"
	if e := q.Encode(); e != "" {
		path += "?" + e
	}
	data, err := c.GetJSON(path)
	if err != nil {
		return mapDaemonError(err, "fetching audit log")
	}
	return output.Emit(map[string]any{"entries": data})
}

func runUsage(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	q := url.Values{}
	if flagUsageStart != "" {
		q.Set("start", flagUsageStart)
	}
	if flagUsageEnd != "" {
		q.Set("end", flagUsageEnd)
	}
	if flagUsageTop > 0 {
		q.Set("top", strconv.Itoa(flagUsageTop))
	}
	path := "/usage"
	if e := q.Encode(); e != "" {
		path += "?" + e
	}
	data, err := c.GetJSON(path)
	if err != nil {
		return mapDaemonError(err, "fetching usage")
	}
	return output.Emit(data)
}

func runConfig(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	data, err := c.GetJSON("/config")
	if err != nil {
		return mapDaemonError(err, "fetching config")
	}
	return output.Emit(data)
}
