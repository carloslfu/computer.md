// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// app_service_deploy_test.go pins the operator-/CLI-facing hosted-tool
// lifecycle added for the "seamless tool deploy" work: deploy (redeploy an
// existing tool, reusing its stored unit + resolving vault refs), status,
// logs, env. The pure helpers are tested directly; the deploy/status/logs
// paths use the same mock seams (stubAppServiceHost) the restart tests use,
// plus a temp systemdUserDir so the unit file round-trips on the test host.

func withTempSystemdDir(t *testing.T) string {
	t.Helper()
	old := systemdUserDir
	dir := t.TempDir()
	systemdUserDir = dir
	t.Cleanup(func() { systemdUserDir = old })
	return dir
}

func writeUnit(t *testing.T, dir string, req installAppServiceRequest) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, req.Name+".service"), []byte(renderUnitFile(req)), 0o644); err != nil {
		t.Fatalf("writeUnit: %v", err)
	}
}

// ---- pure helpers ----

func TestParseUnitFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.service")
	if err := os.WriteFile(p, []byte(renderUnitFile(installAppServiceRequest{
		Name:             "x",
		ExecStart:        "/usr/bin/node /home/vibecraft/systems/x/s.js",
		WorkingDirectory: "/home/vibecraft/systems/x",
		Description:      "X tool",
		Environment:      []string{"A=1", "B=2"},
	})), 0o644); err != nil {
		t.Fatal(err)
	}
	pu, err := parseUnitFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if pu.ExecStart != "/usr/bin/node /home/vibecraft/systems/x/s.js" {
		t.Errorf("ExecStart = %q", pu.ExecStart)
	}
	if pu.WorkingDirectory != "/home/vibecraft/systems/x" {
		t.Errorf("WorkingDirectory = %q", pu.WorkingDirectory)
	}
	if pu.Description != "X tool" {
		t.Errorf("Description = %q", pu.Description)
	}
	if len(pu.Environment) != 2 || pu.Environment[0] != "A=1" || pu.Environment[1] != "B=2" {
		t.Errorf("Environment = %+v", pu.Environment)
	}
}

func TestMergeEnv_OverrideWinsPreserveOrder(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "C=3"}, []string{"B=20", "D=4"})
	want := "A=1,B=20,C=3,D=4"
	if strings.Join(got, ",") != want {
		t.Fatalf("mergeEnv = %v, want %s", got, want)
	}
}

func TestStripReservedInjectedEnv(t *testing.T) {
	got := stripReservedInjectedEnv([]string{
		"PORT=1", "VIBECRAFT_AI_PROXY_URL=x", "VIBECRAFT_AI_CREDITS_TOKEN=y", "AWS_REGION=us", "KEEP=2",
	})
	if strings.Join(got, ",") != "PORT=1,KEEP=2" {
		t.Fatalf("stripReservedInjectedEnv = %v", got)
	}
}

func TestEnvKeys_DropsBlankAndMalformed(t *testing.T) {
	got := envKeys([]string{"A=1", " B=2 ", "", "MALFORMED"})
	if strings.Join(got, ",") != "A,B" {
		t.Fatalf("envKeys = %v", got)
	}
}

// ---- deploy ----

