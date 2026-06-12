// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/usage"
)

var (
	openAIKeyPath                  = "/etc/vibecraft/openai.key"
	managerKeyModePath             = "/etc/vibecraft/manager_key_mode"
	managerModelPath               = "/etc/vibecraft/manager_model"
	managerReasoningEffortPath     = "/etc/vibecraft/manager_reasoning_effort"
	managerMaxOutputTokensPath     = "/etc/vibecraft/manager_max_output_tokens"
	managerDeepReasoningEffortPath = "/etc/vibecraft/manager_deep_reasoning_effort"
	managerDeepMaxOutputTokensPath = "/etc/vibecraft/manager_deep_max_output_tokens"
	managerConfigDirPath           = "/etc/vibecraft"
	defaultMachineNamePath         = "/etc/vibecraft/machine.name"
)

// Config holds all daemon configuration derived from files on disk
// and environment variables.
type Config struct {
	// Listening port for the HTTP server.
	Port int

	// DaemonToken is the management auth token read from /etc/vibecraft/daemon.token.
	DaemonToken string

	// OpenAIKey is the API key for the manager model. Managed machines read
	// VibeCraft's platform-owned key from /etc/vibecraft/openai.key; BYOM and
	// self-host machines read an operator-owned key from the same root-owned
	// path. The key is never passed to shells, workers, apps, logs, or the UI.
	OpenAIKey string

	// ManagerKeyMode declares who pays for manager calls:
	// platform = VibeCraft-owned managed key and VibeCraft credits apply.
	// operator = BYOM/self-host operator-owned key and VibeCraft credits do not.
	// relay = no local hosted key; reserved for a future server-side relay.
	// Until that transport exists, relay mode fails closed and ignores any
	// local key so it cannot silently become unmetered direct OpenAI usage.
	ManagerKeyMode string

	// ManagerModel is the OpenAI model id used by the manager. Hosted launch
	// keeps the default; OSS/self-host can override with VIBECRAFT_MANAGER_MODEL.
	ManagerModel string

	// ManagerReasoningEffort is sent to the Responses API for the main
	// manager loop. Helper calls keep their own smaller output budgets.
	ManagerReasoningEffort string

	// ManagerMaxOutputTokens caps generated tokens for the normal manager
	// loop, including reasoning and visible output tokens. Deep-planning
	// turns use the separate deep cap below.
	ManagerMaxOutputTokens int

	// ManagerDeepReasoningEffort and ManagerDeepMaxOutputTokens are used
	// when the user asks for deep planning, system design, production
	// verification, or multi-worker synthesis.
	ManagerDeepReasoningEffort string
	ManagerDeepMaxOutputTokens int

	// ManagerUnavailableReason is set when the daemon can serve health/UI
	// but cannot run manager tasks because local manager auth is incomplete.
	// This keeps legacy BYOM auto-updates from turning into Caddy 502s while
	// still refusing work until an operator-owned OpenAI key is configured.
	ManagerUnavailableReason string

	// JWTPublicKey is the RS256 public key used to verify platform-issued JWTs.
	// Read from /etc/vibecraft/jwt_public.pem.
	JWTPublicKey *rsa.PublicKey

	// MachineID identifies this customer machine. Read from /etc/vibecraft/machine.id.
	MachineID string

	// MachineName is the customer-facing display name mirrored from the platform.
	// Read from /etc/vibecraft/machine.name when present.
	MachineName string

	// MachineNamePath is where the local machine-name mirror lives. Tests
	// override this so handlers never write to /etc.
	MachineNamePath string

	// MachineHost is the public hostname (e.g. "vc-abc123.vc.vibecraft.so").
	// Read from /etc/vibecraft/machine.host.
	MachineHost string

	// DataDir is the root data directory (/var/lib/vibecraft).
	DataDir string

	// DBPath is the full path to the SQLite database.
	DBPath string

	// VaultPath is the path to the encrypted vault file.
	VaultPath string

	// ScreenshotDir is the directory for screenshot images.
	ScreenshotDir string

	// InboxDir is where user-uploaded attachments land, grouped by
	// conversation id. The agent reads these paths directly from the
	// filesystem — so the default lives under /home/vibecraft to give
	// the agent a natural home-relative path to reference.
	InboxDir string

	// DBEncryptionKey is loaded from /etc/vibecraft/encryption.key for SQLCipher.
	DBEncryptionKey string

	// VaultEncryptionKey is the 32-byte key for AES-256-GCM vault encryption.
	// Loaded from /etc/vibecraft/encryption.key.
	VaultEncryptionKey [32]byte

	// HealthToken is the token used for health check authentication.
	// Written by cloud-init to /etc/vibecraft/health.token.
	HealthToken string

	// LocalToken gates the localhost-only endpoints (/api/routes,
	// /api/daemon/task, /api/notify). Written by cloud-init / install.sh
	// to /etc/vibecraft/local.token (0600 root) so the vibecraft user
	// cannot read it directly; the daemon reads it and injects it only
	// into shells it spawns (Phase 0b). Empty = pre-migration machine:
	// legacy behavior (loopback-only, no token) + a startup warning,
	// until the platform health-cron pushes a token.
	LocalToken string

	// PlatformBaseURL is the base URL of the VibeCraft platform. Used
	// for outbound calls (notifications ingest, future relay channels).
	// Defaults to https://www.vibecraft.so; overridable via env var for
	// dev / staging.
	PlatformBaseURL string

	// AuditSink turns on the write-only remote audit stream (Phase 6).
	// Managed machines enable it (cloud-init writes /etc/vibecraft/
	// audit_sink = "on"); BYOM is opt-in (install.sh writes "off"; the
	// customer flips it). Absent file = off (safe default). Env
	// VIBECRAFT_AUDIT_SINK=on|off overrides.
	AuditSink bool
}

