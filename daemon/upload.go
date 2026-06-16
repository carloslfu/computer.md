// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/core"
)

// The daemon runs as root but the agent's bash tool executes as the
// `vibecraft` user. Inbox files must be readable by that user or the
// agent can't open them. We look up vibecraft's uid/gid once and chown
// every saved file to it. Silently skipped when the user doesn't exist
// (local dev) or the chown fails (non-root daemon). We keep 0600 mode —
// after the chown, the file's owner is vibecraft, which is what we want.
var (
	agentOwnerOnce sync.Once
	agentUID       = -1
	agentGID       = -1
)

func agentOwnerIDs() (int, int) {
	agentOwnerOnce.Do(func() {
		u, err := user.Lookup("vibecraft")
		if err != nil {
			return
		}
		if uid, err := strconv.Atoi(u.Uid); err == nil {
			agentUID = uid
		}
		if gid, err := strconv.Atoi(u.Gid); err == nil {
			agentGID = gid
		}
	})
	return agentUID, agentGID
}

// chownToAgent makes a path readable by the agent user. No-ops if the
// user isn't present on this machine (common in dev / tests) and swallows
// EPERM so running the daemon as a non-root user during development
// doesn't break uploads.
func chownToAgent(path string) {
	uid, gid := agentOwnerIDs()
	if uid < 0 || gid < 0 {
		return
	}
	if err := os.Chown(path, uid, gid); err != nil && !os.IsPermission(err) {
		log.Printf("upload: chown %s: %v", path, err)
	}
}

// Upload limits. Tuned for the operator ICP — receipts, CSV exports,
// contracts, screenshots, and the occasional deck. Video and giant
// datasets are outside the scope of chat attachments; if a customer
// needs them, they should land via scp / wget inside the machine.
const (
	maxUploadFileSize  = 25 << 20  // 25 MB per file
	maxUploadTotalSize = 100 << 20 // 100 MB per request
	maxUploadFiles     = 10
)

// conversationIDPattern restricts conversation IDs used as directory names
// to the shape the platform emits (UUID v4). The exact UUID regex would be
// slower with little benefit — we just need to reject path traversal,
// shell metacharacters, and overly long strings.
var conversationIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// filenameCleanPattern matches characters safe to keep in a stored filename.
// Anything else is replaced with `_`.
var filenameCleanPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// handleUpload accepts multipart form uploads. Files are saved under
// <InboxDir>/<conversation_id>/<prefix>-<sanitized-name> with a
// millisecond-precision prefix to guarantee uniqueness and preserve
// upload order on disk.
//
// Response shape:
//
//	{
//	  "attachments": [
//	    { "name": "...", "path": "...", "mime": "...", "size": 123,
//	      "original": "original-client-name.pdf" }
//	  ]
//	}
//
// The `path` is absolute on the daemon filesystem so the agent can cat,
// file, or open it straight from the bash tool without path gymnastics.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	// Cap the body up front so a rogue client can't exhaust memory by
	// streaming gigabytes — Go's multipart reader won't help here because
	// it greedily reads the headers and part boundaries.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotalSize)

	// ParseMultipartForm holds up to 32 MB in memory and spills to disk.
	// We don't care about the spill path because we immediately copy each
	// part to its final location with per-file size enforcement.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, fmt.Sprintf("parsing multipart form: %v", err), http.StatusBadRequest)
		return
	}
	// Go does not auto-remove the temp files ParseMultipartForm spills to
	// $TMPDIR for parts beyond the in-memory budget. Remove them on every
	// exit path once the form has parsed successfully.
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	convID := r.FormValue("conversation_id")
	if convID == "" {
		jsonError(w, "conversation_id is required", http.StatusBadRequest)
		return
	}
	if !conversationIDPattern.MatchString(convID) {
		jsonError(w, "invalid conversation_id", http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		jsonError(w, "at least one file is required (form field: file)", http.StatusBadRequest)
		return
	}
	if len(files) > maxUploadFiles {
		jsonError(w, fmt.Sprintf("too many files (max %d per upload)", maxUploadFiles), http.StatusBadRequest)
		return
	}

	convDir, err := ensureInboxConversationDir(s.cfg.InboxDir, convID)
	if err != nil {
		log.Printf("upload: prepare inbox conversation dir: %v", err)
		jsonError(w, "could not create inbox directory", http.StatusInternalServerError)
		return
	}
	// Ensure the jail dir is traversable by the agent user. Idempotent.
	chownToAgent(convDir)
	_ = os.Chmod(convDir, 0700)

	saved := make([]core.Attachment, 0, len(files))
	for _, fh := range files {
		if fh.Size > maxUploadFileSize {
			rollbackUploads(saved)
			jsonError(w, fmt.Sprintf("file %q exceeds %d byte limit", fh.Filename, maxUploadFileSize), http.StatusRequestEntityTooLarge)
			return
		}

		att, err := persistUploadedFile(fh, convDir)
		if err != nil {
			rollbackUploads(saved)
			log.Printf("upload: persist %s: %v", fh.Filename, err)
			jsonError(w, fmt.Sprintf("failed to save %q: %v", fh.Filename, err), http.StatusInternalServerError)
			return
		}
		saved = append(saved, att)
	}

	s.auditLog.Log(audit.Entry{
		Action:   "file_uploaded",
		Category: "chat",
		UserID:   userFromCtx(r),
		Details:  fmt.Sprintf("conv=%s count=%d", convID, len(saved)),
	})

	jsonResponse(w, http.StatusOK, map[string]interface{}{"attachments": saved})
}

