// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	encKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	db, err := persistence.Open(dbPath, encKey)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAddMessageWithAttachmentsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	store := NewTaskStore(db)

	// CreateTask auto-inserts a user message; use it end-to-end so we
	// cover the same path the HTTP layer hits in production.
	_, err := store.CreateTask("conv-1", "look at this",
		Attachment{Name: "stored.png", Path: "/tmp/stored.png", MIME: "image/png", Size: 42, Original: "photo.png"},
		Attachment{Name: "stored.csv", Path: "/tmp/stored.csv", MIME: "text/csv", Size: 100, Original: "data.csv"},
	)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	msgs, err := store.GetMessages("conv-1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != "user" || m.Content != "look at this" {
		t.Errorf("unexpected message content: role=%q content=%q", m.Role, m.Content)
	}
	if len(m.Attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(m.Attachments))
	}
	if m.Attachments[0].Original != "photo.png" || m.Attachments[1].Original != "data.csv" {
		t.Errorf("attachment originals: got %+v", m.Attachments)
	}
}

func TestGetMessagesWithoutAttachments(t *testing.T) {
	db := openTestDB(t)
	store := NewTaskStore(db)

	// CreateTask seeds the conversation row (FK target) and inserts the
	// user message with no attachments — the non-attachment path we want
	// to verify.
	if _, err := store.CreateTask("conv-2", "just text"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	msgs, err := store.GetMessages("conv-2")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if len(msgs[0].Attachments) != 0 {
		t.Errorf("expected no attachments, got %+v", msgs[0].Attachments)
	}
}

func TestBuildMessageContentNoAttachmentsReturnsString(t *testing.T) {
	result := buildMessageContent(ConversationMessage{
		Role:    "user",
		Content: "hello there",
	})
	if s, ok := result.(string); !ok || s != "hello there" {
		t.Errorf("expected bare string %q, got %T=%v", "hello there", result, result)
	}
}

func TestBuildMessageContentInlinesImages(t *testing.T) {
	dir := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\nrealcontent")
	path := filepath.Join(dir, "x.png")
	if err := os.WriteFile(path, png, 0600); err != nil {
		t.Fatal(err)
	}

	result := buildMessageContent(ConversationMessage{
		Role:    "user",
		Content: "what do you make of this",
		Attachments: []Attachment{
			{Name: "x.png", Path: path, MIME: "image/png", Size: int64(len(png)), Original: "photo.png"},
		},
	})

	blocks, ok := result.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected []map blocks, got %T: %v", result, result)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected text + image block, got %d", len(blocks))
	}
	// Text block should precede the image so the model sees the user's
	// instruction before the inline bytes.
	if blocks[0]["type"] != "text" {
		t.Fatalf("expected text block first, got %q", blocks[0]["type"])
	}
	txt, _ := blocks[0]["text"].(string)
	if !strings.Contains(txt, path) {
		t.Errorf("text should reference path %q; got %q", path, txt)
	}
	if !strings.Contains(txt, "what do you make of this") {
		t.Errorf("text should include user's message; got %q", txt)
	}
	if !strings.Contains(txt, "inline image bytes are included in this same message") {
		t.Errorf("text should tell the manager the image bytes are inline; got %q", txt)
	}
	if !strings.Contains(txt, "Do not tell the customer the attachment content is unavailable") {
		t.Errorf("text should prohibit false attachment-unavailable replies; got %q", txt)
	}
	if blocks[1]["type"] != "image" {
		t.Errorf("expected second block to be image, got %q", blocks[1]["type"])
	}
	src, _ := blocks[1]["source"].(map[string]string)
	if src["media_type"] != "image/png" {
		t.Errorf("expected media_type=image/png, got %q", src["media_type"])
	}
	if _, err := base64.StdEncoding.DecodeString(src["data"]); err != nil {
		t.Errorf("image data not valid base64: %v", err)
	}
}

func TestBuildMessageContentNonImagesAreTextOnly(t *testing.T) {
	result := buildMessageContent(ConversationMessage{
		Role:    "user",
		Content: "process this",
		Attachments: []Attachment{
			{Name: "x.csv", Path: "/tmp/x.csv", MIME: "text/csv", Size: 1000, Original: "report.csv"},
		},
	})
	// With no image blocks we should fall through to a bare string so the
	// simpler API payload path kicks in.
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected bare string, got %T: %v", result, result)
	}
	if !strings.Contains(s, "report.csv") || !strings.Contains(s, "/tmp/x.csv") {
		t.Errorf("expected filename and path in text: %q", s)
	}
	if !strings.Contains(s, "process this") {
		t.Errorf("expected user text retained: %q", s)
	}
}

