// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/core"
)

// helper: build a multipart body with conversation_id + one or more files.
func buildMultipart(t *testing.T, convID string, files map[string][]byte, contentTypes map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	if convID != "" {
		if err := w.WriteField("conversation_id", convID); err != nil {
			t.Fatalf("write conv id: %v", err)
		}
	}
	for name, contents := range files {
		var part io.Writer
		var err error
		ct := contentTypes[name]
		if ct != "" {
			h := make(map[string][]string)
			h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, name)}
			h["Content-Type"] = []string{ct}
			part, err = w.CreatePart(h)
		} else {
			part, err = w.CreateFormFile("file", name)
		}
		if err != nil {
			t.Fatalf("create part %s: %v", name, err)
		}
		if _, err := part.Write(contents); err != nil {
			t.Fatalf("write part %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return body, w.FormDataContentType()
}

func uploadRequest(t *testing.T, ts *testServer, auth string, body *bytes.Buffer, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", body)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	return w
}

func TestUploadRejectsMissingAuth(t *testing.T) {
	ts := newTestServer(t)
	body, ct := buildMultipart(t, "abc", map[string][]byte{"a.txt": []byte("hi")}, nil)
	w := uploadRequest(t, ts, "", body, ct)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsViewJWT(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWTWithAccess(t, "viewer-1", "view")
	body, ct := buildMultipart(t, "abc", map[string][]byte{"a.txt": []byte("hi")}, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsViewCookie(t *testing.T) {
	ts := newTestServer(t)
	sess := ts.handshakeCookie(t, "viewer-cookie", "Viewer", "viewer@example.com", "view", "nonce-upload-view")
	body, ct := buildMultipart(t, "abc", map[string][]byte{"a.txt": []byte("hi")}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/upload", body)
	req.AddCookie(sess)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	ts.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsMissingConversationID(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")
	body, ct := buildMultipart(t, "", map[string][]byte{"a.txt": []byte("hi")}, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsInvalidConversationID(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")
	for _, bad := range []string{"../etc", "../../passwd", "a/b", "a b", "", strings.Repeat("a", 200)} {
		body, ct := buildMultipart(t, bad, map[string][]byte{"a.txt": []byte("hi")}, nil)
		w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
		if w.Code != http.StatusBadRequest {
			t.Errorf("conversation_id=%q should have been 400, got %d", bad, w.Code)
		}
	}
}

func TestUploadRejectsSymlinkConversationDir(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	outside := t.TempDir()
	convID := "conv-symlink"
	if err := os.Symlink(outside, filepath.Join(ts.server.cfg.InboxDir, convID)); err != nil {
		t.Fatal(err)
	}

	body, ct := buildMultipart(t, convID, map[string][]byte{"a.txt": []byte("hi")}, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code == http.StatusOK {
		t.Fatalf("upload through symlinked conversation dir must be rejected: %s", w.Body.String())
	}
	if entries, err := os.ReadDir(outside); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("outside target was written via symlink: %v", entries)
	}
}

func TestUploadRejectsWithNoFile(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	mw.WriteField("conversation_id", "abc-123")
	mw.Close()
	w := uploadRequest(t, ts, "Bearer "+jwt, body, mw.FormDataContentType())
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadHappyPathSingleFile(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	content := []byte("hello world")
	body, ct := buildMultipart(t, convID, map[string][]byte{"hello.txt": content}, map[string]string{"hello.txt": "text/plain"})
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Attachments []core.Attachment `json:"attachments"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(resp.Attachments))
	}
	att := resp.Attachments[0]
	if att.Original != "hello.txt" {
		t.Errorf("original: expected hello.txt, got %q", att.Original)
	}
	if att.MIME != "text/plain" {
		t.Errorf("mime: expected text/plain, got %q", att.MIME)
	}
	if att.Size != int64(len(content)) {
		t.Errorf("size: expected %d, got %d", len(content), att.Size)
	}

	// File should exist on disk with matching contents and be confined to the
	// conversation's inbox directory.
	convDir := filepath.Join(ts.server.cfg.InboxDir, convID)
	if !strings.HasPrefix(att.Path, convDir+string(filepath.Separator)) {
		t.Errorf("path %q should start with conv dir %q", att.Path, convDir)
	}
	got, err := os.ReadFile(att.Path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("contents: expected %q, got %q", content, got)
	}
}

func TestUploadMultipleFilesAllSaved(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "multi-convo-123"
	files := map[string][]byte{
		"a.txt": []byte("alpha"),
		"b.png": []byte("\x89PNG\r\n\x1a\nfakepng"),
	}
	body, ct := buildMultipart(t, convID, files, map[string]string{
		"a.txt": "text/plain",
		"b.png": "image/png",
	})
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Attachments []core.Attachment `json:"attachments"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(resp.Attachments))
	}

	mimeByOriginal := map[string]string{}
	for _, a := range resp.Attachments {
		mimeByOriginal[a.Original] = a.MIME
	}
	if mimeByOriginal["b.png"] != "image/png" {
		t.Errorf("png mime: expected image/png, got %q", mimeByOriginal["b.png"])
	}
}

func TestUploadRejectsTooManyFiles(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	files := map[string][]byte{}
	for i := 0; i < maxUploadFiles+2; i++ {
		files[fmt.Sprintf("f%02d.txt", i)] = []byte{'x'}
	}
	body, ct := buildMultipart(t, "many-files", files, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadRejectsOversizedFile(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	oversize := make([]byte, maxUploadFileSize+1024)
	body, ct := buildMultipart(t, "big-file", map[string][]byte{"big.bin": oversize}, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	// Either 413 (our own check) or 400 (maxBytesReader truncation) is
	// acceptable — both mean "we refused the upload," which is the contract.
	if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusBadRequest {
		t.Errorf("expected 413 or 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadSanitizesFilename(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "sanitize-convo"
	// Tricky names: traversal, spaces, unicode, leading dots.
	dangerous := "../../etc/passwd some thing.txt"
	body, ct := buildMultipart(t, convID, map[string][]byte{dangerous: []byte("x")}, nil)
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Attachments []core.Attachment `json:"attachments"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	att := resp.Attachments[0]

	// Saved filename must not contain a slash or ..
	if strings.ContainsAny(att.Name, `/\`) || strings.Contains(att.Name, "..") {
		t.Errorf("stored filename should be flat: %q", att.Name)
	}
	// Saved file must live inside the conv dir.
	convDir := filepath.Join(ts.server.cfg.InboxDir, convID)
	if !strings.HasPrefix(att.Path, convDir+string(filepath.Separator)) {
		t.Errorf("path %q escaped conv dir %q", att.Path, convDir)
	}
	if _, err := os.Stat(att.Path); err != nil {
		t.Errorf("saved file missing: %v", err)
	}
}

func TestInboxServesUploadedFile(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "inbox-fetch-123"
	content := []byte("hello from disk")
	body, ct := buildMultipart(t, convID, map[string][]byte{"note.txt": content}, map[string]string{"note.txt": "text/plain"})
	w := uploadRequest(t, ts, "Bearer "+jwt, body, ct)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body.String())
	}

	var resp struct {
		Attachments []core.Attachment `json:"attachments"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	stored := resp.Attachments[0].Name

	req := httptest.NewRequest(http.MethodGet, "/api/inbox/"+convID+"/"+stored, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rr := httptest.NewRecorder()
	ts.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("inbox fetch expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), content) {
		t.Errorf("content mismatch: expected %q, got %q", content, rr.Body.Bytes())
	}
}

func TestInboxRejectsPathTraversal(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	// Plant a secret outside the inbox root but inside the tmpdir so
	// realpath resolution can reach it via an encoded traversal.
	outside := filepath.Join(filepath.Dir(ts.server.cfg.InboxDir), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}

	// URL-encoded `..` — some servers normalize path before routing and
	// strip the segment; httptest.NewRequest preserves the raw path.
	attempts := []string{
		"/inbox/../secret.txt",
		"/inbox/%2e%2e/secret.txt",
		"/inbox//",
		"/inbox/conv/..secret.txt",
	}
	for _, p := range attempts {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rr := httptest.NewRecorder()
		ts.mux.ServeHTTP(rr, req)
		if rr.Code == http.StatusOK {
			t.Errorf("path %q should NOT have returned 200", p)
		}
	}
}

func TestInboxRejectsSymlinkConversationDir(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")

	convID := "conv-symlink-fetch"
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ts.server.cfg.InboxDir, convID)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/inbox/"+convID+"/secret.txt", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rr := httptest.NewRecorder()
	ts.mux.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("inbox fetch through symlinked conversation dir must be rejected, got body %q", rr.Body.String())
	}
}

func TestInboxRejectsUnknownConversation(t *testing.T) {
	ts := newTestServer(t)
	jwt := ts.signJWT(t, "user-1")
	req := httptest.NewRequest(http.MethodGet, "/api/inbox/not-a-convo/whatever.txt", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rr := httptest.NewRecorder()
	ts.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"ok.txt":               "ok.txt",
		"../etc/passwd":        "passwd",
		"with spaces.pdf":      "with_spaces.pdf",
		".hidden":              "hidden",
		"-leading-dash":        "leading-dash",
		"":                     "file",
		"résumé.pdf":           "r_sum_.pdf",
		"weird/slashes.png":    "slashes.png",
		"\\windows\\stuff.doc": "_windows_stuff.doc",
	}
	for in, want := range cases {
		got := sanitizeFilename(in)
		if got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilenameCapsLength(t *testing.T) {
	long := strings.Repeat("x", 500) + ".txt"
	got := sanitizeFilename(long)
	if len(got) > 200 {
		t.Errorf("expected length <= 200, got %d", len(got))
	}
	if !strings.HasSuffix(got, ".txt") {
		t.Errorf("expected .txt preserved, got %q", got)
	}
}

func TestValidateAttachmentPathsAcceptsInboxPaths(t *testing.T) {
	ts := newTestServer(t)
	convID := "conv-abc"
	convDir := filepath.Join(ts.server.cfg.InboxDir, convID)
	if err := os.MkdirAll(convDir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(convDir, "real.txt")
	os.WriteFile(p, []byte("ok"), 0600)

	err := ts.server.validateAttachmentPaths(convID, []core.Attachment{{Path: p, Name: "real.txt"}})
	if err != nil {
		t.Errorf("valid path rejected: %v", err)
	}
}

func TestValidateAttachmentPathsRejectsOutsideInbox(t *testing.T) {
	ts := newTestServer(t)
	// File just outside the conv dir but inside InboxDir root.
	convID := "conv-xyz"
	convDir := filepath.Join(ts.server.cfg.InboxDir, convID)
	os.MkdirAll(convDir, 0700)

	// Attacker supplies a path in a different conv's dir.
	otherConv := filepath.Join(ts.server.cfg.InboxDir, "other-conv")
	os.MkdirAll(otherConv, 0700)
	otherFile := filepath.Join(otherConv, "leak.txt")
	os.WriteFile(otherFile, []byte("leak"), 0600)

	err := ts.server.validateAttachmentPaths(convID, []core.Attachment{{Path: otherFile, Name: "leak.txt"}})
	if err == nil {
		t.Error("cross-conversation path should have been rejected")
	}
}
