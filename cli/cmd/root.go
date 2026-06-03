// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
	"github.com/spf13/cobra"
)

const (
	configDir  = ".config/vibecraft"
	configFile = "config.json"
)

var (
	flagAPIKey     string
	flagMachineURL string
	flagMachineID  string
	flagJSON       bool
	flagText       bool
	cliVersion     = "dev"
)

// MachineConfig stores credentials for a single machine.
//
// APIKey (`vc_machine_*`) is the daemon-side credential the CLI uses to
// talk directly to the box. Under the account-login model it is no
// longer entered by hand — it's brokered on demand (the platform
// exchanges the account key for a daemon key the first time a machine
// is touched) and cached here. PlatformKey (`vk_*`) is legacy: the old
// per-machine login minted one for notifications; account login
// replaces it with Config.AccountKey.
type MachineConfig struct {
	URL         string `json:"url"`
	APIKey      string `json:"api_key,omitempty"`
	PlatformKey string `json:"platform_key,omitempty"`
	Name        string `json:"name,omitempty"`
}

// Config is the structure stored in ~/.config/vibecraft/config.json.
type Config struct {
	// AccountKey (`vc_account_*`) is the credential from a single
	// `vibecraft auth login`. It authenticates the CLI to the whole
	// VibeCraft account: list machines, broker per-machine daemon keys,
	// read notifications. Empty for configs created by the legacy
	// per-machine login or the headless --api-key paste-in path.
	AccountKey string `json:"account_key,omitempty"`

	ActiveMachine string                   `json:"active_machine,omitempty"`
	Machines      map[string]MachineConfig `json:"machines,omitempty"`

	// Legacy fields for backward compat detection (auto-migrated on load)
	MachineURL string `json:"machine_url,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
}

var rootCmd = &cobra.Command{
	Use:   "vibecraft",
	Short: "VibeCraft CLI",
	Long: `Command-line interface for your VibeCraft agentic computer.

Optimized for agents. JSON output by default; pass --text for humans.
Run 'vibecraft docs' for the full agent-facing reference.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flagJSON && flagText {
			return schema.Newf(schema.CodeValidationError, "--json and --text are mutually exclusive")
		}
		if flagText {
			output.SetMode(output.ModeText)
		} else {
			output.SetMode(output.ModeJSON)
		}
		// G5: best-effort permission check at every command entry.
		warnIfConfigWorldReadable()
		return nil
	},
	// PersistentPostRun runs the background self-update AFTER the command,
	// so it never delays the command or races its signal handling. cobra
	// skips PostRun when RunE returned an error — fine: a failed command
	// just defers the check to the next successful one.
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		maybeAutoUpdate(cmd.Name())
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flagAPIKey, "api-key", "", "API key (overrides stored config). Prefer VIBECRAFT_API_KEY; or '-' to read from stdin, '@file' to read from a file. A literal value is visible via 'ps'.")
	rootCmd.PersistentFlags().StringVar(&flagMachineURL, "machine-url", "", "Machine URL (overrides stored config)")
	rootCmd.PersistentFlags().StringVar(&flagMachineID, "machine", "", "Machine ID to use (overrides active machine)")
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "JSON output (default; explicit form accepted)")
	rootCmd.PersistentFlags().BoolVar(&flagText, "text", false, "Human-readable text output instead of JSON")

	// A bad flag is a caller error, not a CLI bug — map cobra's
	// flag-parse failures to validation_error so agents branch on the
	// right code. Without this, "unknown flag: --x" surfaces as
	// internal_error ("report a bug"), which mis-routes error handling.
	// Inherited by every subcommand.
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return schema.Newf(schema.CodeValidationError, "%s", err.Error())
	})

	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return output.Emit(schema.VersionData{
				Version: cliVersion,
				Commit:  cliCommit,
				BuiltAt: cliBuiltAt,
			})
		},
	})
}

// cliCommit and cliBuiltAt are optional build-time variables set via
// -ldflags. Empty when developing or building without ldflags.
var (
	cliCommit  = ""
	cliBuiltAt = ""
)

// SetBuildInfo lets main inject commit/built-at metadata.
func SetBuildInfo(commit, builtAt string) {
	cliCommit = commit
	cliBuiltAt = builtAt
}

// SetVersion is called from main to inject the build-time version.
func SetVersion(v string) {
	cliVersion = v
	rootCmd.Version = v
}

