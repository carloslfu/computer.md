// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/golang-jwt/jwt/v5"
)

// Cookie names.
const (
	cookieSession = "vc_session"
	cookieSSO     = "vc_sso"
)

// Cookie lifetime — 8h matches AWS Console / Stripe for sensitive
// surfaces. The SPA silent-refresh kicks in at <1h remaining so the
// user never experiences the expiry.
const sessionLifetime = 8 * time.Hour

// refreshHintWindow is the slack zone before expires_at where the
// daemon sets X-Vc-Refresh: 1 on responses so the SPA can refresh in
// the background.
const refreshHintWindow = 1 * time.Hour

// Context keys for cookie-derived claims.
type cookieCtxKey string

const (
	ctxKeyCookieSub    cookieCtxKey = "cookieSub"
	ctxKeyCookieName   cookieCtxKey = "cookieName"
	ctxKeyCookieEmail  cookieCtxKey = "cookieEmail"
	ctxKeyCookieAccess cookieCtxKey = "cookieAccess"
	ctxKeyCookieIDHash cookieCtxKey = "cookieIDHash"
	ctxKeyCookieExpiry cookieCtxKey = "cookieExpiry"
)

// hashSessionID returns SHA-256 hex of the raw cookie value. The DB
// keys on this so a DB compromise can't exfiltrate live cookies.
func hashSessionID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// generateSessionID returns 32 random bytes hex-encoded — the value
// that travels in the cookie. Use crypto/rand for unguessability.
func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session id rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// withCookieOrBearer is the dual-auth path the SPA + legacy ChatLayout
// both flow through during the rollout window. It tries the vc_session
// cookie first; if absent or unrecognized it falls through to the
// existing Bearer (JWT or API key) middleware.
//
// Once D-7 retires the legacy ChatLayout the Bearer branch can drop on
// the routes the SPA owns; until then dual-auth is the way both worlds
// hit the same handler.
//
// CSRF gate: mutating cookie requests must declare an explicit content
// type (json or multipart). Bearer requests are not subject to the
// gate — they come from CLI/programmatic clients and carry an opaque
// secret that cross-site browsers can't replay.
func (s *Server) withCookieOrBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cookie path.
		if c, err := r.Cookie(cookieSession); err == nil && c.Value != "" {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				ct := r.Header.Get("Content-Type")
				if !strings.HasPrefix(ct, "application/json") &&
					!strings.HasPrefix(ct, "multipart/form-data") {
					jsonError(w, "mutating requests require application/json or multipart/form-data", http.StatusUnsupportedMediaType)
					return
				}
			}
			idHash := hashSessionID(c.Value)
			sess, err := s.db.GetSessionByIDHash(idHash)
			if err == nil {
				go s.db.TouchSession(idHash)
				if time.Until(sess.ExpiresAt) < refreshHintWindow {
					w.Header().Set("X-Vc-Refresh", "1")
				}
				ctx := r.Context()
				ctx = context.WithValue(ctx, ctxKeyCookieSub, sess.Sub)
				ctx = context.WithValue(ctx, ctxKeyCookieName, sess.Name)
				ctx = context.WithValue(ctx, ctxKeyCookieEmail, sess.Email)
				ctx = context.WithValue(ctx, ctxKeyCookieAccess, sess.Access)
				ctx = context.WithValue(ctx, ctxKeyCookieIDHash, idHash)
				ctx = context.WithValue(ctx, ctxKeyCookieExpiry, sess.ExpiresAt)
				ctx = context.WithValue(ctx, ctxKeyUserID, sess.Sub)
				next(w, r.WithContext(ctx))
				return
			}
			// Cookie present but unrecognized — clear it so the next
			// request doesn't keep tripping the same lookup, then fall
			// through to the Bearer path. The SPA's 401 → silent-refresh
			// flow will reissue from there.
			clearCookie(w, cookieSession, "")
		}

		// Bearer path. If the caller sent neither a cookie nor an
		// Authorization header, the legacy middleware will reply 401.
		if r.Header.Get("Authorization") == "" {
			jsonError(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		s.withAuth(next).ServeHTTP(w, r)
	}
}

