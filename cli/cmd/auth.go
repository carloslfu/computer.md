// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/client"
	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var (
	flagAuthAPIKey     string
	flagAuthFromFile   string
	flagAuthMachineURL string
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication",
	Long:  "Authenticate the CLI with your VibeCraft account.",
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate to your VibeCraft account",
	Long: `Connect the CLI to your VibeCraft account.

  # Account login (the normal path) — one browser approval:
  vibecraft auth login

After this the CLI reaches every machine on your account. The first time
a command touches a machine it brokers that machine's key automatically —
there is no per-machine login, and machines you provision later just work.

  # Headless single-machine (CI / BYOM / no browser) — paste a key:
  vibecraft auth login --api-key vc_machine_... --machine-url https://vc-abc.vc.vibecraft.so
  vibecraft auth login --from-file ./key.txt   --machine-url https://vc-abc.vc.vibecraft.so
  echo "vc_machine_..." | vibecraft auth login --api-key - --machine-url https://vc-abc...

Credentials are stored in ~/.config/vibecraft/config.json (0600).`,
	RunE: runAuthLogin,
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove stored credentials",
	Long:  "Delete the local config file and log out.",
	RunE:  runAuthLogout,
}

var authWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show the machines the CLI knows about locally",
	Long: `Print the locally-known machines and the active one. This reads the
local config; for the live account-wide fleet use 'vibecraft machine list'.`,
	RunE: runAuthWhoami,
}

var authStatusAlias = &cobra.Command{
	Use:    "status",
	Short:  "Alias for 'whoami'",
	Hidden: true,
	RunE:   runAuthWhoami,
}

var authPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print credentials for handoff to another agent",
	Long: `Emit machine_url + api_key as a JSON envelope (or shell env exports in --text mode).

This is the only verb that includes the full API key in its output. Pipe to a
secret manager / scp / 1Password — never paste into a shared terminal.

Example:
  vibecraft auth print | ssh other-box 'cat > ~/.vibecraft-creds'
  vibecraft auth print --text  # exports VIBECRAFT_MACHINE_URL and VIBECRAFT_API_KEY`,
	RunE: runAuthPrint,
}

func init() {
	rootCmd.AddCommand(authCmd)

	authLoginCmd.Flags().StringVar(&flagAuthAPIKey, "api-key", "", "API key (use '-' to read from stdin)")
	authLoginCmd.Flags().StringVar(&flagAuthFromFile, "from-file", "", "Read API key from this file")
	authLoginCmd.Flags().StringVar(&flagAuthMachineURL, "machine-url", "", "Machine URL (required with --api-key/--from-file)")

	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authLogoutCmd)
	authCmd.AddCommand(authWhoamiCmd)
	authCmd.AddCommand(authStatusAlias)
	authCmd.AddCommand(authPrintCmd)
}

func runAuthLogin(cmd *cobra.Command, args []string) error {
	// Headless paste-in: a single machine's vc_machine_* key. Kept for
	// CI / BYOM / agent-to-agent handoff where there's no browser.
	if flagAuthAPIKey != "" || flagAuthFromFile != "" {
		return runAuthLoginHeadless(cmd, args)
	}
	// Browser handshake: account login. One approval authorizes the CLI
	// for the whole account — every machine, present and future.
	return runAuthLoginBrowser(cmd, args)
}