// Execute runs the root command, routing any error through the output
// writer (JSON envelope to stderr in JSON mode; "Error: ..." in text mode).
// main() is responsible for mapping the returned error to a process exit
// code via exit.FromError.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		output.EmitError(err)
		return err
	}
	return nil
}

// configPath returns the full path to the config file.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(home, configDir, configFile), nil
}

// loadConfig reads the stored config file and migrates legacy format.
func loadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Migrate legacy single-machine format
	if cfg.MachineURL != "" && cfg.APIKey != "" && cfg.Machines == nil {
		machineID := extractMachineID(cfg.MachineURL)
		cfg.Machines = map[string]MachineConfig{
			machineID: {URL: cfg.MachineURL, APIKey: cfg.APIKey, Name: "My Machine"},
		}
		cfg.ActiveMachine = machineID
		cfg.MachineURL = ""
		cfg.APIKey = ""
		_ = saveConfig(&cfg)
	}

	return &cfg, nil
}

// extractMachineID extracts "vc-abc123" from "https://vc-abc123.vc.vibecraft.so".
func extractMachineID(machineURL string) string {
	// Remove scheme
	host := machineURL
	for _, prefix := range []string{"https://", "http://"} {
		host = strings.TrimPrefix(host, prefix)
	}
	// Extract subdomain before first "."
	if idx := strings.Index(host, "."); idx > 0 {
		return host[:idx]
	}
	return "default"
}

// saveConfig writes the config atomically (G12). The write is:
//  1. flock on a sibling .lock file so concurrent `auth login` processes
//     serialize cleanly. We take an exclusive lock with a 5-second deadline.
//  2. write the new JSON to a `<configFile>.tmp` next to the destination
//     (same filesystem, so the rename below is atomic).
//  3. rename(tmp, configFile) — atomic on POSIX even on partial-write crashes.
//
// A reader that hits the file mid-write either sees the previous version
// (if rename hasn't fired yet) or the new version (if it has); never
// a half-written JSON.
func saveConfig(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	// flock(2) to serialize concurrent writers.
	lockPath := filepath.Join(dir, ".lock")
	unlock, err := acquireConfigLock(lockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("acquiring config lock: %w", err)
	}
	defer unlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("writing config tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming config tmp: %w", err)
	}
	return nil
}

// warnIfConfigWorldReadable (G5) prints a single line on stderr if the
// resolved config file is readable by group or other. It is a warning,
// not a fatal — the file is owner-only by default and only humans who
// chmod'd it loosely will see this.
func warnIfConfigWorldReadable() {
	path, err := configPath()
	if err != nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by other users (mode %v). Run: chmod 600 %s\n",
			path, info.Mode().Perm(), path)
	}
}

// deleteConfig removes the config file.
func deleteConfig() error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing config: %w", err)
	}

	return nil
}

// resolveConfig returns the machine URL + daemon key to talk to. The
// resolution order:
//
//  1. Explicit flags / env vars (--machine-url + --api-key, or
//     VIBECRAFT_MACHINE_URL + VIBECRAFT_API_KEY) — the headless path.
//  2. A cached per-machine daemon key in the local config.
//  3. Lazy brokering: if there's an account key but no cached daemon
//     key for the target machine, exchange the account key for one
//     (via the platform), cache it, and use it. This is what makes a
//     single `auth login` reach every machine — no per-machine login.
func resolveConfig() (machineURL string, apiKey string, err error) {
	// 1. Flags + env vars take priority — the fully-headless path.
	machineURLProvided := flagMachineURL != "" || os.Getenv("VIBECRAFT_MACHINE_URL") != ""
	apiKeyProvided := flagAPIKey != "" || os.Getenv("VIBECRAFT_API_KEY") != ""

	machineURL = flagMachineURL
	apiKey, err = resolveAPIKeyFlag(flagAPIKey)
	if err != nil {
		return "", "", err
	}
	if machineURL == "" {
		machineURL = os.Getenv("VIBECRAFT_MACHINE_URL")
	}
	if apiKey == "" {
		apiKey = os.Getenv("VIBECRAFT_API_KEY")
	}
	if machineURL != "" && apiKey != "" {
		return machineURL, apiKey, nil
	}
	if machineURLProvided || apiKeyProvided {
		return "", "", schema.Newf(schema.CodeValidationError,
			"partial explicit credentials: machine URL and API key must be provided together").
			WithHint("set both VIBECRAFT_MACHINE_URL and VIBECRAFT_API_KEY, or pass both --machine-url and --api-key")
	}

	cfg, loadErr := loadConfig()
	if loadErr != nil {
		return "", "", schema.Newf(schema.CodeInternal, "%s", loadErr.Error())
	}

	// Pick the target machine.
	targetID := flagMachineID
	if isFanoutSelector(targetID) {
		// Fan-out is resolved by the caller, not here.
		return "", "", schema.Newf(schema.CodeValidationError,
			"this command does not support a multi-machine --machine selector")
	}
	if targetID == "" && cfg != nil {
		targetID = cfg.ActiveMachine
	}
	if targetID == "" && cfg != nil && len(cfg.Machines) == 1 {
		for id := range cfg.Machines {
			targetID = id
		}
	}

	if targetID == "" {
		// Authenticated (env-var or config account key) but no machine
		// picked — a caller error, not an auth failure.
		if accountKey, keyErr := loadAccountKey(); keyErr == nil && accountKey != "" {
			return "", "", schema.Newf(schema.CodeValidationError,
				"no machine specified").
				WithHint("pass --machine <id>, or run 'vibecraft machine list' to see your fleet")
		}
		return "", "", schema.Newf(schema.CodeAuthRequired,
			"not authenticated").WithHint("run 'vibecraft auth login'")
	}

	// Cached daemon key, or lazily broker one via the account key.
	return daemonCredsFor(cfg, targetID)
}

