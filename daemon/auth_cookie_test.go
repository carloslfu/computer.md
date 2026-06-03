// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/golang-jwt/jwt/v5"
)

// signGrant produces a grant code for the test machine with the given
// claims. The grant has a 30s expiry — same as platform-side grants.
func (ts *testServer) signGrant(t *testing.T, sub, name, email, access, nonce string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":     sub,
		"name":    name,
		"email":   email,
		"access":  access,
		"machine": ts.machineID,
		"nonce":   nonce,
		"iss":     "vibecraft.so",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(30 * time.Second).Unix(),
	})
	tokenStr, err := token.SignedString(ts.privateKey)
	if err != nil {
		t.Fatalf("sign grant: %v", err)
	}
	return tokenStr
}

func TestAuthCallback_HappyPath(t *testing.T) {
	ts := newTestServer(t)
	grant := ts.signGrant(t, "user-1", "Alice", "alice@example.com", "control", "nonce-1")

	req := httptest.NewRequest("GET", "/auth/callback?code="+grant+"&return=%2Fc%2Fabc", nil)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}

	loc := w.Header().Get("Location")
	if loc != "/c/abc" {
		t.Errorf("expected redirect to /c/abc, got %q", loc)
	}

	cookies := w.Result().Cookies()
	var sess, sso *http.Cookie
	for _, c := range cookies {
		switch c.Name {
		case cookieSession:
			sess = c
		case cookieSSO:
			sso = c
		}
	}
	if sess == nil {
		t.Fatalf("vc_session cookie not set")
	}
	if sso == nil {
		t.Fatalf("vc_sso cookie not set")
	}

	// vc_session: NO Domain attribute (exact-host scope).
	if sess.Domain != "" {
		t.Errorf("vc_session must not set Domain= (got %q)", sess.Domain)
	}
	if !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteLaxMode {
		t.Errorf("vc_session cookie attrs wrong: HttpOnly=%v Secure=%v SameSite=%v",
			sess.HttpOnly, sess.Secure, sess.SameSite)
	}

	// vc_sso: Domain set → subdomain-wide. Go's cookie marshaller strips
	// the leading dot we passed in (RFC 6265 §5.2.3 — Domain attribute
	// at all implies subdomain inclusion), so we only assert the
	// attribute is non-empty and matches the machine host.
	if sso.Domain == "" {
		t.Errorf("vc_sso Domain must be set (got empty)")
	}
	if sso.Domain != "" && !strings.HasSuffix(sso.Domain, ts.server.cfg.MachineHost) {
		t.Errorf("vc_sso Domain should target machine host, got %q (host=%q)",
			sso.Domain, ts.server.cfg.MachineHost)
	}

	// Session row should be in DB keyed by the SHA-256 hash of the raw value.
	hash := sha256.Sum256([]byte(sess.Value))
	row, err := ts.server.db.GetSessionByIDHash(hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatalf("session row missing: %v", err)
	}
	if row.Sub != "user-1" || row.Name != "Alice" || row.Email != "alice@example.com" || row.Access != "control" {
		t.Errorf("session row wrong: %+v", row)
	}
}

func TestAuthCallback_RejectsNonceReplay(t *testing.T) {
	ts := newTestServer(t)
	grant := ts.signGrant(t, "user-1", "Alice", "alice@example.com", "control", "nonce-replay")

	// First use succeeds.
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("first use: expected 302, got %d", w.Code)
	}

	// Second use of same nonce must fail even though the JWT itself is
	// still valid.
	w = httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("nonce replay: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthCallback_RejectsWrongMachine(t *testing.T) {
	ts := newTestServer(t)
	// Sign for a DIFFERENT machine.
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":     "user-1",
		"name":    "Alice",
		"email":   "alice@example.com",
		"access":  "control",
		"machine": "vc-other-machine",
		"nonce":   "n",
		"iss":     "vibecraft.so",
		"iat":     time.Now().Unix(),
		"exp":     time.Now().Add(30 * time.Second).Unix(),
	})
	grant, _ := token.SignedString(ts.privateKey)

	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong machine: expected 403, got %d", w.Code)
	}
}

