// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/core"
	"github.com/carloslfu/computer.md/daemon/jwks"
	"github.com/carloslfu/computer.md/daemon/memory"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// testServer sets up a daemon Server with real DB, real auth middleware,
// and real route handlers for /health, /status, /keys, /management/*.
type testServer struct {
	server     *Server
	mux        *http.ServeMux
	privateKey *rsa.PrivateKey
	healthTok  string
	daemonTok  string
	localTok   string
	machineID  string
}

// lauth is the Authorization header a legitimate in-shell loopback
// caller presents: the per-machine local token (a boot invariant in
// production, so the harness always has one). Loopback data-plane tests
// that assert method/body behavior pass this; token-negative tests in
// local_token_test.go construct headers explicitly instead.
func (ts *testServer) lauth() map[string]string {
	return map[string]string{"Authorization": "Bearer " + ts.server.cfg.LocalToken}
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	// Generate RSA key pair for JWT signing/verification.
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	// Create temp dir for SQLCipher DB.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Open encrypted DB with a test key.
	encKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	db, err := persistence.Open(dbPath, encKey)
	if err != nil {
		t.Fatalf("opening test DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	healthTok := "test-health-token-64chars-abcdef1234567890abcdef1234567890abcd"
	daemonTok := "test-daemon-token-64chars-abcdef1234567890abcdef1234567890abcd"
	localTok := "test-local-token-64chars-abcdef1234567890abcdef1234567890abcd"
	machineID := "vc-test123"

	inboxDir := filepath.Join(tmpDir, "inbox")
	if err := os.MkdirAll(inboxDir, 0700); err != nil {
		t.Fatalf("creating inbox dir: %v", err)
	}

	cfg := &Config{
		Port:         0,
		DaemonToken:  daemonTok,
		HealthToken:  healthTok,
		LocalToken:   localTok,
		JWTPublicKey: &privKey.PublicKey,
		MachineID:    machineID,
		MachineHost:  machineID + ".vc.vibecraft.so",
		InboxDir:     inboxDir,
	}

	taskStore := core.NewTaskStore(db)
	auditLog := audit.NewLogger(db)
	memStore := memory.NewStore(db)

	vaultPath := filepath.Join(tmpDir, "vault.enc")
	var vaultKey [32]byte
	copy(vaultKey[:], []byte("01234567890123456789012345678901"))
	vaultStore, err := vault.NewStore(vaultPath, vaultKey)
	if err != nil {
		t.Fatalf("opening vault: %v", err)
	}

	srv := &Server{
		cfg:        cfg,
		db:         db,
		taskStore:  taskStore,
		auditLog:   auditLog,
		memStore:   memStore,
		vaultStore: vaultStore,
		// Use a no-op JWKS client whose only entry is the test pubkey
		// under kid="vibecraft-1"; tests sign without an explicit kid,
		// so the daemon falls back to vibecraft-1.
		jwks:      jwks.NewClient("http://127.0.0.1:1/jwks", "vibecraft-1", &privKey.PublicKey),
		startTime: time.Now(),
	}

	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	return &testServer{
		server:     srv,
		mux:        mux,
		privateKey: privKey,
		healthTok:  healthTok,
		daemonTok:  daemonTok,
		localTok:   localTok,
		machineID:  machineID,
	}
}

// signJWT creates a valid JWT for testing.
func (ts *testServer) signJWT(t *testing.T, sub string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":     sub,
		"machine": ts.machineID,
		"iss":     "vibecraft.so",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(5 * time.Minute).Unix(),
	})
	tokenStr, err := token.SignedString(ts.privateKey)
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	return tokenStr
}

// signJWTWrongMachine creates a JWT for a different machine.
func (ts *testServer) signJWTWrongMachine(t *testing.T) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":     "user-123",
		"machine": "vc-wrong",
		"iss":     "vibecraft.so",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(5 * time.Minute).Unix(),
	})
	tokenStr, err := token.SignedString(ts.privateKey)
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	return tokenStr
}