func LoadConfig() (*Config, error) {
	cfg := &Config{
		Port:            8420,
		DataDir:         "/var/lib/vibecraft",
		DBPath:          "/var/lib/vibecraft/vibecraft.db",
		VaultPath:       "/var/lib/vibecraft/vault.enc",
		ScreenshotDir:   "/var/lib/vibecraft/screenshots",
		InboxDir:        "/home/vibecraft/inbox",
		MachineNamePath: defaultMachineNamePath,
	}

	// Generate daemon token on first boot if it doesn't exist.
	if err := ensureRandomHexFile("/etc/vibecraft/daemon.token", 32); err != nil {
		return nil, fmt.Errorf("ensuring daemon token: %w", err)
	}

	// Generate encryption key on first boot if it doesn't exist.
	if err := ensureRandomBytesFile("/etc/vibecraft/encryption.key", 32); err != nil {
		return nil, fmt.Errorf("ensuring encryption key: %w", err)
	}

	var err error

	cfg.DaemonToken, err = readFileString("/etc/vibecraft/daemon.token")
	if err != nil {
		return nil, fmt.Errorf("reading daemon token: %w", err)
	}

	cfg.OpenAIKey, cfg.ManagerKeyMode, cfg.ManagerModel, cfg.ManagerUnavailableReason, err = loadManagerSettings(readFileString, os.Getenv)
	if err != nil {
		return nil, err
	}
	cfg.ManagerReasoningEffort, cfg.ManagerMaxOutputTokens, cfg.ManagerDeepReasoningEffort, cfg.ManagerDeepMaxOutputTokens, err = loadManagerRuntimeSettings(readFileString, os.Getenv)
	if err != nil {
		return nil, err
	}
	if cfg.ManagerUnavailableReason != "" {
		log.Printf("manager unavailable: %s", cfg.ManagerUnavailableReason)
	}

	jwtPEM, err := readFileString("/etc/vibecraft/jwt_public.pem")
	if err == nil {
		pubKey, parseErr := parseRSAPublicKey(jwtPEM)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing JWT public key: %w", parseErr)
		}
		cfg.JWTPublicKey = pubKey
	}

	cfg.MachineID, err = readFileString("/etc/vibecraft/machine.id")
	if err != nil {
		return nil, fmt.Errorf("reading machine ID: %w (this file is required for JWT auth)", err)
	}
	cfg.MachineName, err = readOptionalFileString(cfg.MachineNamePath)
	if err != nil {
		return nil, fmt.Errorf("reading machine name: %w", err)
	}

	cfg.MachineHost, err = readFileString("/etc/vibecraft/machine.host")
	if err != nil {
		// Derive from machine ID if file doesn't exist (backwards compatibility).
		// The default suffix matches VibeCraft's hosted convention; forks /
		// pure self-host operators can override via VIBECRAFT_MACHINE_HOST_SUFFIX.
		suffix := os.Getenv("VIBECRAFT_MACHINE_HOST_SUFFIX")
		if suffix == "" {
			suffix = ".vc.vibecraft.so"
		}
		cfg.MachineHost = cfg.MachineID + suffix
		log.Printf("machine.host not found, derived: %s", cfg.MachineHost)
	}

	// Load health token (written by cloud-init, not generated by daemon).
	cfg.HealthToken, err = readFileString("/etc/vibecraft/health.token")
	if err != nil {
		return nil, fmt.Errorf("reading health token: %w", err)
	}

	// Local token gates the loopback data-plane endpoints (Phase 0b).
	// Source of truth by provider: managed machines get it from
	// cloud-init write_files at provision; BYOM from install.sh. The
	// generate-if-absent below is the safety net that closes the pre-0b
	// BYOM updater gap (a machine whose updater script predates the
	// install.sh local-token block updates its binary but never had a
	// token written). A crypto/rand 32-byte token is unpredictable, so
	// D2's "no predictable daemon-minted token" rationale holds; D2's
	// platform-generation requirement was specifically about the managed
	// migration *push* channel, now retired — the token is a boot
	// invariant, not a thing the platform pushes after the fact. Net:
	// every machine has a token after one daemon start; there is no
	// legacy no-token mode.
	if err := ensureRandomHexFile("/etc/vibecraft/local.token", 32); err != nil {
		return nil, fmt.Errorf("ensuring local token: %w", err)
	}
	cfg.LocalToken, err = readFileString("/etc/vibecraft/local.token")
	if err != nil {
		return nil, fmt.Errorf("reading local token: %w", err)
	}
	if cfg.LocalToken == "" {
		return nil, fmt.Errorf("local token is empty after ensure")
	}

	// Storage encryption key: loaded from the dedicated encryption key
	// file. Existing machines have both the SQLCipher DB and the vault
	// encrypted directly from this key material, so this derivation is an
	// on-disk compatibility contract. Do not rotate or domain-separate it
	// in place without a real migration that can open old DB/vault files,
	// rewrite them, and roll back safely.
	encKeyBytes, err := os.ReadFile("/etc/vibecraft/encryption.key")
	if err != nil {
		return nil, fmt.Errorf("reading encryption key: %w", err)
	}
	if len(encKeyBytes) < 32 {
		return nil, fmt.Errorf("encryption key too short (need >= 32 bytes, got %d)", len(encKeyBytes))
	}
	cfg.DBEncryptionKey = hex.EncodeToString(encKeyBytes)
	copy(cfg.VaultEncryptionKey[:], encKeyBytes[:32])

	// Override port from environment if set.
	if portStr := os.Getenv("VIBECRAFT_PORT"); portStr != "" {
		fmt.Sscanf(portStr, "%d", &cfg.Port)
	}
	if dir := os.Getenv("VIBECRAFT_INBOX_DIR"); dir != "" {
		cfg.InboxDir = dir
	}
	cfg.PlatformBaseURL = os.Getenv("VIBECRAFT_PLATFORM_URL")
	if cfg.PlatformBaseURL == "" {
		cfg.PlatformBaseURL = "https://www.vibecraft.so"
	}

	// Remote audit sink (Phase 6). File is the provider-set default;
	// env overrides for dev/ops.
	if v, ferr := readFileString("/etc/vibecraft/audit_sink"); ferr == nil && v == "on" {
		cfg.AuditSink = true
	}
	switch os.Getenv("VIBECRAFT_AUDIT_SINK") {
	case "on":
		cfg.AuditSink = true
	case "off":
		cfg.AuditSink = false
	}

	// Ensure directories exist.
	for _, dir := range []string{cfg.DataDir, cfg.ScreenshotDir, cfg.InboxDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	// The inbox holds files the agent must read from its bash tool, which
	// runs as the vibecraft user. The daemon runs as root and MkdirAll
	// created InboxDir with mode 0700 owned by root — an agent running as
	// vibecraft can't even traverse in. Chown the root of the tree to the
	// agent user so the per-file chown the upload handler does has an
	// effect. No-op when the user doesn't exist (local dev / tests).
	chownToAgent(cfg.InboxDir)

	return cfg, nil
}

