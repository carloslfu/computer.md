// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/carloslfu/computer.md/cli/schema"
)

// TestConfigLegacyMigration verifies the single-machine legacy config
// (MachineURL + APIKey at the top level) is migrated to the
// Machines[machineID] shape on load. F2.
func TestConfigLegacyMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".config", "vibecraft")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := `{"machine_url":"https://vc-legacy.vc.vibecraft.so","api_key":"vc_machine_legacy_legacy_xyz"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("nil cfg")
	}
	if cfg.ActiveMachine != "vc-legacy" {
		t.Errorf("ActiveMachine = %q, want vc-legacy", cfg.ActiveMachine)
	}
	if len(cfg.Machines) != 1 {
		t.Fatalf("Machines len = %d, want 1", len(cfg.Machines))
	}
	m, ok := cfg.Machines["vc-legacy"]
	if !ok {
		t.Fatalf("Machines[vc-legacy] missing: %+v", cfg.Machines)
	}
	if m.URL != "https://vc-legacy.vc.vibecraft.so" {
		t.Errorf("URL = %q", m.URL)
	}
	if m.APIKey != "vc_machine_legacy_legacy_xyz" {
		t.Errorf("APIKey = %q", m.APIKey)
	}

	// Legacy fields should be cleared on the migrated file on disk too.
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if generic["machine_url"] != nil && generic["machine_url"] != "" {
		t.Errorf("expected migrated file to drop legacy machine_url; got %v", generic["machine_url"])
	}
}

// TestSaveConfig_AtomicAndLocked verifies that saveConfig writes via a
// temp file + rename (G12) and creates a .lock sibling.
func TestSaveConfig_AtomicAndLocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := &Config{
		ActiveMachine: "vc-test",
		Machines: map[string]MachineConfig{
			"vc-test": {URL: "https://vc-test.vc.vibecraft.so", APIKey: "vc_machine_test_test_xyz"},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	dir := filepath.Join(home, ".config", "vibecraft")

	// .lock file should exist (left in place between writes; that's ok —
	// flock cleans up on close).
	if _, err := os.Stat(filepath.Join(dir, ".lock")); err != nil {
		t.Errorf(".lock file missing after saveConfig: %v", err)
	}
	// No stray .tmp left behind.
	if _, err := os.Stat(filepath.Join(dir, "config.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("config.json.tmp should not exist after a clean save")
	}
	// config.json mode must be 0600.
	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	if info.Mode().Perm() != 0600 {
		t.Errorf("config.json mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestValidateAPIKey covers G6 — prefix, length, charset.
func TestValidateAPIKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"good", "vc_machine_AbC0123_456_789012", true},
		{"wrong prefix", "vk_machine_AbC0123_456_789012", false},
		{"too short", "vc_machine_abc", false},
		{"too long", "vc_machine_" + repeat("a", 260), false},
		{"bad char", "vc_machine_abc def 123 456", false},
		{"bad char 2", "vc_machine_abc\ndef\nxyz_789", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAPIKey(tc.key)
			if tc.ok && err != nil {
				t.Errorf("validateAPIKey(%q) = %v, want nil", tc.key, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateAPIKey(%q) = nil, want err", tc.key)
			}
		})
	}
}

func TestResolveConfigRejectsPartialExplicitCredentials(t *testing.T) {
	oldMachineURL := flagMachineURL
	oldAPIKey := flagAPIKey
	t.Cleanup(func() {
		flagMachineURL = oldMachineURL
		flagAPIKey = oldAPIKey
	})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("VIBECRAFT_MACHINE_URL", "")
	t.Setenv("VIBECRAFT_API_KEY", "vc_machine_env_only_key_12345")
	flagMachineURL = ""
	flagAPIKey = ""

	_, _, err := resolveConfig()
	se, ok := err.(*schema.Error)
	if !ok || se.Code != schema.CodeValidationError {
		t.Fatalf("resolveConfig with api key only: got %T %v, want %s", err, err, schema.CodeValidationError)
	}

	t.Setenv("VIBECRAFT_MACHINE_URL", "https://vc-test.vc.vibecraft.so")
	t.Setenv("VIBECRAFT_API_KEY", "")
	_, _, err = resolveConfig()
	se, ok = err.(*schema.Error)
	if !ok || se.Code != schema.CodeValidationError {
		t.Fatalf("resolveConfig with machine url only: got %T %v, want %s", err, err, schema.CodeValidationError)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