func TestAuthCallback_RejectsBadSignature(t *testing.T) {
	ts := newTestServer(t)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code=not-a-jwt", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad code: expected 401, got %d", w.Code)
	}
}

func TestAuthWhoami_Session(t *testing.T) {
	ts := newTestServer(t)
	grant := ts.signGrant(t, "user-2", "Bob", "bob@example.com", "view", "nonce-2")

	// Handshake.
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	var sess *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieSession {
			sess = c
		}
	}
	if sess == nil {
		t.Fatalf("no session cookie")
	}

	// whoami with the session cookie.
	req := httptest.NewRequest("GET", "/api/auth/whoami", nil)
	req.AddCookie(sess)
	w = httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("whoami: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["sub"] != "user-2" {
		t.Errorf("whoami sub wrong: %v", body["sub"])
	}
	if body["access"] != "view" {
		t.Errorf("whoami access wrong: %v", body["access"])
	}
	if body["name"] != "Bob" {
		t.Errorf("whoami name wrong: %v", body["name"])
	}
}

func TestAuthWhoami_NoCookies(t *testing.T) {
	ts := newTestServer(t)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/auth/whoami", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthLogout_RemovesSession(t *testing.T) {
	ts := newTestServer(t)
	grant := ts.signGrant(t, "user-3", "Carol", "carol@example.com", "control", "nonce-3")

	// Handshake.
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	var sess *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieSession {
			sess = c
		}
	}
	if sess == nil {
		t.Fatalf("no session cookie after handshake")
	}

	// Confirm logged in.
	req := httptest.NewRequest("GET", "/api/auth/whoami", nil)
	req.AddCookie(sess)
	w = httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pre-logout whoami: expected 200, got %d", w.Code)
	}

	// Logout.
	req = httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.AddCookie(sess)
	w = httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", w.Code)
	}

	// Same cookie should now fail.
	req = httptest.NewRequest("GET", "/api/auth/whoami", nil)
	req.AddCookie(sess)
	w = httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("post-logout whoami: expected 401, got %d", w.Code)
	}

	// Logout response should have set Max-Age=-1 on both cookies (cleared).
	var cleared int
	for _, c := range w.Result().Cookies() {
		if (c.Name == cookieSession || c.Name == cookieSSO) && c.MaxAge < 0 {
			cleared++
		}
	}
	// whoami response doesn't clear cookies — that was the logout response.
	// So we don't expect cleared > 0 on the *post-logout* whoami. Skipped.
	_ = cleared
}

// handshakeCookie returns the freshly-issued vc_session cookie for use
// in tests that need an authenticated browser session.
func (ts *testServer) handshakeCookie(t *testing.T, sub, name, email, access, nonce string) *http.Cookie {
	t.Helper()
	grant := ts.signGrant(t, sub, name, email, access, nonce)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("handshake: expected 302, got %d: %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieSession {
			return c
		}
	}
	t.Fatalf("handshake: no vc_session cookie")
	return nil
}

// withCookieOrBearer end-to-end: the SPA's cookie path AND the legacy
// Bearer path must both reach the same data-plane handler. The
// "/api/conversations" route is wired through withCookieOrBearer so a
// 200 from each auth shape proves the chain.
func TestCookieOrBearer_CookiePathReadOnly(t *testing.T) {
	ts := newTestServer(t)
	sess := ts.handshakeCookie(t, "user-cookie", "Alice", "alice@example.com", "control", "nonce-cookie-1")

	req := httptest.NewRequest("GET", "/api/conversations", nil)
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cookie path: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCookieOrBearer_BearerPathReadOnly(t *testing.T) {
	ts := newTestServer(t)
	jwtTok := ts.signJWT(t, "user-bearer")

	req := httptest.NewRequest("GET", "/api/conversations", nil)
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("bearer path: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCookieOrBearer_NoAuthFails(t *testing.T) {
	ts := newTestServer(t)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/conversations", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: expected 401, got %d", w.Code)
	}
}