func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readOptionalFileString(path string) (string, error) {
	value, err := readFileString(path)
	if err == nil {
		return value, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return "", err
}

func loadManagerSettings(
	readFile func(string) (string, error),
	getenv func(string) string,
) (key, mode, model, unavailable string, err error) {
	key = strings.TrimSpace(getenv("OPENAI_API_KEY"))
	if key == "" {
		v, ferr := readFile(openAIKeyPath)
		if ferr == nil {
			key = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", "", "", "", fmt.Errorf("reading OpenAI manager key: %w", ferr)
		}
	}

	mode = strings.TrimSpace(getenv("VIBECRAFT_MANAGER_KEY_MODE"))
	if mode == "" {
		v, ferr := readFile(managerKeyModePath)
		if ferr == nil {
			mode = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", "", "", "", fmt.Errorf("reading manager key mode: %w", ferr)
		}
	}
	if mode == "" {
		if key != "" {
			mode = "platform"
			log.Printf("manager key mode missing; defaulting to platform for managed compatibility")
		} else {
			mode = "operator"
			log.Printf("manager key mode and OpenAI key missing; defaulting to operator-degraded mode")
		}
	}
	switch mode {
	case "platform", "operator", "relay":
	default:
		return "", "", "", "", fmt.Errorf("invalid manager key mode %q (want platform, operator, or relay)", mode)
	}

	if mode == "operator" && key == "" {
		unavailable = "This connected computer needs your OpenAI API key before the manager can run."
	}
	if mode == "platform" && key == "" {
		unavailable = "This managed computer needs manager setup before it can run tasks. No local key is needed."
	}
	if mode == "relay" {
		key = ""
		unavailable = "This computer is configured for VibeCraft manager relay, but relay transport is not available in this daemon build yet. Switch to an operator-owned key or update after relay support ships."
	}

	model = strings.TrimSpace(getenv("VIBECRAFT_MANAGER_MODEL"))
	if model == "" {
		v, ferr := readFile(managerModelPath)
		if ferr == nil {
			model = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", "", "", "", fmt.Errorf("reading manager model: %w", ferr)
		}
	}
	if model == "" {
		model = "gpt-5.4-mini"
	}
	if mode == "platform" && !usage.IsKnownModel(model) {
		return "", "", "", "", fmt.Errorf("manager model %q has no VibeCraft usage pricing entry", model)
	}

	return key, mode, model, unavailable, nil
}

func loadManagerRuntimeSettings(
	readFile func(string) (string, error),
	getenv func(string) string,
) (reasoningEffort string, maxOutputTokens int, deepReasoningEffort string, deepMaxOutputTokens int, err error) {
	reasoningEffort = strings.TrimSpace(getenv("VIBECRAFT_MANAGER_REASONING_EFFORT"))
	if reasoningEffort == "" {
		v, ferr := readFile(managerReasoningEffortPath)
		if ferr == nil {
			reasoningEffort = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", 0, "", 0, fmt.Errorf("reading manager reasoning effort: %w", ferr)
		}
	}
	if reasoningEffort == "" {
		reasoningEffort = managerclient.DefaultReasoningEffort
	}
	if !validManagerReasoningEffort(reasoningEffort) {
		return "", 0, "", 0, fmt.Errorf("invalid manager reasoning effort %q (want none, low, medium, high, or xhigh)", reasoningEffort)
	}

	maxOutputTokens = managerclient.DefaultComputerMaxOutputTokens
	maxOutputRaw := strings.TrimSpace(getenv("VIBECRAFT_MANAGER_MAX_OUTPUT_TOKENS"))
	if maxOutputRaw == "" {
		v, ferr := readFile(managerMaxOutputTokensPath)
		if ferr == nil {
			maxOutputRaw = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", 0, "", 0, fmt.Errorf("reading manager max output tokens: %w", ferr)
		}
	}
	if maxOutputRaw != "" {
		n, perr := strconv.Atoi(maxOutputRaw)
		if perr != nil {
			return "", 0, "", 0, fmt.Errorf("invalid manager max output tokens %q: %w", maxOutputRaw, perr)
		}
		if n < managerclient.MinComputerMaxOutputTokens || n > managerclient.MaxComputerMaxOutputTokens {
			return "", 0, "", 0, fmt.Errorf("invalid manager max output tokens %d (want %d-%d)", n, managerclient.MinComputerMaxOutputTokens, managerclient.MaxComputerMaxOutputTokens)
		}
		maxOutputTokens = n
	}

	deepReasoningEffort = strings.TrimSpace(getenv("VIBECRAFT_MANAGER_DEEP_REASONING_EFFORT"))
	if deepReasoningEffort == "" {
		v, ferr := readFile(managerDeepReasoningEffortPath)
		if ferr == nil {
			deepReasoningEffort = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", 0, "", 0, fmt.Errorf("reading manager deep reasoning effort: %w", ferr)
		}
	}
	if deepReasoningEffort == "" {
		deepReasoningEffort = managerclient.DefaultDeepReasoningEffort
	}
	if !validManagerReasoningEffort(deepReasoningEffort) {
		return "", 0, "", 0, fmt.Errorf("invalid manager deep reasoning effort %q (want none, low, medium, high, or xhigh)", deepReasoningEffort)
	}

	deepMaxOutputTokens = managerclient.DefaultDeepMaxOutputTokens
	deepMaxOutputRaw := strings.TrimSpace(getenv("VIBECRAFT_MANAGER_DEEP_MAX_OUTPUT_TOKENS"))
	if deepMaxOutputRaw == "" {
		v, ferr := readFile(managerDeepMaxOutputTokensPath)
		if ferr == nil {
			deepMaxOutputRaw = strings.TrimSpace(v)
		} else if !errors.Is(ferr, os.ErrNotExist) {
			return "", 0, "", 0, fmt.Errorf("reading manager deep max output tokens: %w", ferr)
		}
	}
	if deepMaxOutputRaw != "" {
		n, perr := strconv.Atoi(deepMaxOutputRaw)
		if perr != nil {
			return "", 0, "", 0, fmt.Errorf("invalid manager deep max output tokens %q: %w", deepMaxOutputRaw, perr)
		}
		if n < managerclient.MinComputerMaxOutputTokens || n > managerclient.MaxComputerMaxOutputTokens {
			return "", 0, "", 0, fmt.Errorf("invalid manager deep max output tokens %d (want %d-%d)", n, managerclient.MinComputerMaxOutputTokens, managerclient.MaxComputerMaxOutputTokens)
		}
		deepMaxOutputTokens = n
	}

	return reasoningEffort, maxOutputTokens, deepReasoningEffort, deepMaxOutputTokens, nil
}

func validManagerReasoningEffort(v string) bool {
	switch v {
	case "none", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

// ensureRandomHexFile generates a file with n random bytes hex-encoded
// (2*n hex chars) if the file does not already exist. Written with mode 0600.
func ensureRandomHexFile(path string, n int) error {
	if _, err := os.Stat(path); err == nil {
		return nil // file already exists
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generating random bytes: %w", err)
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(b)), 0600)
}

// ensureRandomBytesFile generates a file with n raw random bytes
// if the file does not already exist. Written with mode 0600.
func ensureRandomBytesFile(path string, n int) error {
	if _, err := os.Stat(path); err == nil {
		return nil // file already exists
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generating random bytes: %w", err)
	}
	return os.WriteFile(path, b, 0600)
}

func parseRSAPublicKey(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is not RSA")
	}
	return rsaPub, nil
}