func TestBuildMessageContentSkipsOversizedImage(t *testing.T) {
	dir := t.TempDir()
	// File larger than the inline cap.
	big := make([]byte, maxInlineImageBytes+1024)
	path := filepath.Join(dir, "big.png")
	if err := os.WriteFile(path, big, 0600); err != nil {
		t.Fatal(err)
	}
	result := buildMessageContent(ConversationMessage{
		Role:    "user",
		Content: "huge",
		Attachments: []Attachment{
			{Name: "big.png", Path: path, MIME: "image/png", Size: int64(len(big))},
		},
	})
	blocks, ok := result.([]map[string]interface{})
	if !ok {
		// When no image is inlined, buildMessageContent falls back to a
		// plain text string. That's also acceptable here.
		if _, isStr := result.(string); !isStr {
			t.Fatalf("expected string or blocks, got %T", result)
		}
		return
	}
	// If blocks were emitted, the only block should be text — the image
	// was skipped because it exceeds the cap.
	if blocks[0]["type"] == "image" {
		t.Error("oversized image should have been skipped, not inlined")
	}
}

func TestBuildMessageContentCapsImageCount(t *testing.T) {
	dir := t.TempDir()
	atts := make([]Attachment, 0, maxImagesPerMessage+3)
	small := []byte("\x89PNG\r\n\x1a\npayload")
	for i := 0; i < maxImagesPerMessage+3; i++ {
		p := filepath.Join(dir, "img.png")
		if i > 0 {
			p = filepath.Join(dir, strings.Repeat("a", i)+".png")
		}
		os.WriteFile(p, small, 0600)
		atts = append(atts, Attachment{
			Name:     filepath.Base(p),
			Path:     p,
			MIME:     "image/png",
			Size:     int64(len(small)),
			Original: filepath.Base(p),
		})
	}
	result := buildMessageContent(ConversationMessage{
		Role:        "user",
		Content:     "many",
		Attachments: atts,
	})
	blocks, ok := result.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected blocks, got %T", result)
	}
	imageCount := 0
	for _, b := range blocks {
		if b["type"] == "image" {
			imageCount++
		}
	}
	if imageCount > maxImagesPerMessage {
		t.Errorf("expected at most %d images, got %d", maxImagesPerMessage, imageCount)
	}
}

func TestBuildMessagesPropagatesAttachments(t *testing.T) {
	msgs := []*Message{
		{
			Role:    "user",
			Content: "hey",
			Type:    "text",
			Attachments: []Attachment{
				{Name: "x.png", Path: "/tmp/x.png", MIME: "image/png", Size: 10},
			},
		},
	}
	conv := NewContextBuilder("").WithMessages(msgs).BuildMessages()
	if len(conv) != 1 {
		t.Fatalf("expected 1 conversation message, got %d", len(conv))
	}
	if len(conv[0].Attachments) != 1 {
		t.Errorf("attachments should flow through BuildMessages, got %v", conv[0].Attachments)
	}
}

func TestShouldAnswerImageAttachmentDirectly(t *testing.T) {
	msgs := []ConversationMessage{{
		Role:    "user",
		Content: "Describe what you see.",
		Attachments: []Attachment{{
			Name: "x.png",
			MIME: "image/png",
		}},
	}}
	if !shouldAnswerImageAttachmentDirectly("Describe what you see.", msgs) {
		t.Fatal("describe-image prompt should use direct attachment path")
	}
}

func TestShouldAnswerImageAttachmentDirectlySkipsComputerActions(t *testing.T) {
	msgs := []ConversationMessage{{
		Role:    "user",
		Content: "Open Chrome and compare this screenshot to the page.",
		Attachments: []Attachment{{
			Name: "x.png",
			MIME: "image/png",
		}},
	}}
	if shouldAnswerImageAttachmentDirectly("Open Chrome and compare this screenshot to the page.", msgs) {
		t.Fatal("computer-action prompt should stay in the normal tool loop")
	}
}
