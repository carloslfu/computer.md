// SPDX-License-Identifier: Apache-2.0

package routes

import (
	"strings"
	"testing"
)

// TestCaddyfileCmdUsesCaddyfileAdapter guards the bug where caddy was invoked as
// `caddy validate --config <tempfile>` without `--adapter caddyfile`. Caddy then
// parses the Caddyfile as JSON and fails ("config is not valid JSON ... did you
// mean the --adapter flag?"), which silently broke every validate-before-apply
// in safeWriteAndReload (and thus hosted-tool route changes). This test would
// fail on the pre-fix code, which had no adapter flag.
func TestCaddyfileCmdUsesCaddyfileAdapter(t *testing.T) {
	cmd := caddyfileCmd("validate", "/etc/caddy/.Caddyfile.123.validate")
	args := strings.Join(cmd.Args, " ")

	if !strings.Contains(args, "--adapter caddyfile") {
		t.Fatalf("caddy command must pass `--adapter caddyfile`, got: %q", args)
	}
	if len(cmd.Args) < 2 || cmd.Args[0] != "caddy" || cmd.Args[1] != "validate" {
		t.Fatalf("unexpected caddy command shape: %q", args)
	}
	if !strings.HasSuffix(args, "--config /etc/caddy/.Caddyfile.123.validate") {
		t.Fatalf("config path must be the final argument, got: %q", args)
	}

	// HOME must be set so Caddy can resolve its user-config dir instead of
	// warning and falling back to the current working directory.
	hasHome := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "HOME=") && len(e) > len("HOME=") {
			hasHome = true
			break
		}
	}
	if !hasHome {
		t.Fatalf("caddy command env must set HOME; env=%v", cmd.Env)
	}
}

// TestCaddyfileCmdReloadShape confirms the reload path is built the same way
// (adapter + config), since reload-with-old-Caddyfile only worked by accident of
// the live file being named exactly "Caddyfile".
func TestCaddyfileCmdReloadShape(t *testing.T) {
	cmd := caddyfileCmd("reload", "/etc/caddy/Caddyfile")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "reload") || !strings.Contains(args, "--adapter caddyfile") {
		t.Fatalf("reload command must use the caddyfile adapter, got: %q", args)
	}
}
