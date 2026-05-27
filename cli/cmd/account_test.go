// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/cli/schema"
)

// TestDaemonCredsFor_BrokersAndCaches verifies the account-login core:
// when a machine has no cached daemon key, daemonCredsFor exchanges the
// account key at the platform broker, returns the daemon creds, and
// caches them so a second call needs no round-trip.
func TestDaemonCredsFor_BrokersAndCaches(t *testing.T) {
	var brokerHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/vc-test/cli-key", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer vc_account_testkey" {
			http.Error(w, `{"error":"bad account key"}`, http.StatusUnauthorized)
			return
		}
		brokerHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"machine_id":  "vc-test",
			"machine_url": "https://vc-test.vc.vibecraft.so",
			"api_key":     "vc_machine_brokered_test_key_xyz",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("VIBECRAFT_PLATFORM_URL", srv.URL)
	t.Setenv("HOME", t.TempDir())
	// daemonCredsFor resolves the account key via loadAccountKey(), which
	// checks VIBECRAFT_ACCOUNT_KEY before the config file.
	t.Setenv("VIBECRAFT_ACCOUNT_KEY", "vc_account_testkey")

	cfg := &Config{
		AccountKey: "vc_account_testkey",
		Machines:   map[string]MachineConfig{"vc-test": {Name: "vc-test"}},
	}

	// First call: no cached key → brokers.
	url, key, err := daemonCredsFor(cfg, "vc-test")
	if err != nil {
		t.Fatalf("daemonCredsFor: %v", err)
	}
	if url != "https://vc-test.vc.vibecraft.so" {
		t.Errorf("url = %q", url)
	}
	if key != "vc_machine_brokered_test_key_xyz" {
		t.Errorf("key = %q", key)
	}
	if brokerHits != 1 {
		t.Errorf("brokerHits = %d, want 1", brokerHits)
	}
	// The brokered key must be cached on cfg.
	if cfg.Machines["vc-test"].APIKey != "vc_machine_brokered_test_key_xyz" {
		t.Errorf("brokered key not cached: %+v", cfg.Machines["vc-test"])
	}

	// Second call: cached → no new broker hit.
	_, _, err = daemonCredsFor(cfg, "vc-test")
	if err != nil {
		t.Fatalf("second daemonCredsFor: %v", err)
	}
	if brokerHits != 1 {
		t.Errorf("second call should use cache; brokerHits = %d", brokerHits)
	}
}

// TestDaemonCredsFor_NoAccountKey verifies the no-auth path.
func TestDaemonCredsFor_NoAccountKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("VIBECRAFT_ACCOUNT_KEY", "")
	cfg := &Config{Machines: map[string]MachineConfig{"vc-x": {Name: "vc-x"}}}
	_, _, err := daemonCredsFor(cfg, "vc-x")
	if err == nil {
		t.Fatal("expected auth_required error, got nil")
	}
}