func (ts *testServer) do(t *testing.T, method, path, authHeader, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	// D-7 retired the legacy un-namespaced data routes — everything
	// lives under /api/* now. Normalize bare data paths here so the
	// many existing call sites keep exercising the real routes without
	// a mass rewrite. /health (cron contract), /routes/verify (Caddy
	// contract), and the /auth/* handshake stay on their bare paths;
	// already-/api/ paths pass through.
	if path == "/health" ||
		strings.HasPrefix(path, "/api/") ||
		strings.HasPrefix(path, "/auth/") ||
		strings.HasPrefix(path, "/routes/verify") {
		// leave as-is
	} else {
		path = "/api" + path
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	return w
}

// ─── Test: Health endpoint auth (health token only) ───

func TestHealthAcceptsHealthToken(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/health", "Bearer "+ts.healthTok, "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHealthRejectsNoAuth(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/health", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHealthRejectsAPIKey(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/health", "Bearer vc_machine_fakekey1234567890", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHealthRejectsJWT(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")
	w := ts.do(t, "GET", "/health", "Bearer "+jwtToken, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHealthRejectsDaemonToken(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/health", "Bearer "+ts.daemonTok, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHealthResponseStripped(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/health", "Bearer "+ts.healthTok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Must have only these three fields.
	allowed := map[string]bool{"status": true, "uptime": true, "timestamp": true}
	for key := range resp {
		if !allowed[key] {
			t.Errorf("health response contains unexpected field %q (violates zero-knowledge)", key)
		}
	}
	for key := range allowed {
		if _, ok := resp[key]; !ok {
			t.Errorf("health response missing expected field %q", key)
		}
	}
}

// ─── Test: Status endpoint auth (JWT or API key) ───

func TestStatusAcceptsJWT(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")
	w := ts.do(t, "GET", "/status", "Bearer "+jwtToken, "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStatusRejectsHealthToken(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/status", "Bearer "+ts.healthTok, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStatusRejectsDaemonToken(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/status", "Bearer "+ts.daemonTok, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStatusRejectsWrongMachineJWT(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWTWrongMachine(t)
	w := ts.do(t, "GET", "/status", "Bearer "+jwtToken, "")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStatusReturnsMachineID(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")
	w := ts.do(t, "GET", "/status", "Bearer "+jwtToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp["machine_id"] != ts.machineID {
		t.Errorf("expected machine_id=%q, got %q", ts.machineID, resp["machine_id"])
	}
}

// ─── Test: Keys endpoint auth (JWT only) ───

func TestKeysRejectsNoAuth(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/keys", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestKeysRejectsAPIKey(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/keys", "Bearer vc_machine_fakekey1234567890", "")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestKeysAcceptsJWT(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")
	w := ts.do(t, "GET", "/keys", "Bearer "+jwtToken, "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── Test: Management endpoint auth (daemon token only) ───

func TestManagementAcceptsDaemonToken(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/management/usage", "Bearer "+ts.daemonTok, "")
	// May return 200 or 500 depending on handler deps, but NOT 401.
	if w.Code == http.StatusUnauthorized {
		t.Errorf("expected daemon token to be accepted, got 401")
	}
}

func TestManagementRejectsJWT(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")
	w := ts.do(t, "GET", "/management/usage", "Bearer "+jwtToken, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestManagementRejectsAPIKey(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/management/usage", "Bearer vc_machine_fakekey1234567890", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestManagementRejectsHealthToken(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, "GET", "/management/usage", "Bearer "+ts.healthTok, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ─── Test: API key lifecycle (create → use → revoke → rejected) ───

func TestAPIKeyLifecycle(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")

	// Step 1: Create an API key via POST /keys (requires JWT).
	w := ts.do(t, "POST", "/keys", "Bearer "+jwtToken, `{"name":"test key"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create key: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var createResp struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("decoding create response: %v", err)
	}

	if !strings.HasPrefix(createResp.Key, "vc_machine_") {
		t.Errorf("key should start with vc_machine_, got %q", createResp.Key)
	}
	if createResp.ID == "" {
		t.Error("key ID should not be empty")
	}
	if createResp.Hint == "" {
		t.Error("key hint should not be empty")
	}

	// Step 2: Use the API key to access /status.
	w = ts.do(t, "GET", "/status", "Bearer "+createResp.Key, "")
	if w.Code != http.StatusOK {
		t.Errorf("use key on /status: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Step 3: Use the API key to access /tasks (list).
	w = ts.do(t, "GET", "/tasks", "Bearer "+createResp.Key, "")
	if w.Code != http.StatusOK {
		t.Errorf("use key on /tasks: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Step 4: Verify the key appears in the list.
	w = ts.do(t, "GET", "/keys", "Bearer "+jwtToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list keys: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), createResp.ID) {
		t.Error("created key not found in list")
	}
	// List should NOT contain the raw key or hash.
	if strings.Contains(w.Body.String(), createResp.Key) {
		t.Error("key list should not expose the raw key")
	}

	// Step 5: Revoke the key.
	w = ts.do(t, "DELETE", "/keys/"+createResp.ID, "Bearer "+jwtToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("revoke key: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Step 6: Revoked key should be rejected.
	w = ts.do(t, "GET", "/status", "Bearer "+createResp.Key, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("revoked key should be rejected, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIKeyCreateStoresCreatedBy(t *testing.T) {
	ts := newTestServer(t)
	userID := "user-456"
	jwtToken := ts.signJWT(t, userID)

	w := ts.do(t, "POST", "/keys", "Bearer "+jwtToken, `{"name":"audit test"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}

	// List keys and check created_by.
	w = ts.do(t, "GET", "/keys", "Bearer "+jwtToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if !strings.Contains(w.Body.String(), userID) {
		t.Errorf("key list should contain created_by=%q", userID)
	}
}

func TestAPIKeyHashValidation(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")

	// Create a key.
	w := ts.do(t, "POST", "/keys", "Bearer "+jwtToken, `{"name":"hash test"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}

	var createResp struct {
		Key string `json:"key"`
	}
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Verify: correct key works.
	w = ts.do(t, "GET", "/status", "Bearer "+createResp.Key, "")
	if w.Code != http.StatusOK {
		t.Errorf("valid key should work, got %d", w.Code)
	}

	// Verify: tampered key fails (change last character).
	tampered := createResp.Key[:len(createResp.Key)-1] + "X"
	w = ts.do(t, "GET", "/status", "Bearer "+tampered, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("tampered key should fail, got %d", w.Code)
	}
}

// ─── Test: First-boot secret generation ───

func TestFirstBootGeneratesDaemonToken(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "daemon.token")

	// File doesn't exist yet.
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatal("token file should not exist before first boot")
	}

	if err := ensureRandomHexFile(tokenPath, 32); err != nil {
		t.Fatalf("ensureRandomHexFile: %v", err)
	}

	// File should now exist with 64 hex chars.
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}
	if len(data) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(data))
	}

	// Verify it's valid hex.
	if _, err := hex.DecodeString(string(data)); err != nil {
		t.Errorf("token is not valid hex: %v", err)
	}

	// Second call should NOT regenerate.
	original := string(data)
	if err := ensureRandomHexFile(tokenPath, 32); err != nil {
		t.Fatalf("second call: %v", err)
	}
	data2, _ := os.ReadFile(tokenPath)
	if string(data2) != original {
		t.Error("token was regenerated on second call (should be idempotent)")
	}
}

func TestFirstBootGeneratesEncryptionKey(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "encryption.key")

	if err := ensureRandomBytesFile(keyPath, 32); err != nil {
		t.Fatalf("ensureRandomBytesFile: %v", err)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading key file: %v", err)
	}
	if len(data) != 32 {
		t.Errorf("expected 32 raw bytes, got %d", len(data))
	}

	// Second call should NOT regenerate.
	original := fmt.Sprintf("%x", data)
	if err := ensureRandomBytesFile(keyPath, 32); err != nil {
		t.Fatalf("second call: %v", err)
	}
	data2, _ := os.ReadFile(keyPath)
	if fmt.Sprintf("%x", data2) != original {
		t.Error("key was regenerated on second call (should be idempotent)")
	}

	// Verify file permissions are 0600.
	info, _ := os.Stat(keyPath)
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected permissions 0600, got %o", perm)
	}
}

// ─── Test: API key hash is SHA-256 ───

func TestAPIKeyHashIsSHA256(t *testing.T) {
	ts := newTestServer(t)
	jwtToken := ts.signJWT(t, "user-123")

	w := ts.do(t, "POST", "/keys", "Bearer "+jwtToken, `{"name":"sha test"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}

	var resp struct {
		Key string `json:"key"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Manually compute SHA-256 and look it up.
	hash := sha256.Sum256([]byte(resp.Key))
	expectedHash := hex.EncodeToString(hash[:])

	key, err := ts.server.db.GetAPIKeyByHash(expectedHash)
	if err != nil {
		t.Fatalf("key lookup by hash failed: %v", err)
	}
	if key == nil {
		t.Fatal("key not found by SHA-256 hash")
	}
}