func runAuthLoginHeadless(cmd *cobra.Command, args []string) error {
	key, err := resolveAPIKeyInput(flagAuthAPIKey, flagAuthFromFile)
	if err != nil {
		return err
	}
	if err := validateAPIKey(key); err != nil {
		return err
	}
	machineURL := flagAuthMachineURL
	if machineURL == "" {
		machineURL = os.Getenv("VIBECRAFT_MACHINE_URL")
	}
	if machineURL == "" {
		return schema.Newf(schema.CodeValidationError,
			"--machine-url is required when using --api-key or --from-file").
			WithHint("e.g. --machine-url https://vc-abc.vc.vibecraft.so")
	}
	machineURL = strings.TrimRight(machineURL, "/")

	// Validate the key by hitting /api/status before persisting.
	c := client.New(machineURL, key)
	c.UserAgent = "vibecraft-cli/" + cliVersion
	status, err := c.GetStatus()
	if err != nil {
		return mapDaemonError(err, "machine unreachable")
	}

	machineID := status.MachineID
	if machineID == "" {
		machineID = extractMachineID(machineURL)
	}

	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{Machines: map[string]MachineConfig{}}
	}
	if cfg.Machines == nil {
		cfg.Machines = map[string]MachineConfig{}
	}
	cfg.Machines[machineID] = MachineConfig{URL: machineURL, APIKey: key, Name: machineID}
	cfg.ActiveMachine = machineID
	if err := saveConfig(cfg); err != nil {
		return schema.Newf(schema.CodeInternal, "saving config: %s", err.Error())
	}

	return output.Emit(schema.AuthLoginData{
		MachineID:  machineID,
		MachineURL: machineURL,
		APIKeyHint: maskKey(key),
	})
}

