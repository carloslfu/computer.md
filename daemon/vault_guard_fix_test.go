// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"

	"github.com/carloslfu/computer.md/daemon/guardrails"
)

// TestRulesPost_RejectsUnknownAction proves the daemon-vault-guard-2 fix at the
// write boundary: the /rules POST handler previously accepted any non-empty
// action string and persisted it verbatim, so a miscased "Block" or a typo
// "deny" was stored and then silently failed open to Allow at evaluation time.
// The handler now validates against the known enum and rejects unknown actions
// with 400 before they ever reach the engine.
func TestRulesPost_RejectsUnknownAction(t *testing.T) {
	ts := newTestServer(t)
	// The handler reaches action validation after requireControl + the
	// name/pattern/action presence check, and BEFORE grEngine.AddRule — so the
	// reject path does not depend on a wired engine.
	jwtToken := ts.signJWT(t, "user-control")

	// Genuinely-unknown actions (not just miscased) must 400 before AddRule.
	// Miscased-but-valid values ("Block", "ALLOW") are normalized and accepted
	// — that path is covered by TestRulesPost_AcceptsAndNormalizesValidAction.
	badActions := []string{"deny", "confirmm", "warn", "nonsense", "blockk", "allowed"}
	for _, action := range badActions {
		body := `{"name":"r","pattern":"do-it","action":"` + action + `"}`
		w := ts.do(t, "POST", "/rules", "Bearer "+jwtToken, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("action %q: want 400 (unknown action rejected), got %d: %s", action, w.Code, w.Body.String())
		}
	}
}

// TestRulesPost_AcceptsAndNormalizesValidAction confirms the validation does not
// break legitimate rule creation: canonical actions (any case) pass and are
// persisted in normalized lowercase form so evaluation matches the enum.
func TestRulesPost_AcceptsAndNormalizesValidAction(t *testing.T) {
	ts := newTestServer(t)
	// A real engine is needed here because the valid path calls AddRule.
	ts.server.grEngine = guardrails.NewEngine(ts.server.db)
	jwtToken := ts.signJWT(t, "user-control")

	cases := []struct{ in, wantStored string }{
		{"block", "block"},
		{"Confirm", "confirm"},
		{"ALLOW", "allow"},
	}
	for _, c := range cases {
		body := `{"name":"r-` + c.in + `","pattern":"do-it","action":"` + c.in + `"}`
		w := ts.do(t, "POST", "/rules", "Bearer "+jwtToken, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("action %q: want 201, got %d: %s", c.in, w.Code, w.Body.String())
		}
	}

	rules, err := ts.server.grEngine.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	for _, c := range cases {
		found := false
		for _, r := range rules {
			if r.Pattern == "do-it" && r.Name == "r-"+c.in {
				found = true
				if r.Action != c.wantStored {
					t.Errorf("rule %q: stored action = %q, want normalized %q", c.in, r.Action, c.wantStored)
				}
			}
		}
		if !found {
			t.Errorf("rule for input %q was not persisted", c.in)
		}
	}
}

// TestRulesPost_StillRequiresAction confirms the pre-existing presence check is
// intact: an empty/absent action is still rejected (and not coerced).
func TestRulesPost_StillRequiresAction(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-control")
	w := ts.do(t, "POST", "/rules", "Bearer "+jwtToken, `{"name":"r","pattern":"p"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing action: want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestTokenAuth_ConstantTimeCompareBehavior locks in the daemon-persist-auth-2
// refactor: switching the health-token and management-token checks from a plain
// `!=` to crypto/subtle.ConstantTimeCompare must preserve accept/reject
// semantics exactly. The constant-time property itself is not observable from a
// functional test; this guards against the refactor accidentally inverting or
// loosening the comparison (e.g. accepting a wrong-length or wrong-value token).
func TestTokenAuth_ConstantTimeCompareBehavior(t *testing.T) {
	ts := newTestServer(t)

	// --- Health-token gated endpoint (/health, withHealthAuth) ---
	if w := ts.do(t, "GET", "/health", "Bearer "+ts.healthTok, ""); w.Code != http.StatusOK {
		t.Errorf("correct health token: want 200, got %d", w.Code)
	}
	healthRejects := []string{
		ts.healthTok + "x",                 // longer
		ts.healthTok[:len(ts.healthTok)-1], // shorter
		"wrong-token",                      // different value
		ts.daemonTok,                       // valid-but-wrong token
		"",                                 // empty (caught earlier as missing header)
	}
	for _, tok := range healthRejects {
		auth := ""
		if tok != "" {
			auth = "Bearer " + tok
		}
		if w := ts.do(t, "GET", "/health", auth, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("health token %q: want 401, got %d", tok, w.Code)
		}
	}

	// --- Management-token gated endpoint (/management/usage, withManagementAuth) ---
	// The correct token must PASS the auth gate. The handler may then 200 or
	// 500 depending on wired deps (usageStore is nil in the harness) — the
	// load-bearing assertion is "not 401", i.e. the constant-time compare
	// accepted the right token.
	if w := ts.do(t, "GET", "/management/usage", "Bearer "+ts.daemonTok, ""); w.Code == http.StatusUnauthorized {
		t.Errorf("correct management token: want auth to pass (not 401), got 401")
	}
	mgmtRejects := []string{
		ts.daemonTok + "x",
		ts.daemonTok[:len(ts.daemonTok)-1],
		"wrong-token",
		ts.healthTok, // valid-but-wrong token
	}
	for _, tok := range mgmtRejects {
		if w := ts.do(t, "GET", "/management/usage", "Bearer "+tok, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("management token %q: want 401, got %d", tok, w.Code)
		}
	}
}
