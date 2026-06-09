// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/core"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// TestFilesEndpoints_RejectViewTier pins the fix for the view-tier
// file-read leak: GET /api/files/<path> and /api/files-ls serve raw bytes
// from anywhere under /home/vibecraft (worker subscription credentials,
// the db.md company store). A read-only `view` principal must be blocked
// even on reads — mirroring /keys' requireControlTier discipline — while a
// `control` principal still gets through.
//
// Red proof (before the fix): both endpoints were wrapped only in
// withCookieOrBearer, which authenticates a view JWT and stores the tier
// in context but never enforces it. A view token returned 400 "path is
// required" / a directory listing (i.e. it reached the handler), NOT 403.
func TestFilesEndpoints_RejectViewTier(t *testing.T) {
	ts := newTestServer(t)

	viewTok := ts.signJWTWithAccess(t, "user-view", "view")
	controlTok := ts.signJWTWithAccess(t, "user-control", "control")

	for _, path := range []string{
		"/api/files/some/path.txt",
		"/api/files-ls?path=/home/vibecraft",
	} {
		// View tier: must be denied with 403 BEFORE the handler runs.
		w := ts.do(t, "GET", path, "Bearer "+viewTok, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s with view tier: want 403, got %d (body=%s)", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Control access required") {
			t.Errorf("%s with view tier: want 'Control access required', got %s", path, w.Body.String())
		}

		// Control tier: must pass the tier gate. It may then 404/400 because
		// the path doesn't exist in the test jail — anything but 403 proves
		// the gate let control through.
		w = ts.do(t, "GET", path, "Bearer "+controlTok, "")
		if w.Code == http.StatusForbidden {
			t.Errorf("%s with control tier: must NOT be 403, got 403 (body=%s)", path, w.Body.String())
		}
	}
}

// newIdempotencyTestServer builds a minimal Server with a real DB-backed
// taskStore — enough for handleTask's create + idempotency path. No budget
// tracker / manager gate, so submissions flow straight to CreateTask.
func newIdempotencyTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := persistence.Open(dir+"/test.db", "test-key-32chars-XXXXXXXXXXXXXXXX")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{
		db:        db,
		taskStore: core.NewTaskStore(db),
		auditLog:  audit.NewLogger(db),
		broker:    NewSSEBroker(),
	}
}

func submitWithKey(srv *Server, convID, key string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{
		"instruction":     "do the thing",
		"conversation_id": convID,
		"idempotency_key": key,
	})
	req := httptest.NewRequest("POST", "/api/task", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyAccess, "control"))
	w := httptest.NewRecorder()
	srv.handleTask(w, req)
	return w
}

// TestHandleTask_ConcurrentSameKeyCreatesOneTask pins the idempotency
// TOCTOU fix: N concurrent POSTs carrying the SAME idempotency key must
// create exactly ONE task. Run with -race to also catch the data race the
// old check-then-set (lock released between lookup and store) exhibited.
//
// Red proof (before the fix): with the lookup and store under separate
// lock acquisitions, multiple goroutines miss the lookup before any
// stores, so CreateTask runs more than once and ListRecent reports >1
// task for the single key — the test fails with "want 1 task, got N".
func TestHandleTask_ConcurrentSameKeyCreatesOneTask(t *testing.T) {
	srv := newIdempotencyTestServer(t)

	// Reset the package-level idempotency cache so a prior test can't leak
	// a stored key into this one.
	idempotencyMu.Lock()
	idempotencyStore = map[string]idempotencyEntry{}
	idempotencyMu.Unlock()

	const n = 16
	const key = "race-key-1"
	const conv = "convo-race"

	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := submitWithKey(srv, conv, key)
			codes[i] = w.Code
			var parsed struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &parsed)
			ids[i] = parsed.ID
		}(i)
	}
	wg.Wait()

	// Exactly one task row must exist for the single key.
	tasks, err := srv.taskStore.ListRecent(100)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("same idempotency key must create exactly one task, got %d", len(tasks))
	}

	// Every successful response must reference that one task id.
	want := tasks[0].ID
	for i := 0; i < n; i++ {
		if (codes[i] == http.StatusOK || codes[i] == http.StatusCreated) && ids[i] != want {
			t.Errorf("response %d returned task id %q, want the single task %q", i, ids[i], want)
		}
	}
}

