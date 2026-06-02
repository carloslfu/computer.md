// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"fmt"
	"strings"
)

// Text formatters for the per-command Data shapes. Implementing
// TextFormatter on the data types (not on cmd-side helpers) means JSON
// and text are renderings of the SAME data — they can't drift apart.
// New fields automatically show up in both by changing one file.

// TextFormat renders StatusData as the dashboard-style indicator + key/value.
func (s StatusData) TextFormat() string {
	var sb strings.Builder

	indicator := "●"
	switch s.Status {
	case "ok", "healthy", "running":
		indicator = "\033[32m●\033[0m"
	case "degraded", "warning":
		indicator = "\033[33m●\033[0m"
	case "error", "unreachable":
		indicator = "\033[31m●\033[0m"
	}
	fmt.Fprintf(&sb, "%s Daemon: %s\n\n", indicator, s.Status)
	if s.MachineID != "" {
		fmt.Fprintf(&sb, "  Machine: %s\n", s.MachineID)
	}
	if s.UptimeSeconds > 0 {
		fmt.Fprintf(&sb, "  Uptime:  %s\n", formatUptime(s.UptimeSeconds))
	}
	if s.CurrentTask != "" {
		fmt.Fprintf(&sb, "  Task:    %s\n", s.CurrentTask)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders a TaskData as a short, human-readable block.
func (t TaskData) TextFormat() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task:    %s\n", t.ID)
	fmt.Fprintf(&sb, "Status:  %s\n", t.Status)
	if t.Instruction != "" {
		fmt.Fprintf(&sb, "Prompt:  %s\n", oneLine(t.Instruction, 200))
	}
	if t.CreatedAt != "" {
		fmt.Fprintf(&sb, "Created: %s\n", t.CreatedAt)
	}
	if t.UpdatedAt != "" {
		fmt.Fprintf(&sb, "Updated: %s\n", t.UpdatedAt)
	}
	if t.Result != "" {
		fmt.Fprintf(&sb, "\n%s", t.Result)
	}
	if t.Error != "" {
		fmt.Fprintf(&sb, "\nError: %s\n", t.Error)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders TaskSubmitData.
func (t TaskSubmitData) TextFormat() string {
	return fmt.Sprintf("Task %s submitted (status: %s).", t.ID, t.Status)
}

// TextFormat renders TaskListData as a short table.
func (l TaskListData) TextFormat() string {
	if len(l.Tasks) == 0 {
		return "No tasks."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-36s  %-18s  %-20s  %s\n", "ID", "STATUS", "CREATED", "PROMPT")
	sb.WriteString(strings.Repeat("-", 100))
	sb.WriteByte('\n')
	for _, t := range l.Tasks {
		fmt.Fprintf(&sb, "%-36s  %-18s  %-20s  %s\n", t.ID, t.Status, t.CreatedAt, oneLine(t.Instruction, 40))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders TaskRespondData.
func (r TaskRespondData) TextFormat() string {
	return fmt.Sprintf("Response sent to task %s (status: %s).", r.ID, r.Status)
}

// TextFormat renders TaskCancelData.
func (c TaskCancelData) TextFormat() string {
	return fmt.Sprintf("Task %s cancelled.", c.ID)
}

// TextFormat renders TaskMessagesData.
func (m TaskMessagesData) TextFormat() string {
	if len(m.Messages) == 0 {
		return "No messages."
	}
	var sb strings.Builder
	for i, msg := range m.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "[%s] %s\n%s\n", msg.Role, msg.CreatedAt, msg.Content)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders AuthWhoamiData.
func (w AuthWhoamiData) TextFormat() string {
	if len(w.Machines) == 0 {
		return "Not authenticated.\nRun 'vibecraft auth login' to connect to your machine."
	}
	var sb strings.Builder
	for _, m := range w.Machines {
		marker := "  "
		if m.Active {
			marker = "* "
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		fmt.Fprintf(&sb, "%s%s (%s)\n", marker, name, m.ID)
		fmt.Fprintf(&sb, "    URL: %s\n", m.URL)
		fmt.Fprintf(&sb, "    API key: %s\n", m.APIKeyHint)
	}
	if len(w.Machines) > 1 {
		sb.WriteString("\nUse 'vibecraft machine select <id>' to switch.")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders AuthLoginData.
func (a AuthLoginData) TextFormat() string {
	return fmt.Sprintf("Authenticated to %s\n  Machine: %s\n  Key:     %s",
		a.MachineURL, a.MachineID, a.APIKeyHint)
}

// TextFormat renders AuthLogoutData.
func (a AuthLogoutData) TextFormat() string {
	return "Logged out."
}

// TextFormat renders AuthPrintData. WARNING: this leaks the full key into
// stdout — that is the verb's whole purpose (handoff between agents).
func (a AuthPrintData) TextFormat() string {
	return fmt.Sprintf("VIBECRAFT_MACHINE_URL=%s\nVIBECRAFT_API_KEY=%s",
		a.MachineURL, a.APIKey)
}

// TextFormat renders MachineSelectData.
func (m MachineSelectData) TextFormat() string {
	return fmt.Sprintf("Active machine: %s", m.ActiveMachine)
}

// TextFormat renders MachineRemoveData.
func (m MachineRemoveData) TextFormat() string {
	if m.ActiveMachine != "" {
		return fmt.Sprintf("Removed %s. Active machine: %s", m.Removed, m.ActiveMachine)
	}
	return fmt.Sprintf("Removed %s.", m.Removed)
}

// TextFormat renders ScreenshotData.
func (s ScreenshotData) TextFormat() string {
	return fmt.Sprintf("Screenshot saved to %s (%d bytes)", s.Path, s.Bytes)
}

// TextFormat renders VersionData.
func (v VersionData) TextFormat() string {
	if v.Commit != "" {
		return fmt.Sprintf("vibecraft %s (commit %s)", v.Version, v.Commit)
	}
	return fmt.Sprintf("vibecraft %s", v.Version)
}

// TextFormat renders DocsData by printing the raw markdown — same content
// either way; in text mode the agent gets just the doc, in JSON mode it
// gets a wrapper around it.
func (d DocsData) TextFormat() string {
	return d.Content
}

// TextFormat renders VaultListData.
func (v VaultListData) TextFormat() string {
	if len(v.Secrets) == 0 {
		return "No secrets."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-32s  %s\n", "NAME", "LABEL")
	sb.WriteString(strings.Repeat("-", 60))
	sb.WriteByte('\n')
	for _, s := range v.Secrets {
		fmt.Fprintf(&sb, "%-32s  %s\n", s.Name, s.Label)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders VaultSetData.
func (v VaultSetData) TextFormat() string {
	return fmt.Sprintf("Stored secret %q.", v.Name)
}

// TextFormat renders VaultDeleteData.
func (v VaultDeleteData) TextFormat() string {
	return fmt.Sprintf("Deleted secret %q.", v.Name)
}

// TextFormat renders MemoryListData.
func (m MemoryListData) TextFormat() string {
	if len(m.Items) == 0 {
		return "No memory items."
	}
	var sb strings.Builder
	for i, it := range m.Items {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "[%s/%s]  %s\n%s\n", it.Category, it.Key, it.ID, oneLine(it.Value, 200))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders MemoryItemData (for memory set output).
func (m MemoryItemData) TextFormat() string {
	return fmt.Sprintf("Stored %s/%s (id=%s)", m.Category, m.Key, m.ID)
}

// TextFormat renders MemoryDeleteData.
func (m MemoryDeleteData) TextFormat() string {
	return fmt.Sprintf("Deleted memory item %q.", m.ID)
}

// TextFormat renders NotificationListData.
func (n NotificationListData) TextFormat() string {
	if len(n.Notifications) == 0 {
		return "No notifications."
	}
	var sb strings.Builder
	for _, no := range n.Notifications {
		marker := "  "
		if no.ReadAt == "" {
			marker = "* "
		}
		fmt.Fprintf(&sb, "%s[%s] %s\n  %s\n", marker, no.Kind, no.Title, oneLine(no.Body, 200))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders NotificationAckData.
func (n NotificationAckData) TextFormat() string {
	if n.Acked == 0 {
		return "Nothing to acknowledge."
	}
	if n.Acked == 1 {
		return "Acknowledged 1 notification."
	}
	return fmt.Sprintf("Acknowledged %d notifications.", n.Acked)
}

// TextFormat renders FilesLsData as a short ls-style table.
func (f FilesLsData) TextFormat() string {
	if len(f.Entries) == 0 {
		return fmt.Sprintf("(empty) %s", f.Path)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s:\n", f.Path)
	for _, e := range f.Entries {
		mark := " "
		switch e.Type {
		case "dir":
			mark = "/"
		case "symlink":
			mark = "@"
		case "other":
			mark = "?"
		}
		fmt.Fprintf(&sb, "  %s%-32s  %10d  %s\n", mark, e.Name, e.Size, e.MTime)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders FilesPushData.
func (f FilesPushData) TextFormat() string {
	return fmt.Sprintf("Pushed %s → %s (%d bytes)", f.LocalPath, f.RemotePath, f.Bytes)
}

// TextFormat renders FilesPullData.
func (f FilesPullData) TextFormat() string {
	return fmt.Sprintf("Pulled %s → %s (%d bytes)", f.RemotePath, f.LocalPath, f.Bytes)
}

// TextFormat renders TaskCredentialsData.
func (t TaskCredentialsData) TextFormat() string {
	if t.Status == "dismissed" {
		return fmt.Sprintf("Credential request on task %s dismissed.", t.ID)
	}
	if len(t.Names) > 0 {
		return fmt.Sprintf("Submitted %d credential(s) to task %s: %s",
			len(t.Names), t.ID, strings.Join(t.Names, ", "))
	}
	return fmt.Sprintf("Credentials submitted to task %s.", t.ID)
}

// TextFormat renders RulesListData.
func (r RulesListData) TextFormat() string {
	if len(r.Rules) == 0 {
		return "No guardrail rules."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-10s  %-22s  %-30s  %s\n", "ACTION", "NAME", "PATTERN", "ID")
	sb.WriteString(strings.Repeat("-", 90))
	sb.WriteByte('\n')
	for _, rule := range r.Rules {
		state := rule.Action
		if !rule.Enabled {
			state += " (off)"
		}
		fmt.Fprintf(&sb, "%-10s  %-22s  %-30s  %s\n",
			state, oneLine(rule.Name, 22), oneLine(rule.Pattern, 30), rule.ID)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders RuleData (the rules add result).
func (r RuleData) TextFormat() string {
	return fmt.Sprintf("Added rule %q (%s %q) — id %s", r.Name, r.Action, r.Pattern, r.ID)
}

// TextFormat renders RuleDeleteData.
func (r RuleDeleteData) TextFormat() string {
	return fmt.Sprintf("Deleted rule %s.", r.ID)
}

// TextFormat renders AppsListData.
func (a AppsListData) TextFormat() string {
	if len(a.Apps) == 0 {
		return "No deployed tools."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-24s  %-6s  %-7s  %s\n", "NAME", "PORT", "SSO", "URL")
	sb.WriteString(strings.Repeat("-", 80))
	sb.WriteByte('\n')
	for _, app := range a.Apps {
		sso := "off"
		if app.SSOEnabled {
			sso = "on"
		}
		fmt.Fprintf(&sb, "%-24s  %-6d  %-7s  %s\n", app.Name, app.Port, sso, app.URL)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders AppActionData.
func (a AppActionData) TextFormat() string {
	return fmt.Sprintf("Tool %q: %s", a.Name, a.Status)
}

// TextFormat renders AppRestartData.
func (a AppRestartData) TextFormat() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Tool %q: %s\n", a.Name, a.Status)
	if a.Unit != "" {
		fmt.Fprintf(&sb, "  Unit:      %s\n", a.Unit)
	}
	fmt.Fprintf(&sb, "  Old PID:   %d\n", a.OldPID)
	fmt.Fprintf(&sb, "  New PID:   %d\n", a.NewPID)
	if a.Port != 0 {
		fmt.Fprintf(&sb, "  Port:      %d\n", a.Port)
		fmt.Fprintf(&sb, "  Listening: %v\n", a.ListeningAfter)
	}
	if a.Detail != "" {
		fmt.Fprintf(&sb, "  Detail:    %s\n", a.Detail)
	}
	// Honesty footer: restart re-runs the existing build. A code/env change
	// needs deploy — say so here so a successful-looking restart isn't read
	// as "my change is live."
	fmt.Fprintf(&sb, "  Note:      re-ran the existing build; run 'vibecraft tools deploy %s' to apply a code/env change.", a.Name)
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders ToolDeployData — the deterministic "now serving" line.
func (d ToolDeployData) TextFormat() string {
	var sb strings.Builder
	verdict := "deploy failed"
	if d.OK {
		verdict = "deployed"
	}
	fmt.Fprintf(&sb, "Tool %q: %s (%s)\n", d.Name, verdict, d.Status)
	if d.Unit != "" {
		fmt.Fprintf(&sb, "  Unit:      %s\n", d.Unit)
	}
	fmt.Fprintf(&sb, "  Old PID:   %d\n", d.OldPID)
	fmt.Fprintf(&sb, "  New PID:   %d\n", d.NewPID)
	if d.Port != 0 {
		fmt.Fprintf(&sb, "  Port:      %d\n", d.Port)
		fmt.Fprintf(&sb, "  Listening: %v\n", d.ListeningAfter)
	}
	if d.Detail != "" {
		fmt.Fprintf(&sb, "  Detail:    %s\n", d.Detail)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders ToolStatusData.
func (s ToolStatusData) TextFormat() string {
	var sb strings.Builder
	state := s.ActiveState
	if s.SubState != "" {
		state += "/" + s.SubState
	}
	if state == "" {
		state = "unknown"
	}
	fmt.Fprintf(&sb, "Tool %q: %s\n", s.Name, state)
	if s.URL != "" {
		fmt.Fprintf(&sb, "  URL:        %s\n", s.URL)
	}
	fmt.Fprintf(&sb, "  Port:       %d (listening=%v)\n", s.Port, s.Listening)
	fmt.Fprintf(&sb, "  Main PID:   %d\n", s.MainPID)
	if s.Since != "" {
		fmt.Fprintf(&sb, "  Since:      %s\n", s.Since)
	}
	if s.ExecStart != "" {
		fmt.Fprintf(&sb, "  ExecStart:  %s\n", s.ExecStart)
	}
	if s.WorkingDirectory != "" {
		fmt.Fprintf(&sb, "  WorkingDir: %s\n", s.WorkingDirectory)
	}
	if s.GitCommit != "" {
		dirty := ""
		if s.GitDirty {
			dirty = " (working copy dirty)"
		}
		fmt.Fprintf(&sb, "  Git:        %s%s\n", s.GitCommit, dirty)
	}
	if len(s.EnvKeys) > 0 {
		fmt.Fprintf(&sb, "  Env keys:   %s\n", strings.Join(s.EnvKeys, ", "))
	}
	fmt.Fprintf(&sb, "  SSO:        %v\n", s.SSOEnabled)
	if !s.UnitPresent {
		sb.WriteString("  (no systemd unit on disk — never deployed, or removed)\n")
	}
	if s.Detail != "" {
		fmt.Fprintf(&sb, "  Detail:     %s\n", s.Detail)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders ToolLogsData — raw journal lines, newline-joined.
func (l ToolLogsData) TextFormat() string {
	if len(l.Lines) == 0 {
		if l.Detail != "" {
			return l.Detail
		}
		return fmt.Sprintf("No logs for %q.", l.Name)
	}
	return strings.Join(l.Lines, "\n")
}

// TextFormat renders ToolEnvData — keys only, reserved ones marked.
func (e ToolEnvData) TextFormat() string {
	if len(e.Keys) == 0 {
		if e.Detail != "" {
			return e.Detail
		}
		return fmt.Sprintf("Tool %q has no environment variables.", e.Name)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Environment for %q (keys only — values are never shown):\n", e.Name)
	for _, k := range e.Keys {
		marker := ""
		if k.Reserved {
			marker = "  (VibeCraft-managed)"
		}
		fmt.Fprintf(&sb, "  %s%s\n", k.Key, marker)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TextFormat renders ComputerMDData — prints the raw markdown.
func (c ComputerMDData) TextFormat() string {
	return c.Content
}

// TextFormat renders UpdateData.
func (u UpdateData) TextFormat() string {
	switch u.Status {
	case "up_to_date":
		return fmt.Sprintf("Already at %s.", u.CurrentVersion)
	case "updated":
		return fmt.Sprintf("Updated %s → %s (%s).", u.CurrentVersion, u.LatestVersion, u.Path)
	}
	return fmt.Sprintf("Update status: %s", u.Status)
}

// TextFormat renders InstallSkillData.
func (i InstallSkillData) TextFormat() string {
	switch i.Action {
	case "installed":
		return fmt.Sprintf("Installed %s skill at %s", i.Target, i.Path)
	case "uninstalled":
		return fmt.Sprintf("Uninstalled %s skill at %s", i.Target, i.Path)
	case "noop":
		return fmt.Sprintf("No %s skill found at %s", i.Target, i.Path)
	}
	return fmt.Sprintf("%s skill: %s (%s)", i.Target, i.Action, i.Path)
}

// TextFormat renders UninstallData.
func (u UninstallData) TextFormat() string {
	var sb strings.Builder
	if len(u.Removed) == 0 {
		sb.WriteString("Nothing to remove.")
	} else {
		sb.WriteString("Removed:")
		for _, p := range u.Removed {
			fmt.Fprintf(&sb, "\n  %s", p)
		}
	}
	if !u.BinaryRemoved && u.BinaryPath != "" {
		fmt.Fprintf(&sb, "\n\nStill on disk: %s", u.BinaryPath)
	}
	if u.Note != "" {
		fmt.Fprintf(&sb, "\n\n%s", u.Note)
	}
	return sb.String()
}

func formatUptime(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

// oneLine collapses whitespace and truncates a long string for table display.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		if max <= 3 {
			return s[:max]
		}
		return s[:max-3] + "..."
	}
	return s
}