// persistUploadedFile streams one multipart part to a fresh file in convDir
// and returns the metadata we hand back to the client. The read is capped
// to maxUploadFileSize bytes; a larger part is treated as an error and the
// partial file is deleted before returning.
func persistUploadedFile(fh *multipartFileHeader, convDir string) (core.Attachment, error) {
	src, err := fh.Open()
	if err != nil {
		return core.Attachment{}, fmt.Errorf("opening upload: %w", err)
	}
	defer src.Close()

	clean := sanitizeFilename(fh.Filename)
	prefix, err := uniqueNamePrefix()
	if err != nil {
		return core.Attachment{}, fmt.Errorf("generating name prefix: %w", err)
	}
	storedName := prefix + "-" + clean
	dstPath := filepath.Join(convDir, storedName)

	// O_EXCL ensures we never overwrite — the prefix makes collisions
	// effectively impossible, but relying on the filesystem to enforce it
	// is cheap insurance.
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return core.Attachment{}, fmt.Errorf("creating dest: %w", err)
	}

	// io.CopyN + one extra byte — if the client lied about Size we catch it.
	written, copyErr := io.Copy(dst, io.LimitReader(src, maxUploadFileSize+1))
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(dstPath)
		return core.Attachment{}, fmt.Errorf("writing upload: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dstPath)
		return core.Attachment{}, fmt.Errorf("closing upload: %w", closeErr)
	}
	if written > maxUploadFileSize {
		_ = os.Remove(dstPath)
		return core.Attachment{}, fmt.Errorf("file exceeds %d byte limit", maxUploadFileSize)
	}

	// Hand off ownership to the agent user so its bash tool can open
	// the file. No-op when the daemon isn't root.
	chownToAgent(dstPath)

	mimeType := detectMIME(fh, dstPath)

	return core.Attachment{
		Name:     storedName,
		Path:     dstPath,
		MIME:     mimeType,
		Size:     written,
		Original: fh.Filename,
	}, nil
}

// rollbackUploads removes files we already wrote when a later file in the
// same batch fails — clients should see the whole request succeed or the
// whole request fail, never a silent partial save.
func rollbackUploads(saved []core.Attachment) {
	for _, att := range saved {
		if err := os.Remove(att.Path); err != nil && !os.IsNotExist(err) {
			log.Printf("upload: rollback remove %s: %v", att.Path, err)
		}
	}
}

// detectMIME returns the MIME type for an uploaded file, preferring the
// client-supplied Content-Type and falling back to extension + content
// sniffing. Both paths produce stable, lowercased type/subtype strings.
func detectMIME(fh *multipartFileHeader, path string) string {
	if ct := fh.Header.Get("Content-Type"); ct != "" {
		// Strip parameters (boundary, charset, etc.).
		if slash := strings.Index(ct, ";"); slash >= 0 {
			ct = ct[:slash]
		}
		ct = strings.TrimSpace(strings.ToLower(ct))
		if ct != "" && ct != "application/octet-stream" {
			return ct
		}
	}
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	var buf [512]byte
	n, _ := f.Read(buf[:])
	return http.DetectContentType(buf[:n])
}

