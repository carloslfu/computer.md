// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os/user"
	"strconv"
	"testing"
)

// Guards the v0.37.0→v0.38.0 follow-on: once the agent-shell runs bwrap
// as the host agent user, that user must be able to reach its
// per-sandbox socket (dir 0755 + socket chowned to it). agentSockUser
// is the resolver that decides whom to chown to; it must stay in sync
// with spawn_linux.go hostAgentCreds (same env var + default).
func TestAgentSockUser(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skipf("user.Current: %v", err)
	}

	t.Setenv("VIBECRAFT_SANDBOX_USER", cur.Username)
	uid, gid, ok := agentSockUser()
	if !ok {
		t.Fatalf("agentSockUser must resolve an existing user %q", cur.Username)
	}
	wantUID, _ := strconv.Atoi(cur.Uid)
	wantGID, _ := strconv.Atoi(cur.Gid)
	if uid != wantUID || gid != wantGID {
		t.Fatalf("agentSockUser = (%d,%d), want (%d,%d) for %q",
			uid, gid, wantUID, wantGID, cur.Username)
	}

	t.Setenv("VIBECRAFT_SANDBOX_USER", "vc-definitely-no-such-user-xyz")
	if _, _, ok := agentSockUser(); ok {
		t.Fatal("agentSockUser must report ok=false for a non-existent user " +
			"(callers then skip the chown rather than break)")
	}

	// Default (env unset) must target "vibecraft" — the production agent
	// identity. ok depends on the host having that user (true on a real
	// machine, false on CI/macOS); either way it must not panic and must
	// agree with a direct lookup.
	t.Setenv("VIBECRAFT_SANDBOX_USER", "")
	_, _, gotOK := agentSockUser()
	_, lookErr := user.Lookup("vibecraft")
	if (lookErr == nil) != gotOK {
		t.Fatalf("default agentSockUser ok=%v but user.Lookup(\"vibecraft\") err=%v "+
			"— resolver/ default out of sync", gotOK, lookErr)
	}
}
