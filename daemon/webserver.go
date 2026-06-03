// SPDX-License-Identifier: Apache-2.0

package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"strings"
)

// webFS holds the Vite-built SPA bundle. A placeholder index.html ships
// in the repo so //go:embed compiles on fresh checkouts; CI / make build
// overwrites the directory with the real bundle.
//
//go:embed all:web/dist
var webFS embed.FS

// registerWebRoutes mounts the SPA at "/" with proper cache headers
// and an SPA fallback (unknown non-asset paths serve index.html so
// React Router can take over).
func (s *Server) registerWebRoutes(mux *http.ServeMux) {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Printf("web: sub fs failed: %v", err)
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "SPA bundle missing", http.StatusServiceUnavailable)
		})
		return
	}
	mux.Handle("/", spaHandler(sub))
}

// spaSecurityHeaders are the baseline security response headers set on
// every SPA (and SPA-404) response. The CSP locks the app to its own
// origin: scripts/styles/fonts/etc. come from 'self' only, the page may
// not be framed (clickjacking), XHR/SSE may only reach 'self' and the
// platform API, and images may load from data:/blob: URLs so the chat
// can render inline screenshots the daemon hands back as data URIs.
// X-Content-Type-Options stops MIME sniffing; Referrer-Policy keeps the
// (potentially sensitive) machine host out of outbound Referer headers.
const spaContentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' data: blob:; " +
	"connect-src 'self' https://www.vibecraft.so; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

func setSPASecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", spaContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers on every response this handler emits, including
		// the SPA shell, hashed assets, the SPA fallback, and 404s.
		setSPASecurityHeaders(w.Header())

		// http.ServeMux routes everything not matched by a more specific
		// pattern to "/", so this handler also receives requests for
		// /favicon.ico, /robots.txt, etc. Anything starting with "/api/",
		// "/auth/", "/health", "/routes/verify", "/daemon/task",
		// "/management/", "/notify", or the un-namespaced legacy paths
		// is owned by the API and should never have reached here — if it
		// did (mux miss), explicitly 404 rather than fall through to
		// the SPA's index.html (which would let an attacker fingerprint
		// the SPA's structure under arbitrary API paths).
		if isAPIRoute(r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// SPA fallback: unknown non-asset paths (e.g. /c/abc,
		// /settings/vault) serve index.html so React Router can pick up
		// from there. Asset paths (contain a dot, e.g. .js / .css /
		// .png) get a real 404 if missing.
		if _, err := fs.Stat(fsys, path); err != nil && !strings.Contains(path, ".") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			r = r2
			path = "index.html"
		}

		// Cache: immutable for hashed assets (filenames carry a content
		// hash from Vite), no-cache for HTML so the user always loads
		// the latest entry point.
		if strings.Contains(path, ".") && !strings.HasSuffix(path, ".html") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// isAPIRoute returns true for paths owned by the daemon's HTTP API.
// Used by the SPA fallback to refuse to mask API routes with
// index.html when the mux doesn't pick them up (e.g. typo in a URL).
func isAPIRoute(p string) bool {
	if strings.HasPrefix(p, "/api/") {
		return true
	}
	if strings.HasPrefix(p, "/auth/") {
		return true
	}
	switch p {
	case "/health", "/status", "/system", "/tasks", "/task", "/conversations",
		"/screenshot", "/upload", "/stream", "/memory", "/rules", "/audit",
		"/vault", "/config", "/keys", "/routes", "/notify":
		return true
	}
	switch {
	case strings.HasPrefix(p, "/task/"),
		strings.HasPrefix(p, "/conversations/"),
		strings.HasPrefix(p, "/inbox/"),
		strings.HasPrefix(p, "/memory/"),
		strings.HasPrefix(p, "/rules/"),
		strings.HasPrefix(p, "/vault/"),
		strings.HasPrefix(p, "/keys/"),
		strings.HasPrefix(p, "/routes/"),
		strings.HasPrefix(p, "/daemon/"),
		strings.HasPrefix(p, "/management/"):
		return true
	}
	return false
}