// handleAuthCallback completes the platform→daemon handshake:
//   - Verifies the grant code JWT via JWKS (Workstream A).
//   - Checks for single-use replay against grant_nonces.
//   - Issues both the host-scoped vc_session cookie and the
//     subdomain-wide vc_sso cookie (Workstream H), pointing at parallel
//     daemon-stored rows so SSO can be revoked independently.
//   - Redirects to the ?return= path or /.
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		jsonError(w, "code is required", http.StatusBadRequest)
		return
	}
	returnTo := safeReturnPath(r.URL.Query().Get("return"))

	claims, err := s.verifyGrantCode(code)
	if err != nil {
		log.Printf("auth/callback: bad grant: %v", err)
		jsonError(w, "invalid grant", http.StatusUnauthorized)
		return
	}

	// Tie the grant to this machine. Same check the legacy JWT path runs.
	if claims.Machine != s.cfg.MachineID {
		jsonError(w, "grant not valid for this machine", http.StatusForbidden)
		return
	}

	// Single-use nonce check — atomic insert; UNIQUE constraint catches
	// replays even across daemon restarts (the row sticks around for the
	// 30s grant lifetime).
	if err := s.db.ConsumeGrantNonce(claims.Nonce, claims.exp()); err != nil {
		if errors.Is(err, persistence.ErrNonceAlreadyConsumed) {
			log.Printf("auth/callback: replayed nonce %s sub=%s", claims.Nonce, claims.Sub)
			defaultMetrics.GrantNonceReplay()
			jsonError(w, "grant already consumed", http.StatusUnauthorized)
			return
		}
		log.Printf("auth/callback: nonce store: %v", err)
		jsonError(w, "auth backend error", http.StatusInternalServerError)
		return
	}
	defaultMetrics.GrantConsumed()

	now := time.Now().UTC()
	expiresAt := now.Add(sessionLifetime)
	ua := r.UserAgent()
	ip := remoteIP(r)

	// Browser session — exact-host cookie. JSON body / SPA bootstrap
	// reads this on every API call.
	rawSession, err := generateSessionID()
	if err != nil {
		jsonError(w, "session id generation failed", http.StatusInternalServerError)
		return
	}
	sessRow := persistence.Session{
		IDHash:    hashSessionID(rawSession),
		Sub:       claims.Sub,
		Name:      claims.Name,
		Email:     claims.Email,
		Access:    claims.Access,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		LastSeen:  now,
		IP:        ip,
		UserAgent: ua,
	}
	if err := s.db.CreateSession(sessRow); err != nil {
		log.Printf("auth/callback: create session: %v", err)
		jsonError(w, "session create failed", http.StatusInternalServerError)
		return
	}

	// SSO session — subdomain-wide cookie. Hosted apps under
	// *.vc-xxx… read this via their backend → localhost:8420/api/auth/
	// whoami (Workstream H).
	rawSSO, err := generateSessionID()
	if err != nil {
		jsonError(w, "sso id generation failed", http.StatusInternalServerError)
		return
	}
	if err := s.db.CreateSSOSession(persistence.Session{
		IDHash:    hashSessionID(rawSSO),
		Sub:       claims.Sub,
		Name:      claims.Name,
		Email:     claims.Email,
		Access:    claims.Access,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		LastSeen:  now,
		IP:        ip,
		UserAgent: ua,
	}); err != nil {
		log.Printf("auth/callback: create sso session: %v", err)
		// Don't fail the whole handshake; the chat works without SSO.
		// Accepted: we still set the vc_sso cookie below even though no
		// matching sso_session row exists. The cookie is harmless without
		// its row — handleAuthWhoami looks the hash up and simply misses,
		// so a hosted app sees a clean 401 (anonymous) rather than a stale
		// identity. Clearing/branching here would only add code to produce
		// the same observable result.
	}

	// vc_session: exact-host cookie. NO Domain= attribute → not sent to
	// subdomains. If a customer hosted app ran XHR back to the chat
	// host, the cookie WOULD travel (same registrable origin), but
	// SameSite=Lax blocks cross-site requests, and we don't expose
	// chat APIs to subdomains.
	http.SetCookie(w, &http.Cookie{
		Name:     cookieSession,
		Value:    rawSession,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionLifetime / time.Second),
	})

	// vc_sso: subdomain-wide via Domain attribute with leading dot.
	// Apps at <name>.<machine-host> automatically receive this cookie
	// in browser requests; their backend calls whoami with it.
	http.SetCookie(w, &http.Cookie{
		Name:     cookieSSO,
		Value:    rawSSO,
		Path:     "/",
		Domain:   "." + s.cfg.MachineHost,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionLifetime / time.Second),
	})

	s.auditLog.Log(audit.Entry{
		Action:   "session_created",
		Category: "security",
		UserID:   claims.Sub,
		Details:  fmt.Sprintf("access=%s ua=%q", claims.Access, ua),
	})

	// 302 to the in-app return path. If the call was an XHR refresh
	// (Accept: application/json), return a small JSON ack instead so
	// the SPA can dispatch a re-fetch without window.location.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		jsonResponse(w, http.StatusOK, map[string]any{
			"ok":         true,
			"sub":        claims.Sub,
			"access":     claims.Access,
			"name":       claims.Name,
			"expires_at": expiresAt,
		})
		return
	}
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// handleAuthWhoami returns identity for the cookie holder. Used by:
//   - The SPA on bootstrap, to confirm the session and render
//     "Signed in as X."
//   - Internal hosted apps (Workstream H), which read vc_sso from
//     incoming request cookies and call this server-side to learn the
//     operator's identity.
//
// Accepts either vc_session OR vc_sso. Looks up whichever the request
// presents. Returns 401 if neither cookie is valid.
func (s *Server) handleAuthWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if c, err := r.Cookie(cookieSSO); err == nil && c.Value != "" {
		if sess, err := s.db.GetSSOSessionByIDHash(hashSessionID(c.Value)); err == nil {
			jsonResponse(w, http.StatusOK, map[string]any{
				"sub":        sess.Sub,
				"access":     sess.Access,
				"name":       sess.Name,
				"email":      sess.Email,
				"expires_at": sess.ExpiresAt,
			})
			return
		}
	}
	if c, err := r.Cookie(cookieSession); err == nil && c.Value != "" {
		if sess, err := s.db.GetSessionByIDHash(hashSessionID(c.Value)); err == nil {
			jsonResponse(w, http.StatusOK, map[string]any{
				"sub":        sess.Sub,
				"access":     sess.Access,
				"name":       sess.Name,
				"email":      sess.Email,
				"expires_at": sess.ExpiresAt,
			})
			return
		}
	}

	jsonError(w, "not authenticated", http.StatusUnauthorized)
}