// handleInbox serves previously-uploaded files back to the dashboard so
// the chat UI can render image thumbnails after a page reload (the browser
// can't read files from the customer's own VM filesystem on its own, and
// we don't want to base64-bloat the DB with every upload).
//
// Path shape: /inbox/<conversation_id>/<filename>
//
// Safety: conversation id and filename are both whitelisted; the resolved
// absolute path must remain inside <InboxDir>/<conv> or we reject. Symlinks
// are resolved via filepath.EvalSymlinks so an attacker can't escape the
// jail by planting one.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/inbox/")
	if rest == "" || rest == r.URL.Path {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	convID, name := parts[0], parts[1]

	if !conversationIDPattern.MatchString(convID) {
		jsonError(w, "invalid conversation_id", http.StatusBadRequest)
		return
	}
	// The filename segment should be flat — we only ever write flat names
	// during upload. A slash or `..` here is always an attack.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		jsonError(w, "invalid filename", http.StatusBadRequest)
		return
	}

	convDir := filepath.Join(s.cfg.InboxDir, convID)
	fullPath := filepath.Join(convDir, name)

	realFull, err := resolveInboxFilePath(s.cfg.InboxDir, convDir, fullPath)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	f, err := os.Open(realFull)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype == "" {
		var buf [512]byte
		n, _ := f.Read(buf[:])
		ctype = http.DetectContentType(buf[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			jsonError(w, "could not rewind file", http.StatusInternalServerError)
			return
		}
	}
	// Harden against stored XSS. Inbox files are uploaded by control-tier
	// principals (team members, agents) and served from the SAME origin as
	// the authenticated SPA, sharing the vc_session cookie. An uploaded
	// evil.html / evil.svg would otherwise execute in the machineHost origin
	// on a top-level navigation. Force a benign content type, stop sniffing,
	// and serve as a download so the browser never renders it as active
	// content. See setDownloadSecurityHeaders.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	setDownloadSecurityHeaders(w.Header(), name, ctype)
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// downloadSafeContentTypes is the allowlist of types we still serve with
// their real Content-Type (for in-app <img>/preview rendering). Everything
// else is forced to application/octet-stream so an attacker-supplied .html
// or .svg can never be rendered as active content on the SPA origin. SVG is
// deliberately NOT here — it can carry inline <script>.
var downloadSafeContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/avif": true,
}

// setDownloadSecurityHeaders kills the stored-XSS path for files served from
// the authenticated machine origin (inbox uploads, agent-written files).
// It always sets X-Content-Type-Options: nosniff and Content-Disposition:
// attachment, and downgrades any non-allowlisted content type to
// application/octet-stream so the browser cannot execute it as HTML/JS/SVG.
// The caller's Content-Type is only honored for the known-safe raster image
// types in downloadSafeContentTypes.
func setDownloadSecurityHeaders(h http.Header, filename, ctype string) {
	bare := ctype
	if i := strings.Index(bare, ";"); i >= 0 {
		bare = bare[:i]
	}
	bare = strings.TrimSpace(strings.ToLower(bare))
	if !downloadSafeContentTypes[bare] {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "attachment; filename=\""+sanitizeContentDispositionFilename(filename)+"\"")
}

// sanitizeContentDispositionFilename strips characters that could break out
// of the quoted Content-Disposition filename token (quotes, backslashes,
// control chars). Inbox/agent names are already path-segment-validated, but
// the header value is a separate trust boundary.
func sanitizeContentDispositionFilename(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if out == "" {
		return "download"
	}
	return out
}

func ensureInboxConversationDir(inboxRoot, convID string) (string, error) {
	if err := rejectSymlinkPath(inboxRoot); err != nil {
		return "", err
	}
	if err := os.MkdirAll(inboxRoot, 0o700); err != nil {
		return "", err
	}
	if err := rejectSymlinkPath(inboxRoot); err != nil {
		return "", err
	}

	convDir := filepath.Join(inboxRoot, convID)
	if fi, err := os.Lstat(convDir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("conversation dir is a symlink: %s", convDir)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("conversation path is not a directory: %s", convDir)
		}
	} else if os.IsNotExist(err) {
		if err := os.Mkdir(convDir, 0o700); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	if err := rejectSymlinkPath(convDir); err != nil {
		return "", err
	}
	if _, err := resolveInboxDir(inboxRoot, convDir); err != nil {
		return "", err
	}
	return convDir, nil
}

