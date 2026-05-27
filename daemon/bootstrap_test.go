// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression for May 2026: Claude Code (the manager's default worker)
// was "Missing" on a BYOM machine whose frozen vibecraft-update.sh
// predated the Claude Code rollout. vibecraft-update.sh is written once
// at install time and never rolls forward; only the daemon binary
// self-updates. ensureWorkerAgents is the in-binary self-heal that
// therefore actually propagates. These pin its pure pieces — and the
// parallel pieces for Codex, which rides the exact same path.

func TestBinaryInstalledDetectsRegularFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")

	if binaryInstalled(bin) {
		t.Fatalf("binaryInstalled true for a path that does not exist")
	}

	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !binaryInstalled(bin) {
		t.Errorf("binaryInstalled false for an existing binary at %s", bin)
	}

	// A directory at the path is not an installed binary — guards
	// against a half-made ~/.npm-global/bin/<agent>/ dir reading as
	// "installed" and suppressing the self-heal forever.
	asDir := filepath.Join(dir, "asdir")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if binaryInstalled(asDir) {
		t.Errorf("binaryInstalled true for a directory")
	}
}

func TestClaudeInstallScriptMatchesProvisioningRoutine(t *testing.T) {
	// Pin the routine so it cannot silently drift from the install
	// paths (public/install.sh, lib/cloud-init.ts). A drift here means
	// self-healed machines diverge from freshly-provisioned ones.
	for _, want := range []string{
		"npm config set prefix ~/.npm-global",
		"@anthropic-ai/claude-code",
		".npm-global/bin", // PATH line appended to ~/.bashrc
	} {
		if !strings.Contains(claudeInstallScript, want) {
			t.Errorf("claudeInstallScript missing %q\nscript=%q", want, claudeInstallScript)
		}
	}
}

func TestClaudeCodeBinIsUnderNpmGlobalPrefix(t *testing.T) {
	// The idempotency check and the install routine must agree on the
	// location, or the self-heal loops (installs, never sees it) or
	// never runs (false "already installed").
	if !strings.HasSuffix(claudeCodeBin, "/.npm-global/bin/claude") {
		t.Errorf("claudeCodeBin %q is not under the pinned npm-global prefix", claudeCodeBin)
	}
}

// Codex parallels — same shape, same risks. If the install script
// silently drops the npm-global prefix line, the bin constant points
// somewhere npm never wrote, and the self-heal never sees the binary
// it just installed: result is a 5-min reinstall loop forever.

func TestCodexInstallScriptMatchesProvisioningRoutine(t *testing.T) {
	for _, want := range []string{
		"npm config set prefix ~/.npm-global",
		"@openai/codex",
		".npm-global/bin", // PATH line appended to ~/.bashrc
		"~/.codex",        // session/credential dir created up-front
	} {
		if !strings.Contains(codexInstallScript, want) {
			t.Errorf("codexInstallScript missing %q\nscript=%q", want, codexInstallScript)
		}
	}
}

func TestCodexBinIsUnderNpmGlobalPrefix(t *testing.T) {
	if !strings.HasSuffix(codexBin, "/.npm-global/bin/codex") {
		t.Errorf("codexBin %q is not under the pinned npm-global prefix", codexBin)
	}
}

// The two worker bins MUST share the same /.npm-global/bin parent.
// If they diverge (e.g. someone moves codex under a different prefix),
// the install scripts' shared `npm config set prefix ~/.npm-global`
// stops covering both and one of them silently lands somewhere the
// self-heal never finds — looping reinstall every 5 min forever.
func TestWorkerBinsShareTheSameNpmGlobalParent(t *testing.T) {
	claudeParent := filepath.Dir(claudeCodeBin)
	codexParent := filepath.Dir(codexBin)
	if claudeParent != codexParent {
		t.Errorf("worker bins must share the same parent dir: claude=%q codex=%q", claudeParent, codexParent)
	}
}

func TestChromeInstallScriptMatchesProvisioningRoutine(t *testing.T) {
	for _, want := range []string{
		"https://dl.google.com/linux/linux_signing_key.pub",
		"/usr/share/keyrings/google-chrome.gpg",
		"/etc/apt/sources.list.d/google-chrome.list",
		"google-chrome-stable",
	} {
		if !strings.Contains(chromeInstallScript, want) {
			t.Errorf("chromeInstallScript missing %q\nscript=%q", want, chromeInstallScript)
		}
	}
}

func TestChromeInstalledRequiresWrapperAndPayload(t *testing.T) {
	if chromeWrapperBin != "/usr/bin/google-chrome-stable" {
		t.Errorf("chromeWrapperBin = %q", chromeWrapperBin)
	}
	if chromePayloadBin != "/opt/google/chrome/google-chrome" {
		t.Errorf("chromePayloadBin = %q", chromePayloadBin)
	}
}

func TestChromeLaunchWrappersForceNoFirstRun(t *testing.T) {
	for _, want := range []string{
		"/usr/bin/google-chrome-stable",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-default-apps",
	} {
		if !strings.Contains(chromeLaunchWrapper, want) {
			t.Errorf("chromeLaunchWrapper missing %q\nscript=%q", want, chromeLaunchWrapper)
		}
	}
	for _, want := range []string{
		"/usr/local/bin/chrome",
		"/usr/local/bin/google-chrome",
		"/usr/local/bin/google-chrome-stable",
	} {
		found := false
		for _, got := range chromeLaunchWrapperBins {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("chromeLaunchWrapperBins missing %q: %v", want, chromeLaunchWrapperBins)
		}
	}
}
