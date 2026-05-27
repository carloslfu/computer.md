// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/computer"
	"github.com/carloslfu/computer.md/daemon/sandbox"
)

// Phase 1: /api/daemon/spawn-worker is localhost-token-gated like
// /daemon/task, and when the flag is off (or the OS can't sandbox — the
// macOS test host) it runs the command the exact legacy way. The real
// sandboxed branch is proven on a Linux kernel by
// daemon/sandbox/sandbox_integration_test.go (14/14).
func TestSpawnWorker_AuthAndLegacyPath(t *testing.T) {
	ts := newTestServer(t)
	ts.server.cfg.LocalToken = "spawn-tok-xyz"
	// macOS test: sandbox.Supported()==false -> legacy path (no flag anymore)
	ts.server.shell = computer.NewShell(t.TempDir(), "")
	body := `{"name":"worker-1","system":"sys-a","cmd":["echo","hello-from-worker"]}`

	t.Run("no local token -> 401", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/spawn-worker", "127.0.0.1:5000", body, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-loopback -> 403", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/spawn-worker", "203.0.113.9:5000", body,
			map[string]string{"Authorization": "Bearer spawn-tok-xyz"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("loopback + token -> legacy run, output captured", func(t *testing.T) {
		// This subtest asserts the macOS-dev LEGACY contract (no sandbox →
		// run the command directly, capture output). On a sandbox-capable
		// kernel (Linux CI) the endpoint takes the privileged sandboxed
		// path instead, which needs root/netns and is proven separately by
		// daemon/sandbox/sandbox_integration_test.go. Asserting the legacy
		// contract on Linux is wrong (it hardcoded "Supported()==false");
		// that environment mismatch is exactly what made this fail on the
		// ubuntu CI runner while release.yml — which runs no tests — shipped
		// regardless. Branch on the real capability instead of assuming OS.
		if sandbox.Supported() {
			t.Skip("sandbox-capable host: the sandboxed worker path is " +
				"covered by the integration suite; this subtest asserts the " +
				"macOS legacy contract only")
		}
		w := ts.doRaw(t, "POST", "/daemon/spawn-worker", "127.0.0.1:5000", body,
			map[string]string{"Authorization": "Bearer spawn-tok-xyz"})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Sandboxed bool   `json:"sandboxed"`
			Output    string `json:"output"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		if resp.Sandboxed {
			t.Fatalf("Supported()==false must take the legacy path (sandboxed:false), got true")
		}
		if resp.Error != "" {
			t.Fatalf("legacy run errored: %s", resp.Error)
		}
		if want := "hello-from-worker"; !contains(resp.Output, want) {
			t.Fatalf("legacy output %q missing %q", resp.Output, want)
		}
	})

	t.Run("missing name/cmd -> 400", func(t *testing.T) {
		w := ts.doRaw(t, "POST", "/daemon/spawn-worker", "127.0.0.1:5000", `{"name":""}`,
			map[string]string{"Authorization": "Bearer spawn-tok-xyz"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// Regression for the Codex worker rollout (May 2026): spawned worker
// sandboxes default-allow the worker CLIs' backends. Without these
// FQDNs in the baseline egress, a spawned `codex` worker silently
// fails its first network call — same shape as the pre-fix
// "Claude Code is Missing" UX, just at the egress layer instead of
// the install layer. Pin the baseline so a refactor of the egress-
// assembly block doesn't silently drop one of them.
func TestSpawnWorkerEgressBaselineCoversBothCLIBackends(t *testing.T) {
	src, err := os.ReadFile("spawn_worker.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, want := range []string{
		`"api.anthropic.com"`, // Claude Code → Anthropic API
		`"*.openai.com"`,      // Codex API + auth subdomains
		`"chatgpt.com"`,       // ChatGPT OAuth landing for `codex login`
	} {
		if !strings.Contains(body, want) {
			t.Errorf("spawn_worker.go missing baseline egress FQDN %s — Codex/Claude Code workers would fail their first network call", want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