// resolveAPIKeyFlag normalizes the value of the global --api-key flag.
//
// Passing a secret as a literal flag value leaks it into argv, where any
// other user on the box can read it via `ps`/`/proc/<pid>/cmdline` and
// where it lands in shell history. The preferred channel is the
// VIBECRAFT_API_KEY env var (resolved by the caller when this returns
// ""). For the cases where a flag is still convenient, two indirections
// keep the secret off argv:
//
//	--api-key -        read the key from stdin (trailing newline trimmed)
//	--api-key @path    read the key from the file at path
//
// A literal value still works for backward compatibility, but emits a
// one-line stderr warning so the operator knows it was exposed.
func resolveAPIKeyFlag(raw string) (string, error) {
	switch {
	case raw == "":
		return "", nil
	case raw == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", schema.Newf(schema.CodeInternal, "reading --api-key from stdin: %s", err.Error())
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case strings.HasPrefix(raw, "@"):
		path := raw[1:]
		if path == "" {
			return "", schema.Newf(schema.CodeValidationError,
				"--api-key @ requires a file path (e.g. --api-key @/run/secrets/key)")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", schema.Newf(schema.CodeValidationError,
				"reading --api-key from file: %s", err.Error())
		}
		return strings.TrimSpace(string(b)), nil
	default:
		// Literal key on argv. Honor it, but warn once: it is visible to
		// every process on the machine via `ps`.
		fmt.Fprintln(os.Stderr,
			"warning: --api-key with a literal value is visible to other users via 'ps'. "+
				"Prefer VIBECRAFT_API_KEY, '--api-key -' (stdin), or '--api-key @file'.")
		return raw, nil
	}
}

// daemonCredsFor returns (machineURL, daemonKey) for one machine. It
// returns a cached per-machine daemon key when present; otherwise it
// exchanges the account key for one via the platform broker and caches
// the result. The caller's cfg is mutated + saved on a fresh broker.
func daemonCredsFor(cfg *Config, machineID string) (string, string, error) {
	if cfg != nil {
		if mc, ok := cfg.Machines[machineID]; ok && mc.URL != "" && mc.APIKey != "" {
			return mc.URL, mc.APIKey, nil
		}
	}
	// Resolve the account key via loadAccountKey() — VIBECRAFT_ACCOUNT_KEY
	// first, then the stored config. Reading cfg.AccountKey directly (as
	// this used to) left every daemon verb broken under headless / CI
	// auth, where the key is in the env var and there is no config file.
	accountKey, err := loadAccountKey()
	if err != nil {
		return "", "", err
	}
	u, k, err := brokerDaemonKey(accountKey, machineID)
	if err != nil {
		return "", "", err
	}
	// Cache the brokered key for later calls — only when there is a config
	// file to cache into. Env-var auth has none, so it re-brokers.
	if cfg != nil {
		if cfg.Machines == nil {
			cfg.Machines = map[string]MachineConfig{}
		}
		existing := cfg.Machines[machineID]
		existing.URL = u
		existing.APIKey = k
		if existing.Name == "" {
			existing.Name = machineID
		}
		cfg.Machines[machineID] = existing
		_ = saveConfig(cfg)
	}
	return u, k, nil
}

