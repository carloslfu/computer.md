// SPDX-License-Identifier: Apache-2.0

package main

// Agent-native CLI surface endpoints (plans/agent-native-cli.md). Three
// additions to the daemon's data plane:
//
//   GET /api/version       — version + schema-versions probe
//   GET /api/files/<path>  — read a file under /home/vibecraft (with path safety)
//   GET /api/files-ls      — list a directory under /home/vibecraft
//
// Plus an in-memory idempotency cache shared with handleTask (A11).
//
// All file paths are jailed under /home/vibecraft and the resolved
// realpath must remain inside the jail after symlink resolution. The
// daemon runs as the vibecraft user; this is the SAME jail the agent
// already operates in. No privilege escalation here.

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Schema versions this daemon speaks. New wire-format versions are
// additive within the same major; agents check ContainsInt(schema_versions, X).
var schemaVersions = []int{1}

// fileJailRoot is the only directory the agent-CLI file endpoints will
// serve from. Resolved at startup so symlink games at request time
// can't escape it.
const fileJailRoot = "/home/vibecraft"

// idempotencyTTL is how long we remember a (key → task_id) mapping so
// retried POSTs return the existing task instead of creating a dup.
// 10 minutes covers retry-after-network-hiccup; longer than that the
// agent should treat as a fresh submission.
const idempotencyTTL = 10 * time.Minute

// idempotencyCache is a tiny in-process LRU-ish map of idempotency_key →
// existing task id. Restarting the daemon clears it (which is fine —
// the wire contract says a key is honored for "minutes," not "forever").
var (
	idempotencyMu    sync.Mutex
	idempotencyStore = map[string]idempotencyEntry{}
)

type idempotencyEntry struct {
	TaskID    string
	ExpiresAt time.Time
}

// idempotencyLookup returns (task_id, found). Expired entries are GC'd.
func idempotencyLookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	idempotencyMu.Lock()
	defer idempotencyMu.Unlock()
	now := time.Now()
	if e, ok := idempotencyStore[key]; ok {
		if now.Before(e.ExpiresAt) {
			return e.TaskID, true
		}
		delete(idempotencyStore, key)
	}
	// Opportunistic sweep — keeps the map from growing unbounded.
	if len(idempotencyStore) > 256 {
		for k, v := range idempotencyStore {
			if now.After(v.ExpiresAt) {
				delete(idempotencyStore, k)
			}
		}
	}
	return "", false
}

// idempotencyStoreSet records key → task_id with the standard TTL.
func idempotencyStoreSet(key, taskID string) {
	if key == "" || taskID == "" {
		return
	}
	idempotencyMu.Lock()
	defer idempotencyMu.Unlock()
	idempotencyStore[key] = idempotencyEntry{
		TaskID:    taskID,
		ExpiresAt: time.Now().Add(idempotencyTTL),
	}
}

// handleVersion answers GET /api/version with the daemon's version and
// the wire schema versions it speaks. The CLI calls this on its first
// request per process and refuses to operate if it was built against
// a schema this daemon doesn't list.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"version":         version,
		"schema_versions": schemaVersions,
	})
}