func resolveInboxFilePath(inboxRoot, convDir, fullPath string) (string, error) {
	if err := rejectSymlinkPath(convDir); err != nil {
		return "", err
	}
	realConv, err := resolveInboxDir(inboxRoot, convDir)
	if err != nil {
		return "", err
	}
	realFull, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realConv, realFull)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("file escapes conversation dir")
	}
	return realFull, nil
}

func resolveInboxDir(inboxRoot, convDir string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(inboxRoot)
	if err != nil {
		return "", err
	}
	realConv, err := filepath.EvalSymlinks(convDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, realConv)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("conversation dir escapes inbox root")
	}
	return realConv, nil
}

// sanitizeFilename reduces a client-provided filename to a safe, bounded,
// shell-friendly form for disk storage. The returned name is still
// human-recognisable (invoice-2025-04.pdf → invoice-2025-04.pdf) but
// stripped of anything that could cause path traversal or shell-escape
// trouble when the agent later references it.
func sanitizeFilename(name string) string {
	// Strip any directory component the client may have sent.
	name = filepath.Base(name)
	// Collapse disallowed characters. Dots, underscores, dashes stay.
	cleaned := filenameCleanPattern.ReplaceAllString(name, "_")
	// Avoid leading dots (hidden files) and leading dashes (arg confusion).
	cleaned = strings.TrimLeft(cleaned, ".-")
	if cleaned == "" {
		cleaned = "file"
	}
	// Cap overall length. The prefix adds ~20 chars; leaving ~180 for the
	// user-visible portion keeps us under most FS limits.
	const maxBase = 180
	if len(cleaned) > maxBase {
		ext := filepath.Ext(cleaned)
		stem := strings.TrimSuffix(cleaned, ext)
		keep := maxBase - len(ext)
		if keep < 1 {
			keep = 1
		}
		if len(stem) > keep {
			stem = stem[:keep]
		}
		cleaned = stem + ext
		// A pathologically long extension (no stem to trim against) can
		// leave cleaned above the cap — and above the filesystem's
		// per-name limit, which would fail the create with ENAMETOOLONG.
		// Hard-bound the final result so the stored name is always within
		// maxBase regardless of where the length came from.
		if len(cleaned) > maxBase {
			cleaned = cleaned[:maxBase]
		}
	}
	return cleaned
}

// uniqueNamePrefix returns a short, sortable, collision-resistant prefix
// suitable for namespacing filenames within the per-conversation inbox.
// Example: 1714608342123-3f9c1a
func uniqueNamePrefix() (string, error) {
	ts := time.Now().UTC().UnixMilli()
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%s", ts, hex.EncodeToString(b[:])), nil
}

// multipartFileHeader is a thin alias for multipart.FileHeader so the
// helpers above read well without `multipart.FileHeader` noise at every
// signature. Keeps the imports clean to mime/multipart only here.
type multipartFileHeader = multipart.FileHeader

// validateAttachmentPaths confirms every attachment handed over with a
// /task request points into the right conversation's inbox. Rejects
// attempts to reuse a path from a different conversation, point at an
// arbitrary path on the machine, or sneak a non-existent file into the
// message history. Returns nil for an empty list.
func (s *Server) validateAttachmentPaths(convID string, atts []core.Attachment) error {
	if len(atts) == 0 {
		return nil
	}
	if !conversationIDPattern.MatchString(convID) {
		return fmt.Errorf("invalid conversation_id")
	}
	convDir, err := filepath.EvalSymlinks(filepath.Join(s.cfg.InboxDir, convID))
	if err != nil {
		return fmt.Errorf("unknown conversation inbox")
	}
	for _, att := range atts {
		if att.Path == "" {
			return fmt.Errorf("attachment missing path")
		}
		real, err := filepath.EvalSymlinks(att.Path)
		if err != nil {
			return fmt.Errorf("attachment not found: %s", filepath.Base(att.Path))
		}
		rel, err := filepath.Rel(convDir, real)
		if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
			return fmt.Errorf("attachment outside conversation inbox")
		}
		// Also reject attachments that contain further path segments —
		// the upload handler only ever writes flat filenames under the
		// conv dir.
		if strings.ContainsAny(rel, string(filepath.Separator)) {
			return fmt.Errorf("attachment path not permitted")
		}
	}
	return nil
}