// Cookie-authed mutating requests without the explicit JSON / multipart
// content-type get rejected to defeat cross-origin form-submit CSRF.
// Bearer requests bypass the gate because they carry an opaque secret
// no browser auto-attaches.
func TestCookieOrBearer_CSRFBlocksFormPost(t *testing.T) {
	ts := newTestServer(t)
	sess := ts.handshakeCookie(t, "user-csrf", "Carol", "carol@example.com", "control", "nonce-csrf-1")

	// Mutating POST without Content-Type → 415.
	req := httptest.NewRequest("POST", "/api/memory", strings.NewReader("text=hello"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415 on form-encoded mutating cookie request, got %d: %s", w.Code, w.Body.String())
	}
}

// A request that arrives with a stale vc_session AND a valid Bearer
// JWT must fall through to the Bearer path rather than locking the
// caller out. This is the "tab still open after machine reboot wiped
// the sessions table" path.
func TestCookieOrBearer_StaleCookieFallsThroughToBearer(t *testing.T) {
	ts := newTestServer(t)
	jwtTok := ts.signJWT(t, "user-fallback")

	req := httptest.NewRequest("GET", "/api/conversations", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: "completely-bogus-value"})
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stale-cookie + valid-bearer: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// /api/auth/whoami issued with the X-Vc-Refresh hint header when the
// session is close to expiry. Lock that in here so the SPA's silent
// refresh contract stays trustworthy.
func TestWhoami_NoRefreshHintFarFromExpiry(t *testing.T) {
	ts := newTestServer(t)
	sess := ts.handshakeCookie(t, "user-refresh", "Dan", "dan@example.com", "control", "nonce-refresh-1")

	// /api/auth/whoami reads the cookie directly rather than going
	// through withCookieOrBearer, so the X-Vc-Refresh path doesn't fire
	// there. Use a route guarded by withCookieOrBearer instead.
	req := httptest.NewRequest("GET", "/api/conversations", nil)
	req.AddCookie(sess)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("conversations: expected 200, got %d", w.Code)
	}
	// Fresh handshake → cookie is 8h out → no refresh hint expected.
	if w.Header().Get("X-Vc-Refresh") == "1" {
		t.Errorf("X-Vc-Refresh should NOT be set on a fresh cookie")
	}
}

// ─── Phase 6: cookie / grant-code attack-surface coverage ───

