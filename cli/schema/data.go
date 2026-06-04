// SPDX-License-Identifier: Apache-2.0

package schema

// Per-command Data shapes. One type per command. Field names are JSON
// snake_case for consistency with the existing daemon API. Optional
// fields use `omitempty` so absent values are absent on the wire, not
// `null` or `""`.
//
// New fields are additive (older clients ignore them). Renames or
// removals are breaking and require bumping schema.Version.

// StatusData is emitted by `vibecraft status`.
type StatusData struct {
	MachineID     string `json:"machine_id"`
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	CurrentTask   string `json:"current_task,omitempty"`
}

// TaskData is emitted by `vibecraft task get`, `vibecraft task submit --wait`,
// and `vibecraft task wait`. The `result` and `error` fields are only set
// when status is terminal.
type TaskData struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Instruction     string `json:"instruction,omitempty"`
	Result          string `json:"result,omitempty"`
	Error           string `json:"error,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
}

// TaskSubmitData is emitted by `vibecraft task submit --no-wait`.
// Lighter shape than TaskData since the task hasn't completed.
type TaskSubmitData struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	ConversationID string `json:"conversation_id,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// TaskListData is emitted by `vibecraft task list`.
type TaskListData struct {
	Tasks      []TaskData `json:"tasks"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// TaskRespondData is emitted by `vibecraft task respond`.
type TaskRespondData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// TaskCancelData is emitted by `vibecraft task cancel`.
type TaskCancelData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// MessageData is one row of a conversation transcript.
type MessageData struct {
	ID        string `json:"id,omitempty"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	TaskID    string `json:"task_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// TaskMessagesData is emitted by `vibecraft task messages`.
type TaskMessagesData struct {
	ID             string        `json:"id"`
	ConversationID string        `json:"conversation_id"`
	Messages       []MessageData `json:"messages"`
}

// AuthMachineData describes one machine in `vibecraft auth whoami` /
// `machine list` output. The active machine has Active=true. APIKeyHint
// is omitted when no per-machine daemon key has been brokered yet
// (account login lists machines before any key exists for them).
type AuthMachineData struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	Name       string `json:"name,omitempty"`
	APIKeyHint string `json:"api_key_hint,omitempty"`
	Access     string `json:"access,omitempty"`
	Active     bool   `json:"active"`
}

// AuthWhoamiData is emitted by `vibecraft auth whoami` (formerly `auth status`).
type AuthWhoamiData struct {
	ActiveMachine string            `json:"active_machine,omitempty"`
	Machines      []AuthMachineData `json:"machines"`
}

// AuthLoginData is emitted by `vibecraft auth login`. The full API key is NOT
// returned here — use `vibecraft auth print` if you need to copy it.
type AuthLoginData struct {
	MachineID  string `json:"machine_id"`
	MachineURL string `json:"machine_url"`
	APIKeyHint string `json:"api_key_hint"`
}

// AuthLogoutData is emitted by `vibecraft auth logout`. Empty.
type AuthLogoutData struct{}

// AuthPrintData is emitted by `vibecraft auth print`. This DOES contain the
// full API key — it's the explicit "give me my credentials to paste into an
// agent's environment" verb.
type AuthPrintData struct {
	MachineID  string `json:"machine_id"`
	MachineURL string `json:"machine_url"`
	APIKey     string `json:"api_key"`
}

// MachineSelectData is emitted by `vibecraft machine select`.
type MachineSelectData struct {
	ActiveMachine string `json:"active_machine"`
}

// MachineRemoveData is emitted by `vibecraft machine remove`.
type MachineRemoveData struct {
	Removed       string `json:"removed"`
	ActiveMachine string `json:"active_machine,omitempty"`
}

// ScreenshotData is emitted by `vibecraft screenshot`. The PNG itself is
// written to the path given by `--out` (or an auto-generated filename);
// the JSON describes the write.
type ScreenshotData struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Ts    string `json:"ts"`
}

// VersionData is emitted by `vibecraft version`.
type VersionData struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"built_at,omitempty"`
}

// DocsData is emitted by `vibecraft docs --json`.
type DocsData struct {
	URL     string `json:"url"`
	Version int    `json:"schema_version"`
	Content string `json:"content"`
}

// UninstallData is emitted by `vibecraft uninstall`. It reports the local
// teardown only — removing credentials here does not revoke them
// server-side (a separate dashboard action).
type UninstallData struct {
	Removed       []string `json:"removed"`               // filesystem paths deleted
	BinaryPath    string   `json:"binary_path,omitempty"` // resolved path of the CLI binary
	BinaryRemoved bool     `json:"binary_removed"`        // false on Windows / when not deletable
	Note          string   `json:"note,omitempty"`        // human follow-up (manual rm, key revocation)
}