// validateAPIKey checks that the key has the expected format.
// Tightened (G6): prefix + length bounds + charset restriction so a
// pasted key that looks roughly right but contains a stray newline or
// shell-escape character is caught here, not at the daemon.
func validateAPIKey(key string) error {
	if !strings.HasPrefix(key, "vc_machine_") {
		return schema.Newf(schema.CodeAuthInvalid,
			"invalid API key format").WithHint("keys start with 'vc_machine_'")
	}
	if len(key) < 20 {
		return schema.Newf(schema.CodeAuthInvalid, "API key is too short")
	}
	if len(key) > 256 {
		return schema.Newf(schema.CodeAuthInvalid, "API key is unreasonably long")
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		isAllowed := isAlnum || c == '_' || c == '-'
		if !isAllowed {
			return schema.Newf(schema.CodeAuthInvalid,
				"API key contains invalid character").
				WithHint("keys are [A-Za-z0-9_-]+")
		}
	}
	return nil
}

// newClient creates an API client using the resolved config.
func newClient() (*client.Client, error) {
	machineURL, apiKey, err := resolveConfig()
	if err != nil {
		return nil, err
	}

	if err := validateAPIKey(apiKey); err != nil {
		return nil, err
	}

	// Refuse plain HTTP for non-loopback hosts (G1). Loopback (127.0.0.1 /
	// localhost / ::1) is always allowed; any other host over http:// is
	// refused unless the operator sets VIBECRAFT_ALLOW_HTTP=1 — the
	// documented escape hatch the error hint points at.
	if strings.HasPrefix(machineURL, "http://") {
		host := strings.TrimPrefix(machineURL, "http://")
		if idx := strings.IndexAny(host, ":/"); idx >= 0 {
			host = host[:idx]
		}
		isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
		if !isLoopback && os.Getenv("VIBECRAFT_ALLOW_HTTP") != "1" {
			return nil, schema.Newf(schema.CodeValidationError,
				"refusing plain HTTP to non-loopback host %q", host).
				WithHint("use https:// (or override at your own risk via VIBECRAFT_ALLOW_HTTP=1)")
		}
	}

	c := client.New(machineURL, apiKey)
	c.UserAgent = "vibecraft-cli/" + cliVersion

	// A15+G11: probe /api/version once per process. Refuse to operate
	// against a daemon that doesn't list schema.Version in its
	// schema_versions array. Older daemons return 404 on /api/version
	// (no version handler) — we treat that as "implicit schema 1" so
	// the CLI works against the install base from before this change.
	if err := versionHandshake(c); err != nil {
		return nil, err
	}
	return c, nil
}

// versionHandshake calls /api/version on the first newClient() call per
// process and caches the result. A daemon that doesn't list our
// schema.Version returns a "schema_unsupported" error and the caller
// refuses to operate (G11). A 404 from /api/version is treated as
// pre-handshake daemon and accepted (graceful rollback for the install
// base that pre-dates the version endpoint).
var (
	versionHandshakeMu   sync.Mutex
	versionHandshakeDone bool
	versionHandshakeErr  error
)

func versionHandshake(c *client.Client) error {
	versionHandshakeMu.Lock()
	defer versionHandshakeMu.Unlock()
	if versionHandshakeDone {
		return versionHandshakeErr
	}
	versionHandshakeDone = true
	versionHandshakeErr = checkClientVersion(c)
	return versionHandshakeErr
}

func checkClientVersion(c *client.Client) error {
	v, err := c.GetVersion()
	if err != nil {
		// Pre-handshake daemon returns 404; treat that as "speaks v1
		// implicitly". Any other failure (network, 5xx) we DO surface so
		// the user sees the problem here rather than on every verb.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		// Network errors at this point mean the machine itself is
		// unreachable — surface cleanly.
		return mapDaemonError(err, "version probe")
	}
	for _, sv := range v.SchemaVersions {
		if sv == schema.Version {
			return nil
		}
	}
	return schema.Newf(
		schema.CodeSchemaUnsupported,
		"daemon speaks schema_versions=%v but CLI requires %d", v.SchemaVersions, schema.Version,
	).WithHint("run 'vibecraft update' to get a compatible CLI")
}

// maskKey returns a masked version of the API key for display.
func maskKey(key string) string {
	if len(key) <= 12 {
		return key[:4] + "..."
	}
	return key[:12] + "..." + key[len(key)-4:]
}