// runAuthLoginBrowser runs the account-login handshake: one browser
// approval mints a `vc_account_*` key that authorizes the CLI for the
// whole account. Per-machine daemon keys are brokered on demand later
// (see resolveConfig), so a machine provisioned tomorrow is reachable
// with no further login.
func runAuthLoginBrowser(cmd *cobra.Command, args []string) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return schema.Newf(schema.CodeInternal, "starting local server: %s", err.Error())
	}
	port := listener.Addr().(*net.TCPAddr).Port

	resultCh := make(chan string, 1) // the account key
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		accountKey := r.URL.Query().Get("account_key")
		if accountKey == "" {
			http.Error(w, "Missing account_key", http.StatusBadRequest)
			errCh <- schema.Newf(schema.CodeInternal, "callback missing account_key")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!DOCTYPE html><html><body style="font-family:sans-serif;text-align:center;padding:60px"><h2>Authenticated</h2><p>You can close this tab and return to the terminal.</p></body></html>`)
		resultCh <- accountKey
	})

	server := &http.Server{Handler: mux}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- schema.Newf(schema.CodeInternal, "local server error: %s", serveErr.Error())
		}
	}()

	browserURL := fmt.Sprintf("%s/cli-auth?port=%d&mode=account", platformBase(), port)

	if output.CurrentMode() == output.ModeText {
		fmt.Fprintf(os.Stderr, "Opening browser to authenticate your VibeCraft account...\n")
		fmt.Fprintf(os.Stderr, "If the browser doesn't open, visit: %s\n\n", browserURL)
	} else {
		fmt.Fprintf(os.Stderr, "Open this URL to authenticate: %s\n", browserURL)
	}

	_ = openBrowser(browserURL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var accountKey string
	select {
	case accountKey = <-resultCh:
	case err := <-errCh:
		_ = server.Shutdown(context.Background())
		return err
	case <-ctx.Done():
		_ = server.Shutdown(context.Background())
		return schema.Newf(schema.CodeTimeout, "authentication timed out").
			WithHint("re-run 'vibecraft auth login'")
	}
	_ = server.Shutdown(context.Background())

	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.Machines == nil {
		cfg.Machines = map[string]MachineConfig{}
	}
	cfg.AccountKey = accountKey

	// Populate the machine list so `machine list` is instant and the
	// active machine is set when there's exactly one. A fresh login
	// rebuilds the machine set from scratch — cached daemon keys are
	// dropped so the next command re-brokers a clean key. That makes
	// `auth login` the recovery path if a key was revoked.
	out := schema.AuthWhoamiData{}
	if machineList, listErr := listAccountMachines(accountKey); listErr == nil {
		rebuilt := map[string]MachineConfig{}
		for _, m := range machineList {
			rebuilt[m.ID] = MachineConfig{
				URL:  "https://" + m.Host,
				Name: m.Name,
			}
		}
		cfg.Machines = rebuilt
		if _, ok := rebuilt[cfg.ActiveMachine]; !ok {
			cfg.ActiveMachine = ""
			if len(machineList) == 1 {
				cfg.ActiveMachine = machineList[0].ID
			}
		}
		out = machinesToWhoami(cfg)
	}

	if err := saveConfig(cfg); err != nil {
		return schema.Newf(schema.CodeInternal, "saving config: %s", err.Error())
	}

	return output.Emit(out)
}

func runAuthLogout(cmd *cobra.Command, args []string) error {
	if err := deleteConfig(); err != nil {
		return schema.Newf(schema.CodeInternal, "%s", err.Error())
	}
	return output.Emit(schema.AuthLogoutData{})
}

func runAuthWhoami(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "%s", err.Error())
	}
	return output.Emit(machinesToWhoami(cfg))
}

// machinesToWhoami renders the local config's machine set as
// AuthWhoamiData. Shared by `auth login`, `auth whoami`, `machine list`.
// APIKeyHint is set only for machines with a brokered daemon key cached.
func machinesToWhoami(cfg *Config) schema.AuthWhoamiData {
	data := schema.AuthWhoamiData{}
	if cfg == nil || len(cfg.Machines) == 0 {
		return data
	}
	data.ActiveMachine = cfg.ActiveMachine
	ids := make([]string, 0, len(cfg.Machines))
	for id := range cfg.Machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		mc := cfg.Machines[id]
		hint := ""
		if mc.APIKey != "" {
			hint = maskKey(mc.APIKey)
		}
		data.Machines = append(data.Machines, schema.AuthMachineData{
			ID:         id,
			URL:        mc.URL,
			Name:       mc.Name,
			APIKeyHint: hint,
			Active:     id == cfg.ActiveMachine,
		})
	}
	return data
}

func runAuthPrint(cmd *cobra.Command, args []string) error {
	if isFanoutSelector(flagMachineID) {
		return schema.Newf(schema.CodeValidationError,
			"auth print does not support fan-out").
			WithHint("auth print emits one machine's credentials; rerun without fan-out selector")
	}
	machineURL, apiKey, err := resolveConfig()
	if err != nil {
		return err
	}

	// Find the machine ID from config; fall back to URL extraction.
	machineID := extractMachineID(machineURL)
	cfg, _ := loadConfig()
	if cfg != nil {
		for id, mc := range cfg.Machines {
			if mc.URL == machineURL && mc.APIKey == apiKey {
				machineID = id
				break
			}
		}
	}

	return output.Emit(schema.AuthPrintData{
		MachineID:  machineID,
		MachineURL: machineURL,
		APIKey:     apiKey,
	})
}

// resolveAPIKeyInput reads an API key from the --api-key flag (literal or
// "-" for stdin) or --from-file. Returns the trimmed key.
func resolveAPIKeyInput(apiKeyFlag, fromFileFlag string) (string, error) {
	if apiKeyFlag != "" && fromFileFlag != "" {
		return "", schema.Newf(schema.CodeValidationError,
			"--api-key and --from-file are mutually exclusive")
	}
	if fromFileFlag != "" {
		b, err := os.ReadFile(fromFileFlag)
		if err != nil {
			return "", schema.Newf(schema.CodePathNotFound,
				"reading key file: %s", err.Error())
		}
		return strings.TrimSpace(string(b)), nil
	}
	if apiKeyFlag == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", schema.Newf(schema.CodeInternal,
				"reading API key from stdin: %s", err.Error())
		}
		return strings.TrimSpace(string(b)), nil
	}
	if apiKeyFlag != "" {
		fmt.Fprintln(os.Stderr, "warning: --api-key with a literal value is visible in shell history and process listings. Prefer VIBECRAFT_API_KEY, '--api-key -' (stdin), or '--from-file'.")
	}
	return strings.TrimSpace(apiKeyFlag), nil
}

// openBrowser opens a URL in the user's default browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return cmd.Start()
}
