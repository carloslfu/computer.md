// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// app_service_test.go pins the pure pieces of the install-app-service
// endpoint — the parts that don't need a real systemd-user bus on the
// test host. Production behavior (writing the unit, running daemon-
// reload + enable --now) is covered by the in-prod verification turn
// after each release.

func TestRenderUnitFile_HasAllExpectedSections(t *testing.T) {
	got := renderUnitFile(installAppServiceRequest{
		Name:             "expense-tracker",
		ExecStart:        "/usr/bin/python3 /home/vibecraft/systems/expense-tracker/server.py",
		WorkingDirectory: "/home/vibecraft/systems/expense-tracker",
		Port:             5050,
		Description:      "Expense tracker",
		Environment:      []string{"PORT=5050", "DB_PATH=/home/vibecraft/systems/expense-tracker/state/db.sqlite"},
	})

	wantFragments := []string{
		"[Unit]",
		"Description=Expense tracker",
		"After=default.target",
		"[Service]",
		"Type=simple",
		"WorkingDirectory=/home/vibecraft/systems/expense-tracker",
		"Environment=PORT=5050",
		"Environment=DB_PATH=/home/vibecraft/systems/expense-tracker/state/db.sqlite",
		"ExecStart=/usr/bin/python3 /home/vibecraft/systems/expense-tracker/server.py",
		"Restart=on-failure",
		"[Install]",
		"WantedBy=default.target",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(got, frag) {
			t.Errorf("rendered unit missing %q\n--- got ---\n%s", frag, got)
		}
	}
}

// Empty Description falls back to the service name so `systemctl
// status` shows something meaningful instead of a blank line.
func TestRenderUnitFile_DescriptionFallsBackToName(t *testing.T) {
	got := renderUnitFile(installAppServiceRequest{
		Name:      "my-app",
		ExecStart: "/usr/bin/node /home/vibecraft/systems/my-app/server.js",
	})
	if !strings.Contains(got, "Description=my-app\n") {
		t.Errorf("expected Description= to fall back to name\n%s", got)
	}
}

// Empty WorkingDirectory falls back to vibecraft's home — useful for
// one-shot tools that don't need a project dir.
func TestRenderUnitFile_WorkingDirFallsBackToHome(t *testing.T) {
	got := renderUnitFile(installAppServiceRequest{
		Name:      "my-app",
		ExecStart: "/usr/bin/echo hello",
	})
	if !strings.Contains(got, "WorkingDirectory=/home/"+vibecraftUser+"\n") {
		t.Errorf("expected WorkingDirectory to fall back to vibecraft home\n%s", got)
	}
}

// Empty Environment entries are dropped — keeps the file clean when
// callers build the slice from optional vault refs.
func TestRenderUnitFile_DropsBlankEnvironmentEntries(t *testing.T) {
	got := renderUnitFile(installAppServiceRequest{
		Name:        "my-app",
		ExecStart:   "/bin/true",
		Environment: []string{"", "  ", "FOO=bar", ""},
	})
	if strings.Contains(got, "Environment=\n") || strings.Contains(got, "Environment=  \n") {
		t.Errorf("blank Environment line leaked through\n%s", got)
	}
	if !strings.Contains(got, "Environment=FOO=bar\n") {
		t.Errorf("real Environment entry missing\n%s", got)
	}
}

func TestValidateAppServiceEnvironment_RejectsPlatformSecrets(t *testing.T) {
	cases := [][]string{
		{"OPENAI_API_KEY=sk-proj-not-for-apps"},
		{"ANTHROPIC_API_KEY=sk-ant-not-for-apps"},
		{"VIBECRAFT_LOCAL_TOKEN=tok"},
		{"VIBECRAFT_AI_CREDITS_TOKEN=vc-user-supplied"},
		{"AWS_SECRET_ACCESS_KEY=secret"},
		{"CONFIG_PATH=/etc/vibecraft/openai.key"},
		{"CLOUD_INIT=/var/lib/cloud/instance/user-data.txt"},
	}
	for _, env := range cases {
		if err := validateAppServiceEnvironment(env); err == nil {
			t.Fatalf("expected reserved env %v to reject", env)
		}
	}
}

