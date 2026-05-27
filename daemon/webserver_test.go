// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// Phase 6 attack-surface coverage for the post-chat-UI SPA static
// surface (daemon/webserver.go). The bundle ships via //go:embed, so the
// real file source is an embed.FS, which has no `..` parent and no OS
// path semantics — but the handler also runs an SPA fallback that
// rewrites unknown paths to index.html and an anti-fingerprint guard
// that 404s API paths instead of masking them with index.html. These
// tests pin all three: traversal cannot escape the embedded root, the
// fallback only fires for non-asset misses, and the guard holds.

// fakeWeb is an in-memory stand-in for web/dist so the test is
// independent of whatever placeholder/real bundle is embedded at build
// time. spaHandler takes an fs.FS, so this exercises the exact code
// path registerWebRoutes uses (fs.Sub(webFS,"web/dist") → spaHandler).
func fakeWeb() fstest.MapFS {
	return fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html><title>spa</title>")},
		"assets/app.js":   {Data: []byte("console.log(1)")},
		"assets/app.css":  {Data: []byte("body{}")},
		"favicon.ico":     {Data: []byte("ico")},
		"secret-only.txt": {Data: []byte("served-from-root-ok")},
	}
}

func TestSPAHandler_PathTraversalCannotEscapeEmbed(t *testing.T) {
	h := spaHandler(fakeWeb())

	// None of these may ever return the bytes of a file outside the
	// embedded tree. Since there IS no outside an embed/MapFS, the
	// concrete assertion is: never a 200 carrying non-SPA content, and
	// for asset-looking traversals never anything but a 404 or the
	// index.html fallback. We also assert the body is never something
	// that looks like an OS file (e.g. "root:" from /etc/passwd) — a
	// regression canary if the impl were ever swapped to os.DirFS.
	traversals := []struct {
		desc string
		path string
	}{
		{"dotdot etc passwd", "/../../../../etc/passwd"},
		{"dotdot caddyfile", "/../../etc/caddy/Caddyfile"},
		{"encoded dotdot", "/%2e%2e/%2e%2e/etc/passwd"},
		{"double encoded dotdot", "/%252e%252e/etc/passwd"},
		{"backslash traversal", `/..\..\etc\passwd`},
		{"absolute path", "//etc/passwd"},
		{"absolute single slash", "/etc/vibecraft/daemon.token"},
		{"null byte truncation", "/assets/app.js%00.png"},
		{"dotdot in asset segment", "/assets/../../../etc/passwd"},
		{"mixed encoded", "/assets/%2e%2e%2f%2e%2e%2fetc%2fpasswd"},
		{"trailing dotdot", "/assets/app.js/../../../../etc/passwd"},
		{"unc-ish", "/\\\\attacker\\share"},
	}

	for _, tc := range traversals {
		t.Run(tc.desc, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			body := w.Body.String()
			// Hard fail: OS-file content must never appear.
			if containsStr(body, "root:") || containsStr(body, "BEGIN OPENSSH") {
				t.Fatalf("traversal %q leaked host file content (status %d): %q",
					tc.path, w.Code, body)
			}
			// The only legitimate 200 a traversal could resolve to is the
			// SPA index.html (the fallback). It must NEVER resolve to an
			// arbitrary embedded file via a `..` escape, and never to
			// host content. So a 200 is acceptable only if the body is
			// exactly the SPA shell.
			if w.Code == http.StatusOK {
				if body != "<!doctype html><title>spa</title>" {
					t.Fatalf("traversal %q returned 200 with non-SPA body: %q",
						tc.path, body)
				}
			} else if w.Code != http.StatusNotFound &&
				w.Code != http.StatusMovedPermanently &&
				w.Code != http.StatusBadRequest {
				t.Fatalf("traversal %q: unexpected status %d (want 200-SPA / 404 / 301 / 400)",
					tc.path, w.Code)
			}
		})
	}
}

func TestSPAHandler_AntiFingerprintGuard(t *testing.T) {
	h := spaHandler(fakeWeb())

	// API-owned paths that miss the mux must 404 — NOT fall through to
	// index.html. Returning the SPA shell under an arbitrary /api path
	// would let an attacker confirm the SPA exists and probe its routing
	// (webserver.go isAPIRoute guard, cited in SECURITY.md).
	apiPaths := []string{
		"/api/", "/api/tasks", "/api/secret/whatever",
		"/auth/", "/auth/callback",
		"/health", "/status", "/tasks", "/vault", "/keys", "/routes",
		"/task/123", "/conversations/abc", "/management/usage",
		"/daemon/task", "/memory/x", "/rules/y",
	}
	for _, p := range apiPaths {
		t.Run("guard "+p, func(t *testing.T) {
			req := httptest.NewRequest("GET", p, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("API path %q must 404 at SPA handler, got %d body=%q",
					p, w.Code, w.Body.String())
			}
			if containsStr(w.Body.String(), "<title>spa</title>") {
				t.Fatalf("API path %q was masked with index.html (fingerprintable)", p)
			}
		})
	}
}

func TestSPAHandler_FallbackAndAssetsBehaveCorrectly(t *testing.T) {
	h := spaHandler(fakeWeb())

	t.Run("known asset served with immutable cache", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/assets/app.js", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for known asset, got %d", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("asset Cache-Control = %q, want immutable", cc)
		}
	})

	t.Run("unknown non-asset path falls back to index.html (no-cache)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/c/some-conversation-id", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("SPA fallback expected 200, got %d", w.Code)
		}
		if w.Body.String() != "<!doctype html><title>spa</title>" {
			t.Errorf("fallback did not serve index.html, got %q", w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("HTML Cache-Control = %q, want no-cache", cc)
		}
	})

	t.Run("missing asset (has dot) gets a real 404, not the SPA shell", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/assets/does-not-exist.js", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("missing asset expected 404, got %d", w.Code)
		}
		if containsStr(w.Body.String(), "<title>spa</title>") {
			t.Errorf("missing asset was masked with index.html (would break asset 404 contract)")
		}
	})

	t.Run("root serves index.html", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Body.String() != "<!doctype html><title>spa</title>" {
			t.Fatalf("root: got %d body=%q", w.Code, w.Body.String())
		}
	})
}

// isAPIRoute is the guard's decision function; pin its contract directly
// so a future route addition that forgets to extend it is caught here.
func TestIsAPIRoute(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/", true},
		{"/api/tasks", true},
		{"/auth/callback", true},
		{"/health", true},
		{"/status", true},
		{"/task/1", true},
		{"/management/usage", true},
		{"/daemon/task", true},
		{"/", false},
		{"/c/abc", false},
		{"/settings/vault", false},
		{"/assets/app.js", false},
		{"/index.html", false},
		{"/favicon.ico", false},
		{"/apinot", false}, // prefix-confusion: not /api/
		{"/healthz", false},
	}
	for _, c := range cases {
		if got := isAPIRoute(c.path); got != c.want {
			t.Errorf("isAPIRoute(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