// handleAuthLogout deletes the cookie-keyed session rows and clears
// both cookies. The SPA navigates to the platform lobby after success.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if c, err := r.Cookie(cookieSession); err == nil && c.Value != "" {
		_ = s.db.DeleteSession(hashSessionID(c.Value))
	}
	if c, err := r.Cookie(cookieSSO); err == nil && c.Value != "" {
		_ = s.db.DeleteSSOSession(hashSessionID(c.Value))
	}

	clearCookie(w, cookieSession, "")
	clearCookie(w, cookieSSO, "."+s.cfg.MachineHost)

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func clearCookie(w http.ResponseWriter, name, domain string) {
	c := &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
	if domain != "" {
		c.Domain = domain
	}
	http.SetCookie(w, c)
}

// startSessionSweeper runs an hourly cleanup goroutine for expired
// sessions / sso sessions / grant nonces. The work is cheap and
// idempotent; no cron / external scheduler needed.
func (s *Server) startSessionSweeper(ctx context.Context) {
	go func() {
		// Initial run after a short delay so startup isn't I/O bound.
		t := time.NewTimer(1 * time.Minute)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := s.db.SweepExpiredSessions(); err != nil {
					log.Printf("session sweep: %v", err)
				} else if n > 0 {
					log.Printf("session sweep: removed %d expired rows", n)
				}
				t.Reset(1 * time.Hour)
			}
		}
	}()
}

// grantClaims captures the subset of JWT claims a grant code carries.
type grantClaims struct {
	Sub     string
	Name    string
	Email   string
	Access  string
	Machine string
	Nonce   string
	Exp     int64
}

func (g grantClaims) exp() time.Time {
	return time.Unix(g.Exp, 0).UTC()
}

func (s *Server) verifyGrantCode(tokenStr string) (*grantClaims, error) {
	if s.jwks == nil {
		return nil, fmt.Errorf("jwks not configured")
	}
	tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			kid = "vibecraft-1"
		}
		return s.jwks.KeyFor(kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(jwtExpectedIssuer),
	)
	if err != nil || !tok.Valid {
		return nil, fmt.Errorf("verify: %w", err)
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("claims not map")
	}
	g := &grantClaims{
		Sub:     stringClaim(mc, "sub"),
		Name:    stringClaim(mc, "name"),
		Email:   stringClaim(mc, "email"),
		Access:  stringClaim(mc, "access"),
		Machine: stringClaim(mc, "machine"),
		Nonce:   stringClaim(mc, "nonce"),
	}
	if expF, ok := mc["exp"].(float64); ok {
		g.Exp = int64(expF)
	}
	if g.Sub == "" || g.Machine == "" || g.Nonce == "" || g.Exp == 0 {
		return nil, fmt.Errorf("grant missing required claims")
	}
	if g.Access != "control" && g.Access != "view" {
		return nil, fmt.Errorf("grant access must be control|view")
	}
	return g, nil
}

func stringClaim(m jwt.MapClaims, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// safeReturnPath validates the ?return= param so an attacker can't
// redirect the user off-machine via a maliciously crafted handshake URL.
// Only same-origin, path-only values are accepted; anything that could
// resolve to a different origin falls back to "/".
//
// Defense layers, in order:
//   - Must be non-empty and start with a single "/" (path-rooted).
//   - No backslashes. Browsers normalize "\" to "/", so "/\evil.com" and
//     "\/evil.com" are treated as protocol-relative URLs pointing
//     off-host. url.Parse does NOT catch these (Go keeps "\" literal), so
//     we reject them before parsing.
//   - No "//" or "/\" prefix (protocol-relative URL → other origin).
//   - After parsing, the URL must carry no Scheme and no Host (and no
//     opaque part). That rules out "https://evil.com", "javascript:…",
//     and "mailto:…" style values that slipped past the prefix checks.
func safeReturnPath(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "/"
	}
	// Backslashes are the classic open-redirect bypass: a browser reads
	// "/\evil.com" (and "\/evil.com", "/\/evil.com") as protocol-relative.
	if strings.Contains(raw, `\`) {
		return "/"
	}
	// Protocol-relative ("//host") → resolves to a different origin.
	if strings.HasPrefix(raw, "//") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "/"
	}
	// Path-only: no scheme, no host, no opaque body. If any is present the
	// value can target another origin, so reject it.
	if u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return "/"
	}
	return raw
}