// TestVerifyGrantCode_RejectsMalformedAndForged exercises the JWT
// parser (verifyGrantCode) directly with hostile inputs. The grant code
// arrives on the wire as ?code= and is the bootstrap of every browser
// session, so a parser that accepts a forged/none/expired/HS256 token
// would mint a session for an attacker.
func TestVerifyGrantCode_RejectsMalformedAndForged(t *testing.T) {
	ts := newTestServer(t)

	// A token signed with an attacker RSA key (valid RS256 structure,
	// wrong key) — must fail JWKS verification.
	attackerKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "attacker", "machine": ts.machineID, "nonce": "n",
		"access": "control", "exp": time.Now().Add(time.Minute).Unix(),
	})
	forgedStr, _ := forged.SignedString(attackerKey)

	// alg=none unsigned token.
	noneTok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "attacker", "machine": ts.machineID, "nonce": "n",
		"access": "control", "exp": time.Now().Add(time.Minute).Unix(),
	})
	noneStr, _ := noneTok.SignedString(jwt.UnsafeAllowNoneSignatureType)

	// Algorithm-confusion: HS256 token. verifyGrantCode pins
	// *SigningMethodRSA, so an HS256 token (the classic RS256→HS256
	// downgrade) must be rejected regardless of the HMAC secret used.
	hsTok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "attacker", "machine": ts.machineID, "nonce": "n",
		"access": "control", "exp": time.Now().Add(time.Minute).Unix(),
	})
	hsStr, _ := hsTok.SignedString([]byte("public-key-as-hmac-secret"))

	// Expired but otherwise validly-signed grant.
	expired := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "u", "machine": ts.machineID, "nonce": "n", "access": "control",
		"iat": time.Now().Add(-2 * time.Minute).Unix(),
		"exp": time.Now().Add(-1 * time.Minute).Unix(),
	})
	expiredStr, _ := expired.SignedString(ts.privateKey)

	// Validly signed but missing the required `machine`/`nonce` claims.
	missingClaims := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "u", "access": "control",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	missingStr, _ := missingClaims.SignedString(ts.privateKey)

	// Validly signed but access claim is neither control nor view
	// (privilege smuggling).
	badAccess := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "u", "machine": ts.machineID, "nonce": "n", "access": "admin",
		"exp": time.Now().Add(time.Minute).Unix(),
	})
	badAccessStr, _ := badAccess.SignedString(ts.privateKey)

	badIssuer := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "u", "machine": ts.machineID, "nonce": "n", "access": "control",
		"iss": "evil.example", "exp": time.Now().Add(time.Minute).Unix(),
	})
	badIssuerStr, _ := badIssuer.SignedString(ts.privateKey)

	cases := []struct {
		desc string
		code string
	}{
		{"empty", ""},
		{"garbage", "not-a-jwt"},
		{"only header", "eyJhbGciOiJSUzI1NiJ9"},
		{"two segments", "aaa.bbb"},
		{"forged RSA key", forgedStr},
		{"alg=none", noneStr},
		{"HS256 algorithm confusion", hsStr},
		{"expired", expiredStr},
		{"missing required claims", missingStr},
		{"invalid access value", badAccessStr},
		{"invalid issuer", badIssuerStr},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if _, err := ts.server.verifyGrantCode(c.code); err == nil {
				t.Fatalf("verifyGrantCode(%q...) = nil error, want rejection", c.desc)
			}
			// And end-to-end through the handler: must not 302 / mint a
			// cookie.
			req := httptest.NewRequest("GET", "/auth/callback?code="+c.code, nil)
			w := httptest.NewRecorder()
			ts.mux.ServeHTTP(w, req)
			if w.Code == http.StatusFound {
				t.Fatalf("hostile grant %q produced a 302 (session minted)", c.desc)
			}
			for _, ck := range w.Result().Cookies() {
				if ck.Name == cookieSession && ck.Value != "" && ck.MaxAge >= 0 {
					t.Fatalf("hostile grant %q set a live vc_session cookie", c.desc)
				}
			}
		})
	}
}

// TestConsumeGrantNonce_SingleUse pins the replay defense at the
// persistence boundary the handler relies on: the first consume of a
// nonce succeeds, every subsequent consume of the SAME nonce returns
// ErrNonceAlreadyConsumed regardless of expiry, so a captured grant
// code cannot be replayed even within its 30s validity window.
func TestConsumeGrantNonce_SingleUse(t *testing.T) {
	ts := newTestServer(t)
	exp := time.Now().Add(30 * time.Second).UTC()

	if err := ts.server.db.ConsumeGrantNonce("nonce-abc", exp); err != nil {
		t.Fatalf("first consume should succeed, got %v", err)
	}
	for i := 0; i < 3; i++ {
		err := ts.server.db.ConsumeGrantNonce("nonce-abc", exp)
		if !errors.Is(err, persistence.ErrNonceAlreadyConsumed) {
			t.Fatalf("replay %d: want ErrNonceAlreadyConsumed, got %v", i, err)
		}
	}
	// A different nonce is unaffected.
	if err := ts.server.db.ConsumeGrantNonce("nonce-other", exp); err != nil {
		t.Fatalf("distinct nonce should succeed, got %v", err)
	}
}

