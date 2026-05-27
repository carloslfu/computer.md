// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAgentShellSupervisorScriptKeepsSandboxAlive(t *testing.T) {
	script := agentShellSupervisorScript()
	for _, want := range []string{
		"mkfifo \"$queue\"",
		"while IFS= read -r id < \"$queue\"",
		"timeout --preserve-status 120s /bin/bash \"$cmd_file\"",
		"printf '%s' \"$code\" > \"$done_file\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("supervisor script missing %q:\n%s", want, script)
		}
	}
}

func TestRenderShellEnvQuotesValues(t *testing.T) {
	got := renderShellEnv(map[string]string{
		"PLAIN": "ok",
		"QUOTE": "don't leak",
	})
	if !strings.Contains(got, "export PLAIN='ok'\n") {
		t.Fatalf("missing plain env export: %q", got)
	}
	if !strings.Contains(got, "export QUOTE='don'\\''t leak'\n") {
		t.Fatalf("missing quoted env export: %q", got)
	}
}

func TestWriteAgentShellRequestFileIsReadableBySandboxUser(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	t.Setenv("VIBECRAFT_SANDBOX_USER", current.Username)

	path := t.TempDir() + "/request.sh"
	if err := writeAgentShellRequestFile(path, []byte("echo ok")); err != nil {
		t.Fatalf("writeAgentShellRequestFile failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 0600", got)
	}

	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	wantUID, _ := strconv.Atoi(current.Uid)
	wantGID, _ := strconv.Atoi(current.Gid)
	if int(st.Uid) != wantUID || int(st.Gid) != wantGID {
		t.Fatalf("owner = %d:%d, want %d:%d", st.Uid, st.Gid, wantUID, wantGID)
	}
}