// VaultSecretData describes one stored secret's metadata. Never the value.
type VaultSecretData struct {
	Name      string `json:"name"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// VaultListData is emitted by `vibecraft vault list`.
type VaultListData struct {
	Secrets []VaultSecretData `json:"secrets"`
}

// VaultSetData is emitted by `vibecraft vault set <name>`.
type VaultSetData struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// VaultDeleteData is emitted by `vibecraft vault delete <name>`.
type VaultDeleteData struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// MemoryItemData is one entry in memory list / set output.
type MemoryItemData struct {
	ID        string  `json:"id"`
	Category  string  `json:"category"`
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	Metadata  *string `json:"metadata,omitempty"`
	CreatedAt string  `json:"created_at,omitempty"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

// MemoryListData is emitted by `vibecraft memory list`.
type MemoryListData struct {
	Items []MemoryItemData `json:"items"`
}

// MemoryDeleteData is emitted by `vibecraft memory delete <id>`.
type MemoryDeleteData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// NotificationData is emitted by `vibecraft notifications list` (one per).
type NotificationData struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Title          string `json:"title,omitempty"`
	Body           string `json:"body,omitempty"`
	Priority       string `json:"priority,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	ReadAt         string `json:"read_at,omitempty"`
}

// NotificationListData is the envelope for `vibecraft notifications list`.
type NotificationListData struct {
	Notifications []NotificationData `json:"notifications"`
}

// NotificationAckData is emitted by `vibecraft notifications ack`.
type NotificationAckData struct {
	Acked int `json:"acked"`
}

// FilesEntryData is one entry of `vibecraft files ls`.
type FilesEntryData struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size,omitempty"`
	Type  string `json:"type"` // file | dir | symlink | other
	MTime string `json:"mtime"`
}

// FilesLsData is emitted by `vibecraft files ls`.
type FilesLsData struct {
	Path    string           `json:"path"`
	Entries []FilesEntryData `json:"entries"`
}

// FilesPushData is emitted by `vibecraft files push`.
type FilesPushData struct {
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
	Bytes      int64  `json:"bytes"`
	Original   string `json:"original,omitempty"`
}

// FilesPullData is emitted by `vibecraft files pull`.
type FilesPullData struct {
	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`
	Bytes      int64  `json:"bytes"`
	MIME       string `json:"mime,omitempty"`
}

// TaskCredentialsData is emitted by `vibecraft task credentials`.
type TaskCredentialsData struct {
	ID     string   `json:"id"`
	Status string   `json:"status"` // "submitted" | "dismissed"
	Names  []string `json:"names,omitempty"`
}

// RuleData is one guardrail rule.
type RuleData struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Priority    int    `json:"priority,omitempty"`
}

// RulesListData is emitted by `vibecraft rules list`.
type RulesListData struct {
	Rules []RuleData `json:"rules"`
}

// RuleDeleteData is emitted by `vibecraft rules delete`.
type RuleDeleteData struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// HostedAppData is one deployed app.
type HostedAppData struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	URL        string `json:"url"`
	SSOEnabled bool   `json:"sso_enabled"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// AppsListData is emitted by `vibecraft apps list`.
type AppsListData struct {
	Apps []HostedAppData `json:"apps"`
}

// AppActionData is emitted by `vibecraft apps sso` / `apps remove`.
type AppActionData struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// AppRestartData is emitted by `vibecraft tools restart`.
type AppRestartData struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Unit           string `json:"unit"`
	Status         string `json:"status"`
	OldPID         int    `json:"old_pid"`
	NewPID         int    `json:"new_pid"`
	Port           int    `json:"port,omitempty"`
	ListeningAfter bool   `json:"listening_after"`
	Detail         string `json:"detail,omitempty"`
}

// ToolDeployData is emitted by `vibecraft tools deploy <name>`. Proof-
// oriented like restart, plus the written unit path.
type ToolDeployData struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Unit           string `json:"unit"`
	Status         string `json:"status"`
	OldPID         int    `json:"old_pid"`
	NewPID         int    `json:"new_pid"`
	Port           int    `json:"port,omitempty"`
	ListeningAfter bool   `json:"listening_after"`
	UnitPath       string `json:"unit_path,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// ToolStatusData is emitted by `vibecraft tools status <name>` — what the
// tool is actually running right now.
type ToolStatusData struct {
	Name             string   `json:"name"`
	Unit             string   `json:"unit"`
	Port             int      `json:"port"`
	URL              string   `json:"url,omitempty"`
	SSOEnabled       bool     `json:"sso_enabled"`
	CreatedAt        string   `json:"created_at,omitempty"`
	UnitPresent      bool     `json:"unit_present"`
	ExecStart        string   `json:"exec_start,omitempty"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	Description      string   `json:"description,omitempty"`
	EnvKeys          []string `json:"env_keys"`
	ActiveState      string   `json:"active_state,omitempty"`
	SubState         string   `json:"sub_state,omitempty"`
	MainPID          int      `json:"main_pid"`
	Listening        bool     `json:"listening"`
	Since            string   `json:"since,omitempty"`
	GitCommit        string   `json:"git_commit,omitempty"`
	GitDirty         bool     `json:"git_dirty,omitempty"`
	Detail           string   `json:"detail,omitempty"`
}

// ToolLogsData is emitted by `vibecraft tools logs <name>`.
type ToolLogsData struct {
	Name   string   `json:"name"`
	Unit   string   `json:"unit"`
	Lines  []string `json:"lines"`
	Detail string   `json:"detail,omitempty"`
}

// ToolEnvKeyData is one environment variable key (never the value).
type ToolEnvKeyData struct {
	Key      string `json:"key"`
	Reserved bool   `json:"reserved"`
}

// ToolEnvData is emitted by `vibecraft tools env <name>`.
type ToolEnvData struct {
	Name   string           `json:"name"`
	Unit   string           `json:"unit"`
	Keys   []ToolEnvKeyData `json:"keys"`
	Detail string           `json:"detail,omitempty"`
}

// ComputerMDData is emitted by `vibecraft computer-md`.
type ComputerMDData struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// UpdateData is emitted by `vibecraft update`.
type UpdateData struct {
	Status         string `json:"status"` // "up_to_date" | "updated"
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	Path           string `json:"path,omitempty"`
}
