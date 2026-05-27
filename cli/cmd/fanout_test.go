// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/carloslfu/computer.md/cli/output"
)

// TestMatchMachines covers the --machine selector grammar (A12).
func TestMatchMachines(t *testing.T) {
	// Isolate from any real config / env so matchMachines resolves "no
	// account key" deterministically and matches against the test cfg.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("VIBECRAFT_ACCOUNT_KEY", "")
	cfg := &Config{
		ActiveMachine: "vc-a",
		Machines: map[string]MachineConfig{
			"vc-a":    {URL: "https://vc-a.vc.vibecraft.so", APIKey: "vc_machine_a_test_a_a_a"},
			"vc-b":    {URL: "https://vc-b.vc.vibecraft.so", APIKey: "vc_machine_b_test_b_b_b"},
			"prod-x":  {URL: "https://prod-x.vc.vibecraft.so", APIKey: "vc_machine_x_test_x_x_x"},
		},
	}

	cases := []struct {
		name   string
		sel    string
		want   []string
		err    bool
	}{
		{"empty → active", "", []string{"vc-a"}, false},
		{"single id", "vc-b", []string{"vc-b"}, false},
		{"comma list", "vc-a,vc-b", []string{"vc-a", "vc-b"}, false},
		{"glob vc-*", "vc-*", []string{"vc-a", "vc-b"}, false},
		{"glob *-x", "*-x", []string{"prod-x"}, false},
		{"all", "all", []string{"prod-x", "vc-a", "vc-b"}, false},
		{"unknown id", "vc-z", nil, true},
		{"unknown in list", "vc-a,vc-z", nil, true},
		{"glob no match", "nope-*", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchMachines(cfg, tc.sel)
			if tc.err {
				if err == nil {
					t.Errorf("got %v, want err", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatchMachinesEnvVarAccountKey verifies fan-out machine resolution
// works under VIBECRAFT_ACCOUNT_KEY with no config file (cfg == nil) —
// it used to return auth_required, breaking `--machine all` headless.
func TestMatchMachinesEnvVarAccountKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vc_account_envkey" {
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"machines":[
			{"id":"vc-one","name":"One","host":"vc-one.vc.vibecraft.so","status":"active","access":"owner"},
			{"id":"vc-two","name":"Two","host":"vc-two.vc.vibecraft.so","status":"active","access":"owner"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("VIBECRAFT_PLATFORM_URL", srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("VIBECRAFT_ACCOUNT_KEY", "vc_account_envkey")

	got, err := matchMachines(nil, "all")
	if err != nil {
		t.Fatalf("matchMachines(nil, \"all\") under env-var auth: %v", err)
	}
	if strings.Join(got, ",") != "vc-one,vc-two" {
		t.Errorf("got %v, want [vc-one vc-two]", got)
	}
}

// TestFanoutStatusLive uses two fake daemon servers and verifies that
// runFanoutForStatus emits one envelope per machine and that the worst
// per-machine outcome propagates.
func TestFanoutStatusLive(t *testing.T) {
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"healthy","machine_id":"vc-ok","uptime":1}`)
	}))
	defer srvOK.Close()
	var failHits int32
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&failHits, 1)
		http.Error(w, `{"error":"oops"}`, http.StatusInternalServerError)
	}))
	defer srvFail.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "vibecraft")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := fmt.Sprintf(`{"active_machine":"vc-ok","machines":{
		"vc-ok": {"url":"%s","api_key":"vc_machine_ok_test_ok_ok"},
		"vc-fail": {"url":"%s","api_key":"vc_machine_fail_test_fail_fail"}
	}}`, srvOK.URL, srvFail.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	withOutputMode(t, output.ModeJSON)

	prev := flagMachineID
	flagMachineID = "all"
	t.Cleanup(func() { flagMachineID = prev })

	got, runErr := captureStdout(t, func() error { return runStatus(statusCmd, nil) })
	// runStatus may return an OutcomeError because vc-fail returns 5xx.
	// That's expected. We just want one line per machine in `got`.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 {
		t.Errorf("want 2 lines (one per machine), got %d:\n%s", len(lines), got)
	}
	if !strings.Contains(got, `"machine":"vc-ok"`) || !strings.Contains(got, `"machine":"vc-fail"`) {
		t.Errorf("output missing per-machine envelopes:\n%s", got)
	}
	if !strings.Contains(got, `"ok":true`) || !strings.Contains(got, `"ok":false`) {
		t.Errorf("missing one of ok=true/false:\n%s", got)
	}
	// runErr should be an OutcomeError (worst-code: vc-fail returned a CLI
	// error, so the worst is exit.CLIError=1).
	if runErr == nil {
		t.Errorf("expected error from worst-machine; got nil")
	}
}