// FileInfo is one row of a files-ls response.
type FileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`           // absolute under fileJailRoot
	Size    int64  `json:"size,omitempty"` // 0 for dirs
	Type    string `json:"type"`           // "file" | "dir" | "symlink" | "other"
	ModTime string `json:"mtime"`          // RFC3339
}

// resolveJailedPath returns the absolute path inside the jail. It rejects
// paths that escape via .. or via a symlink chain. Returns the canonical
// path on success.
func resolveJailedPath(reqPath string) (string, error) {
	if reqPath == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(reqPath, "/") {
		// Relative paths are joined to the jail root by convention so
		// agents can write `inbox/foo.txt` without remembering the prefix.
		reqPath = filepath.Join(fileJailRoot, reqPath)
	}
	// Canonicalize. EvalSymlinks resolves any link chain; if the resolved
	// real path falls outside the jail we reject — symlink-out is the
	// classic jailbreak this guards against.
	clean := filepath.Clean(reqPath)
	// Resolve the JAIL root once (it is itself a real path, no symlink
	// games expected) so the prefix check below compares apples to apples.
	jailReal, err := filepath.EvalSymlinks(fileJailRoot)
	if err != nil {
		// /home/vibecraft must exist on a running daemon; if it doesn't
		// we treat that as a server config issue, not a 4xx.
		return "", fmt.Errorf("jail root not available: %w", err)
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Not-found is the common case; return a sentinel so the handler
		// can map to 404. The error string is safe to bubble up — it
		// won't leak unexpected paths.
		if os.IsNotExist(err) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	rel, err := filepath.Rel(jailReal, real)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." && real != jailReal {
		// rel "." is OK only when listing the root itself.
		if rel == "." {
			return real, nil
		}
		return "", fmt.Errorf("path escapes jail")
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes jail")
	}
	return real, nil
}

// handleFiles serves GET /api/files/<path>. Path is everything after
// "/files/". Returns the file body with a best-effort MIME type. Hard
// caps at 64MB to mirror the CLI client's response limit (G7).
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/files/")
	if rest == r.URL.Path {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	// "/files/" leads to the jail-root list — point users at files-ls.
	if rest == "" {
		jsonError(w, "use /api/files-ls?path=/ for directory listings", http.StatusBadRequest)
		return
	}
	real, err := resolveJailedPath("/" + rest)
	if err != nil {
		if err == os.ErrNotExist {
			jsonError(w, "path not found", http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "escapes jail") {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonError(w, fmt.Sprintf("resolving path: %v", err), http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(real)
	if err != nil {
		jsonError(w, "stat failed", http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		jsonError(w, "path is a directory", http.StatusBadRequest)
		return
	}
	const maxFileSize = 64 << 20
	if info.Size() > maxFileSize {
		jsonError(w, fmt.Sprintf("file exceeds %d byte cap", maxFileSize), http.StatusRequestEntityTooLarge)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		jsonError(w, "open failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	ctype := mime.TypeByExtension(filepath.Ext(real))
	if ctype == "" {
		var buf [512]byte
		n, _ := f.Read(buf[:])
		ctype = http.DetectContentType(buf[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			jsonError(w, "rewind failed", http.StatusInternalServerError)
			return
		}
	}
	// The agent writes files all over /home/vibecraft, and a prompt-injected
	// or malicious worker can plant attacker-controlled HTML/SVG there. This
	// endpoint serves them from the SAME origin as the authenticated SPA, so
	// a top-level navigation to /api/files/<that>.html would otherwise run
	// the attacker's JS under the victim's vc_session. The CLI consumes the
	// raw body (it doesn't render), so forcing nosniff + attachment +
	// neutral content-type for non-image types breaks nothing on the CLI
	// side while closing the browser stored-XSS path. Shared with handleInbox.
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	setDownloadSecurityHeaders(w.Header(), filepath.Base(real), ctype)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// handleFilesLs answers GET /api/files-ls?path=<absolute path>.
// Returns a JSON array of FileInfo. Path defaults to /home/vibecraft.
func (s *Server) handleFilesLs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		reqPath = fileJailRoot
	}
	real, err := resolveJailedPath(reqPath)
	if err != nil {
		if err == os.ErrNotExist {
			jsonError(w, "path not found", http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "escapes jail") {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonError(w, fmt.Sprintf("resolving path: %v", err), http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(real)
	if err != nil {
		jsonError(w, "stat failed", http.StatusInternalServerError)
		return
	}
	if !info.IsDir() {
		jsonError(w, "path is a file", http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		jsonError(w, fmt.Sprintf("readdir: %v", err), http.StatusInternalServerError)
		return
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		t := "file"
		switch {
		case fi.IsDir():
			t = "dir"
		case fi.Mode()&os.ModeSymlink != 0:
			t = "symlink"
		case fi.Mode()&os.ModeType != 0:
			t = "other"
		}
		out = append(out, FileInfo{
			Name:    fi.Name(),
			Path:    filepath.Join(real, fi.Name()),
			Size:    fi.Size(),
			Type:    t,
			ModTime: fi.ModTime().UTC().Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"path": real, "entries": out})
}