func TestDeployHostedAppService_ReusesUnitAndResolvesVault(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	if err := ts.server.vaultStore.Set("NOTES_DB_PASSWORD", "s3cr3t", ""); err != nil {
		t.Fatal(err)
	}
	// Existing unit as the manager would have written it (incl. a
	// VibeCraft-injected var that must be stripped on redeploy).
	writeUnit(t, dir, installAppServiceRequest{
		Name:             "notes",
		ExecStart:        "/usr/bin/node /home/vibecraft/systems/notes/server.js",
		WorkingDirectory: "/home/vibecraft/systems/notes",
		Description:      "Notes",
		Port:             5055,
		Environment: []string{
			"PORT=5055", "OLD_FLAG=keep",
			"VIBECRAFT_AI_PROXY_URL=http://127.0.0.1:8420/api/ai/credits",
		},
	})

	showCalls := 0
	restore := stubAppServiceHost(t, func(ctx context.Context, uid, rt, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) >= 2 && args[1] == "show" {
			showCalls++
			if showCalls == 1 {
				return "MainPID=100\n", nil
			}
			return "MainPID=200\n", nil
		}
		return "", nil
	})
	defer restore()

	result, code := ts.server.deployHostedAppService(context.Background(), "notes",
		[]string{"DB_PASSWORD=$NOTES_DB_PASSWORD", "OLD_FLAG=changed"}, nil)
	if code != http.StatusOK {
		t.Fatalf("code %d: %+v", code, result)
	}
	if !result.OK || result.Status != "running" {
		t.Fatalf("want running+ok, got %+v", result)
	}
	if result.OldPID != 100 || result.NewPID != 200 {
		t.Fatalf("want pid cycle 100->200, got %+v", result)
	}

	b, err := os.ReadFile(filepath.Join(dir, "notes.service"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "Environment=DB_PASSWORD=s3cr3t") {
		t.Errorf("resolved vault value missing:\n%s", got)
	}
	if strings.Contains(got, "$NOTES_DB_PASSWORD") {
		t.Errorf("unresolved $ref leaked into unit:\n%s", got)
	}
	if !strings.Contains(got, "Environment=OLD_FLAG=changed") {
		t.Errorf("env override did not win:\n%s", got)
	}
	if !strings.Contains(got, "Environment=PORT=5055") {
		t.Errorf("base env dropped:\n%s", got)
	}
	if !strings.Contains(got, "ExecStart=/usr/bin/node /home/vibecraft/systems/notes/server.js") {
		t.Errorf("stored ExecStart not reused:\n%s", got)
	}
}

func TestDeployHostedAppService_RejectsUnknownRoute(t *testing.T) {
	ts := newTestServer(t)
	_, code := ts.server.deployHostedAppService(context.Background(), "missing", nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestDeployHostedAppService_RequiresExistingUnit(t *testing.T) {
	ts := newTestServer(t)
	withTempSystemdDir(t) // empty temp dir — no unit file
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	_, code := ts.server.deployHostedAppService(context.Background(), "notes", nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("want 409 for missing unit, got %d", code)
	}
}

func TestDeployHostedAppService_RejectsReservedEnv(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	writeUnit(t, dir, installAppServiceRequest{Name: "notes", ExecStart: "/bin/true"})
	_, code := ts.server.deployHostedAppService(context.Background(), "notes",
		[]string{"OPENAI_API_KEY=sk-test"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for reserved env, got %d", code)
	}
}

// ---- status ----

func TestHostedAppStatus_AssemblesRuntimeTruth(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	writeUnit(t, dir, installAppServiceRequest{
		Name:             "notes",
		ExecStart:        "/usr/bin/node /home/vibecraft/systems/notes/server.js",
		WorkingDirectory: "/home/vibecraft/systems/notes",
		Description:      "Notes",
		Environment:      []string{"PORT=5055", "SECRET_TOKEN=do-not-leak"},
	})

	restore := stubAppServiceHost(t, func(ctx context.Context, uid, rt, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) >= 2 && args[1] == "show" {
			return "MainPID=4242\nActiveState=active\nSubState=running\nExecMainStartTimestamp=Mon 2026-06-02 10:00:00 UTC\n", nil
		}
		if name == "git" {
			return "", fmt.Errorf("not a git repo")
		}
		return "", nil
	})
	defer restore()

	res, code := ts.server.hostedAppStatus(context.Background(), "notes")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if res.MainPID != 4242 || res.ActiveState != "active" || res.SubState != "running" {
		t.Fatalf("runtime state wrong: %+v", res)
	}
	if !res.Listening {
		t.Fatalf("expected listening true (stub): %+v", res)
	}
	if !res.UnitPresent || res.ExecStart == "" || res.WorkingDirectory == "" {
		t.Fatalf("unit fields missing: %+v", res)
	}
	hasKey := false
	for _, k := range res.EnvKeys {
		if k == "SECRET_TOKEN" {
			hasKey = true
		}
	}
	if !hasKey {
		t.Fatalf("env keys missing SECRET_TOKEN: %+v", res.EnvKeys)
	}
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), "do-not-leak") {
		t.Fatalf("env VALUE leaked in status output: %s", blob)
	}
}

