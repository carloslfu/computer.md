// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

// TestManagementRevokeSessionsForUser pins the daemon half of SESSION-REVOKE:
// POST /api/management/revoke-sessions-for-user {"userId":...} (health-token
// authed, like revoke-keys) deletes every browser (vc_session) AND SSO
// (vc_sso) session for that user and ONLY that user, so an offboarded
// teammate's open dashboard loses control immediately instead of at cookie
// expiry. Another user's session is untouched.
func TestManagementRevokeSessionsForUser(t *testing.T) {
	ts := newTestServer(t)
	db := ts.server.db

	future := time.Now().UTC().Add(8 * time.Hour)
	mkSession := func(hash, sub string) persistence.Session {
		return persistence.Session{
			IDHash: hash, Sub: sub, Access: "control",
			CreatedAt: time.Now().UTC(), ExpiresAt: future, LastSeen: time.Now().UTC(),
		}
	}
	if err := db.CreateSession(mkSession("victim-s1", "user-victim")); err != nil {
		t.Fatalf("seed s1: %v", err)
	}
	if err := db.CreateSession(mkSession("victim-s2", "user-victim")); err != nil {
		t.Fatalf("seed s2: %v", err)
	}
	if err := db.CreateSSOSession(mkSession("victim-sso", "user-victim")); err != nil {
		t.Fatalf("seed sso: %v", err)
	}
	if err := db.CreateSession(mkSession("other-s1", "user-other")); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	body := `{"userId":"user-victim"}`

	// Auth gate mirrors revoke-keys: no token / daemon token rejected.
	if w := postManagement(ts, "/api/management/revoke-sessions-for-user", "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: want 401, got %d", w.Code)
	}
	if w := postManagement(ts, "/api/management/revoke-sessions-for-user", "Bearer "+ts.daemonTok, body); w.Code != http.StatusUnauthorized {
		t.Errorf("daemon token: want 401, got %d", w.Code)
	}

	w := postManagement(ts, "/api/management/revoke-sessions-for-user", "Bearer "+ts.healthTok, body)
	if w.Code != http.StatusOK {
		t.Fatalf("health token revoke: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Revoked != 3 {
		t.Errorf("revoked: want 3 (2 sessions + 1 sso), got %d", resp.Revoked)
	}

	// The victim's sessions no longer resolve.
	if _, err := db.GetSessionByIDHash("victim-s1"); err == nil {
		t.Error("victim-s1 should be gone")
	}
	if _, err := db.GetSSOSessionByIDHash("victim-sso"); err == nil {
		t.Error("victim-sso should be gone")
	}
	// The bystander's session still resolves.
	if _, err := db.GetSessionByIDHash("other-s1"); err != nil {
		t.Errorf("other-s1 should survive (different user): %v", err)
	}

	// Second revoke is a clean no-op.
	w = postManagement(ts, "/api/management/revoke-sessions-for-user", "Bearer "+ts.healthTok, body)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Revoked != 0 {
		t.Errorf("second revoke: want 200/0, got %d/%d", w.Code, resp.Revoked)
	}
}
