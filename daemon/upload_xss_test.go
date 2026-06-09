// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/core"
)

// upload_xss_test.go pins the stored-XSS hardening on the same-origin file
// handlers (handleInbox + handleFiles share setDownloadSecurityHeaders) and
// the multipart spill-temp-file cleanup.

// uploadOneFile uploads a single file and returns the stored name.
func uploadOneFile(t *testing.T, ts *testServer, jwt, convID, filename string, content []byte, ct string) string {
	t.Helper()
	cts := map[string]string{}
	if ct != "" {
		cts[filename] = ct
	}
	body, mpct := buildMultipart(t, convID, map[string][]byte{filename: content}, cts)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, mpct)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Attachments []core.Attachment `json:"attachments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(resp.Attachments))
	}
	return resp.Attachments[0].Name
}

// TestInboxServesHTMLAsHardenedDownload is the red→green for the high-sev
// inbox stored-XSS finding. An uploaded evil.html served same-origin under
// the vc_session cookie must NOT come back as renderable text/html. The
// response must carry X-Content-Type-Options: nosniff, Content-Disposition:
// attachment, and a neutral (non-active) Content-Type so a top-level
// navigation can't execute the injected <script>.
func TestInboxServesHTMLAsHardenedDownload(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "xss-inbox-1"
	evil := []byte(`<html><body><script>fetch('/api/vault')</script></body></html>`)
	stored := uploadOneFile(t, ts, jwt, convID, "evil.html", evil, "text/html")

	req := httptest.NewRequest(http.MethodGet, "/api/inbox/"+convID+"/"+stored, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rr := httptest.NewRecorder()
	ts.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("inbox fetch want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	ctype := strings.ToLower(rr.Header().Get("Content-Type"))
	if strings.Contains(ctype, "text/html") {
		t.Errorf("Content-Type = %q — HTML must not be served as active content on the SPA origin", ctype)
	}
	// Body still served (it's a download), just not as active content.
	if !bytes.Equal(rr.Body.Bytes(), evil) {
		t.Errorf("body changed; want passthrough download")
	}
}

// TestInboxServesSVGAsHardenedDownload pins the SVG variant — SVG can carry
// inline <script>, so image/svg+xml must NOT be in the inline allowlist.
func TestInboxServesSVGAsHardenedDownload(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "xss-inbox-svg"
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	stored := uploadOneFile(t, ts, jwt, convID, "evil.svg", svg, "image/svg+xml")

	req := httptest.NewRequest(http.MethodGet, "/api/inbox/"+convID+"/"+stored, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rr := httptest.NewRecorder()
	ts.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing for svg: %q", got)
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("svg not served as attachment: %q", rr.Header().Get("Content-Disposition"))
	}
	if ctype := strings.ToLower(rr.Header().Get("Content-Type")); strings.Contains(ctype, "svg") {
		t.Errorf("Content-Type = %q — svg must not be served as active content", ctype)
	}
}

// TestInboxServesPNGInline confirms the hardening doesn't break the legit
// case: a real raster image keeps its image/* Content-Type (so the chat can
// render thumbnails) while STILL carrying nosniff. PNG can't execute script,
// so it's on the allowlist.
func TestInboxServesPNGInline(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "img-inbox-1"
	// 1x1 PNG.
	png := []byte("\x89PNG\r\n\x1a\n_______________some_png_bytes_______________")
	stored := uploadOneFile(t, ts, jwt, convID, "shot.png", png, "image/png")

	req := httptest.NewRequest(http.MethodGet, "/api/inbox/"+convID+"/"+stored, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rr := httptest.NewRecorder()
	ts.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing for png: %q", got)
	}
	if ctype := strings.ToLower(rr.Header().Get("Content-Type")); !strings.Contains(ctype, "image/png") {
		t.Errorf("png Content-Type = %q, want image/png (safe raster stays inline)", ctype)
	}
}

// TestSetDownloadSecurityHeaders directly pins the shared helper both file
// handlers (handleInbox + handleFiles) use. This is the unit-level proof for
// the agent-cli /api/files finding too: handleFiles routes its response
// through this same helper, and the const fileJailRoot makes its full handler
// path hard to exercise off a real /home/vibecraft host.
func TestSetDownloadSecurityHeaders(t *testing.T) {
	cases := []struct {
		name        string
		filename    string
		inCType     string
		wantCType   string
		wantInline  bool // true => Content-Type preserved (allowlisted image)
	}{
		{"html neutralized", "x.html", "text/html; charset=utf-8", "application/octet-stream", false},
		{"svg neutralized", "x.svg", "image/svg+xml", "application/octet-stream", false},
		{"js neutralized", "x.js", "text/javascript", "application/octet-stream", false},
		{"png inline", "x.png", "image/png", "image/png", true},
		{"jpeg inline", "x.jpg", "image/jpeg", "image/jpeg", true},
		{"gif inline", "x.gif", "image/gif", "image/gif", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			setDownloadSecurityHeaders(h, c.filename, c.inCType)
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("nosniff = %q, want nosniff", got)
			}
			if cd := h.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
				t.Errorf("Content-Disposition = %q, want attachment", cd)
			}
			gotCType := h.Get("Content-Type")
			if c.wantInline {
				if !strings.HasPrefix(gotCType, c.wantCType) {
					t.Errorf("Content-Type = %q, want %q (allowlisted image)", gotCType, c.wantCType)
				}
			} else if gotCType != "application/octet-stream" {
				t.Errorf("active type %q not neutralized: Content-Type = %q", c.inCType, gotCType)
			}
		})
	}
}

// TestContentDispositionFilenameSanitized ensures a hostile stored filename
// can't break out of the quoted Content-Disposition token.
func TestContentDispositionFilenameSanitized(t *testing.T) {
	h := http.Header{}
	setDownloadSecurityHeaders(h, "a\"b\\c\nd.txt", "application/octet-stream")
	cd := h.Get("Content-Disposition")
	if strings.ContainsAny(strings.TrimPrefix(cd, "attachment; filename=\""), "\n\r") {
		t.Errorf("control chars survived into Content-Disposition: %q", cd)
	}
	if strings.Count(cd, "\"") != 2 {
		t.Errorf("filename quoting broken (embedded quote not stripped): %q", cd)
	}
}

// TestUploadRemovesMultipartSpillTempFiles is the red→green for the disk-leak
// finding. ParseMultipartForm spills file parts beyond its 32 MB in-memory
// budget to $TMPDIR; Go does not auto-remove them — handleUpload must call
// RemoveAll. We point $TMPDIR at a fresh dir, push a request well past 32 MB
// (forcing a spill), and assert no orphaned multipart-* files remain after.
func TestUploadRemovesMultipartSpillTempFiles(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	convID := "spill-conv-1"
	// Three ~20 MB parts = ~60 MB > 32 MB in-memory budget → spills to TMPDIR.
	chunk := bytes.Repeat([]byte("a"), 20<<20)
	files := map[string][]byte{
		"a.bin": append([]byte(nil), chunk...),
		"b.bin": append([]byte(nil), chunk...),
		"c.bin": append([]byte(nil), chunk...),
	}
	body, ct := buildMultipart(t, convID, files, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body.String())
	}

	// After the handler returns, no multipart spill temp files should remain.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	var leaked []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multipart-") {
			leaked = append(leaked, filepath.Join(tmpDir, e.Name()))
		}
	}
	if len(leaked) > 0 {
		t.Errorf("multipart spill temp files leaked (RemoveAll not called): %v", leaked)
	}
}