func TestHostedAppStatus_RejectsUnknownRoute(t *testing.T) {
	ts := newTestServer(t)
	_, code := ts.server.hostedAppStatus(context.Background(), "missing")
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
}

// ---- logs ----

func TestHostedAppLogs_TailsJournal(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	var journalArgs string
	restore := stubAppServiceHost(t, func(ctx context.Context, uid, rt, name string, args ...string) (string, error) {
		if name == "journalctl" {
			journalArgs = strings.Join(args, " ")
			return "2026-06-02T10:00:00 notes[42]: listening on :5055\n2026-06-02T10:00:01 notes[42]: GET / 200\n", nil
		}
		return "", nil
	})
	defer restore()

	res, code := ts.server.hostedAppLogs(context.Background(), "notes", 50)
	if code != http.StatusOK {
		t.Fatalf("code %d: %+v", code, res)
	}
	if len(res.Lines) != 2 || !strings.Contains(res.Lines[0], "listening on :5055") {
		t.Fatalf("unexpected lines: %+v", res.Lines)
	}
	if !strings.Contains(journalArgs, "-u notes.service") || !strings.Contains(journalArgs, "-n 50") {
		t.Fatalf("journalctl args wrong: %q", journalArgs)
	}
}

func TestHostedAppLogs_RejectsUnknownRoute(t *testing.T) {
	ts := newTestServer(t)
	_, code := ts.server.hostedAppLogs(context.Background(), "missing", 10)
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
}

// ---- env ----

func TestHostedAppEnv_ListsKeysNeverValues(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	writeUnit(t, dir, installAppServiceRequest{
		Name:      "notes",
		ExecStart: "/bin/true",
		Environment: []string{
			"PORT=5055", "API_KEY=super-secret-value",
			"VIBECRAFT_AI_PROXY_URL=http://127.0.0.1:8420/api/ai/credits",
		},
	})

	res, code := ts.server.hostedAppEnv("notes")
	if code != http.StatusOK {
		t.Fatalf("code %d: %+v", code, res)
	}
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), "super-secret-value") {
		t.Fatalf("env VALUE leaked: %s", blob)
	}
	keys := map[string]bool{}
	reserved := map[string]bool{}
	for _, k := range res.Keys {
		keys[k.Key] = true
		if k.Reserved {
			reserved[k.Key] = true
		}
	}
	if !keys["API_KEY"] || !keys["PORT"] {
		t.Fatalf("expected keys missing: %+v", res.Keys)
	}
	if !reserved["VIBECRAFT_AI_PROXY_URL"] {
		t.Fatalf("expected VIBECRAFT_AI_PROXY_URL flagged reserved: %+v", res.Keys)
	}
}

func TestHostedAppEnv_RejectsBadNameAndUnknownRoute(t *testing.T) {
	ts := newTestServer(t)
	if _, code := ts.server.hostedAppEnv("Has-Caps"); code != http.StatusBadRequest {
		t.Errorf("bad name: want 400, got %d", code)
	}
	if _, code := ts.server.hostedAppEnv("missing"); code != http.StatusNotFound {
		t.Errorf("unknown route: want 404, got %d", code)
	}
}

// ---- HTTP-level: the real mux + withCookieOrBearer + handleHostedAppByName
// dispatch (action routing, method gating, body parse, auth). The host-exec
// seam is stubbed; everything above it is the production path a CLI request
// hits. ----

