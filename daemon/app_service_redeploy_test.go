// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// app_service_redeploy_test.go pins the fix for the redeploy env-corruption
// bug: stored env values were already vault-resolved literals, but a bare
// redeploy re-ran the resolver over ALL merged lines, silently rewriting any
// stored literal that happened to contain a `$NAME` token colliding with a
// vault secret name.

// TestDeployHostedAppService_DoesNotReResolveStoredEnvLiterals is the
// red→green. A stored secret literal `JWT_SECRET=xK$SESSION-9f` (already
// resolved at install time) must survive a bare redeploy VERBATIM even though
// a vault secret named SESSION exists. The buggy path re-resolved it and
// replaced the embedded `$SESSION` with the vault value, corrupting the app's
// signing key on restart.
//
// Fixture discipline: the embedded token must CLEANLY match the SESSION
// secret. The resolver's pattern is `\$\{?([A-Z][A-Z0-9_]*)\}?`, which is
// greedy over [A-Z0-9_]. A token like `$SESSION9f` captures `SESSION9` (the
// digit extends the name) — which matches NO secret, so even the buggy
// resolve-everything path leaves it unchanged and the test passes on broken
// code. A boundary char that is NOT in [A-Z0-9_] (here `-`) terminates the
// token at `SESSION`, so `$SESSION` resolves to the SESSION secret on the
// buggy path and the corruption is observable.
func TestDeployHostedAppService_DoesNotReResolveStoredEnvLiterals(t *testing.T) {
	ts := newTestServer(t)
	dir := withTempSystemdDir(t)
	if err := ts.server.db.CreateRoute("notes", 5055); err != nil {
		t.Fatal(err)
	}
	// A vault secret whose NAME collides with a token embedded in a stored
	// literal. If the redeploy re-resolves stored values, this value will be
	// spliced into JWT_SECRET and corrupt it.
	if err := ts.server.vaultStore.Set("SESSION", "VAULT_SESSION_VALUE", ""); err != nil {
		t.Fatal(err)
	}

	// Stored env as written at install time: JWT_SECRET is an already-resolved
	// LITERAL that happens to contain `$SESSION`.
	const storedJWT = "JWT_SECRET=xK$SESSION-9fLiteral"
	writeUnit(t, dir, installAppServiceRequest{
		Name:             "notes",
		ExecStart:        "/usr/bin/node /home/vibecraft/systems/notes/server.js",
		WorkingDirectory: "/home/vibecraft/systems/notes",
		Description:      "Notes",
		Port:             5055,
		Environment:      []string{"PORT=5055", storedJWT},
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

	// Bare redeploy — no env overrides at all.
	result, code := ts.server.deployHostedAppService(context.Background(), "notes", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("code %d: %+v", code, result)
	}

	eb, err := os.ReadFile(filepath.Join(dir, "notes.env"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(eb)
	if !strings.Contains(env, storedJWT) {
		t.Errorf("stored literal was mutated on redeploy.\nwant line: %q\ngot env:\n%s", storedJWT, env)
	}
	if strings.Contains(env, "VAULT_SESSION_VALUE") {
		t.Errorf("vault SESSION value spliced into a stored literal (re-resolution bug):\n%s", env)
	}
}

// TestDeployHostedAppService_StillResolvesCallerOverrides guards the
// non-regression: a CALLER-PROVIDED `$SECRET` override must still resolve.
// Only stored values are passed through verbatim.
func TestDeployHostedAppService_StillResolvesCallerOverrides(t *testing.T) {
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
		Description:      "Notes",
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

	result, code := ts.server.deployHostedAppService(context.Background(), "notes",
		[]string{"DB_PASSWORD=$NOTES_DB_PASSWORD"}, nil)
	if code != http.StatusOK {
		t.Fatalf("code %d: %+v", code, result)
	}

	eb, err := os.ReadFile(filepath.Join(dir, "notes.env"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(eb)
	if !strings.Contains(env, "DB_PASSWORD=s3cr3t") {
		t.Errorf("caller-provided $ref override did not resolve:\n%s", env)
	}
	if strings.Contains(env, "$NOTES_DB_PASSWORD") {
		t.Errorf("unresolved $ref leaked into env file:\n%s", env)
	}
}