// TestDaemonCredsFor_EnvVarAccountKey verifies the headless path: with
// VIBECRAFT_ACCOUNT_KEY set and no config file (cfg == nil), daemonCredsFor
// still brokers a daemon key. Before the fix it read cfg.AccountKey
// directly and returned auth_required — breaking every daemon verb
// (status, task, screenshot, vault, …) under env-var / CI auth.
func TestDaemonCredsFor_EnvVarAccountKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/vc-test/cli-key", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vc_account_envkey" {
			http.Error(w, `{"error":"bad account key"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"machine_id":  "vc-test",
			"machine_url": "https://vc-test.vc.vibecraft.so",
			"api_key":     "vc_machine_brokered_env_xyz",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("VIBECRAFT_PLATFORM_URL", srv.URL)
	t.Setenv("HOME", t.TempDir()) // no config file → exercises env-var auth
	t.Setenv("VIBECRAFT_ACCOUNT_KEY", "vc_account_envkey")

	url, key, err := daemonCredsFor(nil, "vc-test")
	if err != nil {
		t.Fatalf("daemonCredsFor(nil, ...) under env-var auth: %v", err)
	}
	if url != "https://vc-test.vc.vibecraft.so" {
		t.Errorf("url = %q", url)
	}
	if key != "vc_machine_brokered_env_xyz" {
		t.Errorf("key = %q", key)
	}
}

// TestUnknownFlagIsValidationError verifies a bad flag maps to
// validation_error (not internal_error) so agents branch correctly.
func TestUnknownFlagIsValidationError(t *testing.T) {
	bin := findBinary(t)
	if bin == "" {
		return
	}
	c := exec.Command(bin, "status", "--bogus-flag")
	c.Env = append(c.Env, "HOME="+t.TempDir(), "VIBECRAFT_NO_AUTO_UPDATE=1")
	out, _ := c.CombinedOutput()
	if !strings.Contains(string(out), `"code":"validation_error"`) {
		t.Errorf("unknown flag should be validation_error; got: %s", string(out))
	}
}

// TestMachineTerminateRequiresConfirm verifies the one critical-action
// gate: `machine terminate` without --confirm returns confirmation_
// required and does NOT hit the network.
func TestMachineTerminateRequiresConfirm(t *testing.T) {
	prev := flagMachineConfirm
	flagMachineConfirm = false
	t.Cleanup(func() { flagMachineConfirm = prev })

	err := runMachineTerminate(machineTerminateCmd, []string{"vc-test"})
	if err == nil {
		t.Fatal("expected confirmation_required error, got nil")
	}
	var se *schema.Error
	if !errorsAsSchema(err, &se) || se.Code != schema.CodeConfirmationRequired {
		t.Errorf("got %v, want code=confirmation_required", err)
	}
}

func errorsAsSchema(err error, target **schema.Error) bool {
	for err != nil {
		if e, ok := err.(*schema.Error); ok {
			*target = e
			return true
		}
		type unwrap interface{ Unwrap() error }
		if u, ok := err.(unwrap); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}

// TestListAccountMachines parses the platform machines list.
func TestListAccountMachines(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"machines":[
			{"id":"vc-a","name":"A","host":"vc-a.vc.vibecraft.so","status":"active","access":"owner"},
			{"id":"vc-b","name":"B","host":"vc-b.vc.vibecraft.so","status":"active","access":"control"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("VIBECRAFT_PLATFORM_URL", srv.URL)

	machines, err := listAccountMachines("vc_account_x")
	if err != nil {
		t.Fatalf("listAccountMachines: %v", err)
	}
	if len(machines) != 2 {
		t.Fatalf("want 2 machines, got %d", len(machines))
	}
	if machines[0].ID != "vc-a" || machines[1].Access != "control" {
		t.Errorf("unexpected machines: %+v", machines)
	}
}

// TestMachineListHonorsAccountKeyEnv verifies `machine list` does the live
// platform query when authenticated via VIBECRAFT_ACCOUNT_KEY (headless /
// CI auth) with no config file — it used to gate on cfg.AccountKey alone
// and silently return an empty list under env-var auth.
func TestMachineListHonorsAccountKeyEnv(t *testing.T) {
	bin := findBinary(t)
	if bin == "" {
		return
	}
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vc_account_envkey" {
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"machines":[{"id":"vc-env","name":"Env Box","host":"vc-env.vc.vibecraft.so","status":"active","access":"owner"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := exec.Command(bin, "machine", "list")
	c.Env = append(c.Env,
		"HOME="+t.TempDir(), // no config file → exercises the env-var path
		"VIBECRAFT_PLATFORM_URL="+srv.URL,
		"VIBECRAFT_ACCOUNT_KEY=vc_account_envkey",
		"VIBECRAFT_NO_AUTO_UPDATE=1",
	)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("machine list failed: %v\n%s", err, out)
	}
	if hits == 0 {
		t.Errorf("machine list did not query the platform with VIBECRAFT_ACCOUNT_KEY")
	}
	if !strings.Contains(string(out), `"vc-env"`) {
		t.Errorf("machine list output missing the live machine; got: %s", out)
	}
}