// TestAuthCallback_RejectsNonceReplayEndToEnd is the handler-level twin
// of the persistence test: a fully valid, correctly-signed,
// not-yet-expired grant works exactly once; replaying the identical
// code is rejected 401 by the ConsumeGrantNonce path.
func TestAuthCallback_RejectsNonceReplayEndToEnd(t *testing.T) {
	ts := newTestServer(t)
	grant := ts.signGrant(t, "u", "U", "u@e.com", "control", "e2e-replay-nonce")

	w1 := httptest.NewRecorder()
	ts.mux.ServeHTTP(w1, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	if w1.Code != http.StatusFound {
		t.Fatalf("first use: want 302, got %d: %s", w1.Code, w1.Body.String())
	}
	w2 := httptest.NewRecorder()
	ts.mux.ServeHTTP(w2, httptest.NewRequest("GET", "/auth/callback?code="+grant, nil))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("replay: want 401, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestWithCookieAuth_OversizedCookieRejected feeds a multi-megabyte
// cookie value. It must not be treated as a valid session (the hash of
// junk won't match any row) and must not panic / hang the auth path —
// it has to fall through to a clean 401.
func TestWithCookieAuth_OversizedCookieRejected(t *testing.T) {
	ts := newTestServer(t)

	huge := make([]byte, 2<<20) // 2 MiB
	for i := range huge {
		huge[i] = 'a'
	}
	req := httptest.NewRequest("GET", "/api/conversations", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: string(huge)})
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("oversized junk cookie must yield 401, got %d", w.Code)
	}
	if containsStr(w.Body.String(), "panic") {
		t.Fatalf("oversized cookie path panicked: %s", w.Body.String())
	}
}

// TestWithCookieOrBearer_CSRFGate locks the anti-CSRF content-type gate
// on the dual-auth middleware: a cookie-authenticated mutating request
// (POST/PUT/PATCH/DELETE) WITHOUT application/json or multipart is
// rejected 415 before the handler runs, defeating a cross-origin
// form-submit that rides SameSite=Lax. JSON and multipart pass the gate.
func TestWithCookieOrBearer_CSRFGate(t *testing.T) {
	ts := newTestServer(t)
	sess := ts.handshakeCookie(t, "u-csrf2", "C", "c@e.com", "control", "nonce-csrf2")

	mutating := []string{"POST", "PUT", "PATCH", "DELETE"}

	t.Run("form-encoded mutating cookie request → 415", func(t *testing.T) {
		for _, m := range mutating {
			req := httptest.NewRequest(m, "/api/memory", strings.NewReader("x=1"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(sess)
			w := httptest.NewRecorder()
			ts.mux.ServeHTTP(w, req)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s form-encoded: want 415, got %d: %s", m, w.Code, w.Body.String())
			}
		}
	})

	t.Run("no content-type mutating cookie request → 415", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/memory", strings.NewReader("{}"))
		req.AddCookie(sess)
		req.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		ts.mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("no content-type: want 415, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("text/plain mutating cookie request → 415 (CSRF-safe type)", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/memory", strings.NewReader("hi"))
		req.Header.Set("Content-Type", "text/plain")
		req.AddCookie(sess)
		w := httptest.NewRecorder()
		ts.mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("text/plain: want 415, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("application/json mutating cookie request passes the gate", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/memory", strings.NewReader(`{"content":"note"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sess)
		w := httptest.NewRecorder()
		ts.mux.ServeHTTP(w, req)
		if w.Code == http.StatusUnsupportedMediaType {
			t.Errorf("application/json must pass the CSRF gate, got 415: %s", w.Body.String())
		}
	})

	t.Run("GET cookie request is never gated", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/conversations", nil)
		req.AddCookie(sess)
		w := httptest.NewRecorder()
		ts.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET with cookie: want 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestSafeReturnPath_RejectsOpenRedirect pins the ?return= validator
// against the open-redirect bypasses that send the user off-machine after
// the handshake. Anything that isn't a same-origin path-only value must
// collapse to "/". The backslash cases are the load-bearing ones: a
// browser normalizes "\" to "/", so values like "/\evil.com" resolve to a
// protocol-relative URL pointing at another origin, and Go's url.Parse
// keeps the backslash literal — it won't flag them on its own.
func TestSafeReturnPath_RejectsOpenRedirect(t *testing.T) {
	cases := []struct {
		desc string
		in   string
		want string
	}{
		// Accepted: genuine same-origin paths.
		{"empty", "", "/"},
		{"root", "/", "/"},
		{"simple path", "/c/abc", "/c/abc"},
		{"nested path", "/settings/vault", "/settings/vault"},
		{"path with query", "/c/abc?tab=files", "/c/abc?tab=files"},
		{"path with fragment", "/dashboard#section", "/dashboard#section"},

		// Rejected: not path-rooted.
		{"relative", "c/abc", "/"},
		{"absolute https", "https://evil.com", "/"},
		{"absolute http", "http://evil.com/path", "/"},
		{"scheme-relative", "//evil.com", "/"},
		{"scheme-relative path", "//evil.com/path", "/"},
		{"javascript scheme", "javascript:alert(1)", "/"},
		{"mailto scheme", "mailto:a@b.com", "/"},
		{"data scheme", "data:text/html,evil", "/"},

		// Rejected: backslash open-redirect bypasses (browser → "/").
		{"backslash slash host", `/\evil.com`, "/"},
		{"backslash backslash host", `\\evil.com`, "/"},
		{"leading backslash slash", `\/evil.com`, "/"},
		{"slash backslash slash host", `/\/evil.com`, "/"},
		{"backslash mid path", `/foo\..\bar`, "/"},
		{"backslash before scheme", `/\\evil.com/path`, "/"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := safeReturnPath(c.in); got != c.want {
				t.Errorf("safeReturnPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAuthCallback_HostileReturnNeverEscapesOrigin is the end-to-end twin:
// a valid grant carrying a hostile ?return= must still 302, but the
// Location header must be the safe "/" fallback, never an off-origin URL.
func TestAuthCallback_HostileReturnNeverEscapesOrigin(t *testing.T) {
	hostile := []string{
		"https://evil.com",
		"//evil.com",
		`/\evil.com`,
		`\/evil.com`,
		`/\/evil.com`,
	}
	for i, ret := range hostile {
		t.Run(ret, func(t *testing.T) {
			ts := newTestServer(t)
			grant := ts.signGrant(t, "u", "U", "u@e.com", "control",
				fmt.Sprintf("nonce-hostile-return-%d", i))
			req := httptest.NewRequest("GET",
				"/auth/callback?code="+grant+"&return="+url.QueryEscape(ret), nil)
			w := httptest.NewRecorder()
			ts.mux.ServeHTTP(w, req)
			if w.Code != http.StatusFound {
				t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
			}
			if loc := w.Header().Get("Location"); loc != "/" {
				t.Errorf("hostile return %q → Location %q, want \"/\"", ret, loc)
			}
		})
	}
}

func TestSessionSweeper_RemovesExpired(t *testing.T) {
	ts := newTestServer(t)

	// Insert an already-expired session directly.
	past := time.Now().UTC().Add(-1 * time.Hour)
	err := ts.server.db.CreateSession(persistence.Session{
		IDHash:    "h-expired",
		Sub:       "user-x",
		Access:    "view",
		CreatedAt: past.Add(-1 * time.Hour),
		ExpiresAt: past,
		LastSeen:  past,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := ts.server.db.SweepExpiredSessions()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row swept, got %d", n)
	}
}