func TestHostedToolHTTP_DeployCyclesAndResolvesVault(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	if err := ts.server.vaultStore.Set("NOTES_DB_PASSWORD", "s3cr3t", ""); err != nil {
		t.Fatal(err)
	}
	writeUnit(t, dir, installAppServiceRequest{
		Name:             "notes",
		ExecStart:        "/usr/bin/node /home/vibecraft/systems/notes/server.js",
		WorkingDirectory: "/home/vibecraft/systems/notes",
		Port:             5055,
		Environment:      []string{"PORT=5055"},
	})
	showCalls := 0
	restore := stubAppServiceHost(t, func(ctx context.Context, uid, rt, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) >= 2 && args[1] == "show" {
			showCalls++
			if showCalls == 1 {
				return "MainPID=100\n", nil
			}
			return "MainPID=200\n", nil
		}
		return "", nil
	})
	defer restore()

	w := ts.do(t, "POST", "/api/hosted-apps/notes/deploy", "Bearer "+ts.signJWT(t, "user-1"),
		`{"environment":["DB_PASSWORD=$NOTES_DB_PASSWORD"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"status":"running"`) {
		t.Fatalf("want ok running, got %s", w.Body.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "notes.service"))
	if !strings.Contains(string(b), "Environment=DB_PASSWORD=s3cr3t") {
		t.Errorf("vault ref not resolved into unit:\n%s", b)
	}
}

func TestHostedToolHTTP_StatusReturnsRuntimeTruth(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	writeUnit(t, dir, installAppServiceRequest{
		Name: "notes", ExecStart: "/bin/true",
		Environment: []string{"SECRET_TOKEN=do-not-leak"},
	})
	restore := stubAppServiceHost(t, func(ctx context.Context, uid, rt, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) >= 2 && args[1] == "show" {
			return "MainPID=4242\nActiveState=active\nSubState=running\n", nil
		}
		return "", fmt.Errorf("skip")
	})
	defer restore()

	w := ts.do(t, "GET", "/api/hosted-apps/notes/status", "Bearer "+ts.signJWT(t, "user-1"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"main_pid":4242`) || !strings.Contains(body, `"listening":true`) {
		t.Fatalf("status body missing runtime truth: %s", body)
	}
	if !strings.Contains(body, `"SECRET_TOKEN"`) {
		t.Fatalf("status should list env KEYS: %s", body)
	}
	if strings.Contains(body, "do-not-leak") {
		t.Fatalf("env VALUE leaked over HTTP: %s", body)
	}
}

func TestHostedToolHTTP_LogsAndEnv(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	writeUnit(t, dir, installAppServiceRequest{
		Name: "notes", ExecStart: "/bin/true",
		Environment: []string{"API_KEY=hidden"},
	})
	restore := stubAppServiceHost(t, func(ctx context.Context, uid, rt, name string, args ...string) (string, error) {
		if name == "journalctl" {
			return "2026-06-02T10:00:00 notes[42]: up on :5055\n", nil
		}
		return "", nil
	})
	defer restore()

	wl := ts.do(t, "GET", "/api/hosted-apps/notes/logs?lines=5", "Bearer "+ts.signJWT(t, "user-1"), "")
	if wl.Code != http.StatusOK || !strings.Contains(wl.Body.String(), "up on :5055") {
		t.Fatalf("logs: code %d body %s", wl.Code, wl.Body.String())
	}
	we := ts.do(t, "GET", "/api/hosted-apps/notes/env", "Bearer "+ts.signJWT(t, "user-1"), "")
	if we.Code != http.StatusOK || !strings.Contains(we.Body.String(), `"API_KEY"`) {
		t.Fatalf("env: code %d body %s", we.Code, we.Body.String())
	}
	if strings.Contains(we.Body.String(), "hidden") {
		t.Fatalf("env VALUE leaked over HTTP: %s", we.Body.String())
	}
}

func TestHostedToolHTTP_MethodAndAuthGating(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	// GET on a POST-only action → 405.
	if w := ts.do(t, "GET", "/api/hosted-apps/notes/deploy", "Bearer "+ts.signJWT(t, "user-1"), ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /deploy: want 405, got %d", w.Code)
	}
	// POST on a GET-only action → 405.
	if w := ts.do(t, "POST", "/api/hosted-apps/notes/status", "Bearer "+ts.signJWT(t, "user-1"), ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /status: want 405, got %d", w.Code)
	}
	// No auth → 401 (withCookieOrBearer).
	if w := ts.do(t, "GET", "/api/hosted-apps/notes/status", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: want 401, got %d", w.Code)
	}
}