func TestValidateAppServiceEnvironment_AllowsCustomerOwnedNames(t *testing.T) {
	env := []string{
		"PORT=5050",
		"CUSTOMER_OPENAI_KEY_REF=$OPENAI_VAULT_KEY",
		"DATABASE_URL=file:/home/vibecraft/systems/app/state/db.sqlite",
	}
	if err := validateAppServiceEnvironment(env); err != nil {
		t.Fatalf("expected customer env to pass: %v", err)
	}
}

// Validation: ExecStart must be an absolute path. systemd rejects
// bare names; we surface the error at API boundary for a clean 400.
func TestInstallAppService_RejectsRelativeExecStart(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "tok"
	body := `{"name":"my-app","exec_start":"python3 server.py"}`

	w := ts.doRaw(t, "POST", "/daemon/install-app-service", "127.0.0.1:5000", body,
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for relative ExecStart, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "absolute path") {
		t.Errorf("400 should mention 'absolute path'; got %s", w.Body.String())
	}
}

func TestInstallAppService_RejectsReservedEnvironment(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "tok"
	body := `{"name":"my-app","exec_start":"/bin/true","environment":["OPENAI_API_KEY=sk-proj-test"]}`

	w := ts.doRaw(t, "POST", "/daemon/install-app-service", "127.0.0.1:5000", body,
		map[string]string{"Authorization": "Bearer tok"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for reserved env, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "OPENAI_API_KEY") {
		t.Errorf("400 should name reserved env; got %s", w.Body.String())
	}
}

func TestInstallAppService_RejectsBadName(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "tok"

	for _, badName := range []string{"", "../escape", "Has-Caps", "with space", "trailing-slash/"} {
		body := `{"name":"` + badName + `","exec_start":"/bin/true"}`
		w := ts.doRaw(t, "POST", "/daemon/install-app-service", "127.0.0.1:5000", body,
			map[string]string{"Authorization": "Bearer tok"})
		if w.Code != http.StatusBadRequest {
			t.Errorf("name %q: expected 400, got %d", badName, w.Code)
		}
	}
}

// Auth pins: localhost-token-only. No token → 401. Non-loopback → 403.
// Same shape as /daemon/task and /daemon/spawn-worker — keeps the
// manager-only contract visible.
func TestInstallAppService_AuthGate(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "tok"
	body := `{"name":"my-app","exec_start":"/bin/true"}`

	t.Run("no token → 401", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/install-app-service", "127.0.0.1:5000", body, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("non-loopback → 403", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/install-app-service", "203.0.113.9:5000", body,
			map[string]string{"Authorization": "Bearer tok"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// waitForLocalPort is the load-bearing "is the backend actually
// listening on the host's loopback" check — the whole reason this
// endpoint exists. Spin up a tiny TCP listener, confirm the helper
// reports true. Then close it and confirm a deadline elapses with
// false. Same shape both ways = no false-positive / false-negative
// at the boundary.
func TestWaitForLocalPort(t *testing.T) {
	// Listen on a random local port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	defer func() { _ = ln.Close() }()

	if !waitForLocalPort(context.Background(), port, 1*time.Second) {
		t.Errorf("waitForLocalPort returned false for a listening port %d", port)
	}

	// Close and verify the deadline path returns false within a tight
	// window (proves we're not blocking on the full timeout when the
	// connection is reachable, but also that we DO return false when
	// the port is gone).
	_ = ln.Close()
	// Pick a fresh port that's almost certainly not in use.
	deadPort, _ := strconv.Atoi(strings.Split(ln.Addr().String(), ":")[1])
	start := time.Now()
	if waitForLocalPort(context.Background(), deadPort, 600*time.Millisecond) {
		t.Errorf("waitForLocalPort returned true for closed port %d (would lie about deploy success)", deadPort)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("waitForLocalPort took %v to give up — should respect the 600ms deadline", elapsed)
	}
}

func TestRestartHostedAppService_CyclesMainPIDAndChecksPort(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.server.db.CreateRoute("caldris-box", 8787); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	var commands []string
	showCalls := 0
	restore := stubAppServiceHost(t, func(ctx context.Context, uid, runtimeDir, name string, args ...string) (string, error) {
		cmd := name + " " + strings.Join(args, " ")
		commands = append(commands, cmd)
		if name == "systemctl" && len(args) >= 4 && args[1] == "show" {
			showCalls++
			if showCalls == 1 {
				return "MainPID=111\n", nil
			}
			return "MainPID=222\n", nil
		}
		return "", nil
	})
	defer restore()

	result, code := ts.server.restartHostedAppService(context.Background(), "caldris-box")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !result.OK || result.Status != "running" || !result.ListeningAfter {
		t.Fatalf("expected running ok result, got %+v", result)
	}
	if result.OldPID != 111 || result.NewPID != 222 {
		t.Fatalf("expected old/new pid proof 111 -> 222, got %+v", result)
	}

	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "systemctl --user restart caldris-box.service") {
		t.Fatalf("restart command missing from:\n%s", joined)
	}
}

func TestRestartHostedAppService_FailsWhenPIDDoesNotCycle(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.server.db.CreateRoute("caldris-box", 8787); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	restore := stubAppServiceHost(t, func(ctx context.Context, uid, runtimeDir, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) >= 4 && args[1] == "show" {
			return "MainPID=111\n", nil
		}
		return "", nil
	})
	defer restore()

	result, code := ts.server.restartHostedAppService(context.Background(), "caldris-box")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if result.OK {
		t.Fatalf("expected failed result when PID does not cycle: %+v", result)
	}
	if result.Status != "failed" || !strings.Contains(result.Detail, "did not cycle") {
		t.Fatalf("expected did-not-cycle detail, got %+v", result)
	}
}

func TestRestartHostedAppService_RejectsUnknownRoute(t *testing.T) {
	ts := newTestServer(t)
	result, code := ts.server.restartHostedAppService(context.Background(), "missing-app")
	if code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %+v", code, result)
	}
}

func TestAppRestartLocalEndpointAuthAndShape(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.server.db.CreateRoute("caldris-box", 8787); err != nil {
		t.Fatalf("CreateRoute: %v", err)
	}

	restore := stubAppServiceHost(t, func(ctx context.Context, uid, runtimeDir, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) >= 4 && args[1] == "show" {
			return "MainPID=42\n", nil
		}
		return "", nil
	})
	defer restore()

	w := ts.doRaw(t, "POST", "/apps/caldris-box/restart", "127.0.0.1:5000", "", ts.lauth())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"old_pid":42`) || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("response should include pid proof and ok=false on stale PID:\n%s", w.Body.String())
	}
}

func stubAppServiceHost(t *testing.T, runAs func(context.Context, string, string, string, ...string) (string, error)) func() {
	t.Helper()
	oldRuntime := vibecraftRuntimeContextFn
	oldHost := runHostCmdFn
	oldRunAs := runAsVibecraftUserFn
	oldWait := waitForLocalPortFn
	oldTimeout := serviceCycleTimeout

	vibecraftRuntimeContextFn = func() (string, string, error) {
		return "1000", "/run/user/1000", nil
	}
	runHostCmdFn = func(ctx context.Context, name string, args ...string) (string, error) {
		return "", nil
	}
	runAsVibecraftUserFn = runAs
	waitForLocalPortFn = func(ctx context.Context, port int, deadline time.Duration) bool {
		return true
	}
	serviceCycleTimeout = 20 * time.Millisecond

	return func() {
		vibecraftRuntimeContextFn = oldRuntime
		runHostCmdFn = oldHost
		runAsVibecraftUserFn = oldRunAs
		waitForLocalPortFn = oldWait
		serviceCycleTimeout = oldTimeout
	}
}