// TestManagementRevokeKeys_RemovesUsersKeys pins the daemon half of the
// control-revoke fix: POST /api/management/revoke-keys {"userId":...}
// (health-token authed, mirroring /api/management/refresh-jwks) revokes
// every brokered vc_machine_* key owned by that user and ONLY that user,
// and reports the count. After revoke, the user's keys no longer
// authenticate (GetAPIKeyByHash skips revoked rows); another user's key
// is untouched.
func TestManagementRevokeKeys_RemovesUsersKeys(t *testing.T) {
	ts := newTestServer(t)
	db := ts.server.db

	// Seed two keys for the target user and one for a bystander.
	type seed struct {
		id, rawKey, owner string
	}
	seeds := []seed{
		{"k-victim-1", "vc_machine_victimkeyone", "user-victim"},
		{"k-victim-2", "vc_machine_victimkeytwo", "user-victim"},
		{"k-other-1", "vc_machine_otherkeyone", "user-other"},
	}
	for _, s := range seeds {
		hash := sha256Hex(s.rawKey)
		if err := db.CreateAPIKey(s.id, "test", hash, s.rawKey[len(s.rawKey)-4:], s.owner); err != nil {
			t.Fatalf("seed key %s: %v", s.id, err)
		}
	}

	// Sanity: all three resolve before revoke.
	for _, s := range seeds {
		if _, err := db.GetAPIKeyByHash(sha256Hex(s.rawKey)); err != nil {
			t.Fatalf("pre-revoke %s should resolve: %v", s.id, err)
		}
	}

	// Health token must pass; wrong/no token must be rejected (mirrors the
	// refresh-jwks gate). Send through the mux end-to-end.
	body := `{"userId":"user-victim"}`
	if w := postManagement(ts, "/api/management/revoke-keys", "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: want 401, got %d", w.Code)
	}
	if w := postManagement(ts, "/api/management/revoke-keys", "Bearer "+ts.daemonTok, body); w.Code != http.StatusUnauthorized {
		t.Errorf("daemon token: want 401 (re-gated off daemon token), got %d", w.Code)
	}

	w := postManagement(ts, "/api/management/revoke-keys", "Bearer "+ts.healthTok, body)
	if w.Code != http.StatusOK {
		t.Fatalf("health token revoke: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Revoked != 2 {
		t.Errorf("revoked count: want 2, got %d", resp.Revoked)
	}

	// The victim's keys no longer authenticate.
	for _, s := range seeds[:2] {
		if _, err := db.GetAPIKeyByHash(sha256Hex(s.rawKey)); err == nil {
			t.Errorf("post-revoke %s should NOT resolve (was revoked)", s.id)
		}
	}
	// The bystander's key still works.
	if _, err := db.GetAPIKeyByHash(sha256Hex(seeds[2].rawKey)); err != nil {
		t.Errorf("post-revoke %s should still resolve (different owner): %v", seeds[2].id, err)
	}

	// A second revoke for the same user is a clean no-op (0 revoked).
	w = postManagement(ts, "/api/management/revoke-keys", "Bearer "+ts.healthTok, body)
	if w.Code != http.StatusOK {
		t.Fatalf("second revoke: want 200, got %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Revoked != 0 {
		t.Errorf("second revoke count: want 0 (already revoked), got %d", resp.Revoked)
	}
}

// postManagement issues a POST through the full mux with a JSON body.
func postManagement(ts *testServer, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	return w
}

// sha256Hex mirrors how withAuth hashes a presented vc_machine_* key
// (sha256.Sum256 → hex) so seeded keys resolve through GetAPIKeyByHash.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
