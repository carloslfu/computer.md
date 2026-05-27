// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// mockOpenAIHandler returns a closure that responds to SendText
// requests with the supplied summary text. Captures the request body
// so tests can assert what the compactor sent.
func mockOpenAIHandler(t *testing.T, summary string, capturedBody *[]byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		if capturedBody != nil {
			*capturedBody = append((*capturedBody)[:0], buf...)
		}
		resp := map[string]interface{}{
			"id":     "resp_test",
			"status": "completed",
			"output": []map[string]interface{}{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]string{
						{"type": "output_text", "text": summary},
					},
				},
			},
			"usage": map[string]int{
				"input_tokens":  200,
				"output_tokens": 50,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// newTestCompactor wires a Compactor against a manager client pointed at
// the supplied test server. broker is a no-op.
func newTestCompactor(t *testing.T, server *httptest.Server) *Compactor {
	t.Helper()
	c := managerclient.NewClient("test-key")
	// Keep compactor tests on the real OpenAI manager client shape while
	// routing requests to httptest instead of the upstream API.
	managerclient.SetBaseURLForTests(c, server.URL)
	return NewCompactor(c, managerclient.DefaultModelID, noopBroker{})
}

// TestCompactorReplacesMiddleWithSummary is the core integration test:
// given a long message list, compaction should keep the initial prefix
// and the recent tail verbatim, and replace everything between with a
// single synthetic message containing the model's summary.
func TestCompactorReplacesMiddleWithSummary(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(mockOpenAIHandler(t, "GOAL: read the docs\nCOMPLETED ACTIONS:\n1. Fetched URL\n2. Parsed text\nCURRENT STATE: read 5 pages so far.\nOPEN QUESTIONS: none.\nKEY VALUES: url=https://example.com/docs", &captured))
	defer server.Close()

	compactor := newTestCompactor(t, server)

	// 3 initial + 30 loop messages. tail = KeepRecentPairs * 2 = 20.
	// We expect: 3 initial + 1 synthetic + 20 tail = 24 messages out.
	initMsgCount := 3
	messages := make([]managerclient.Message, 0, 33)
	for i := 0; i < initMsgCount; i++ {
		messages = append(messages, managerclient.Message{Role: "user", Content: "initial msg " + string(rune('a'+i))})
	}
	for i := 0; i < 30; i++ {
		role := "assistant"
		if i%2 == 1 {
			role = "user"
		}
		messages = append(messages, managerclient.Message{Role: role, Content: "loop msg " + string(rune('A'+i%26))})
	}
	if len(messages) != initMsgCount+30 {
		t.Fatalf("setup: expected %d msgs, got %d", initMsgCount+30, len(messages))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := compactor.Compact(ctx, "task-1", "convo-1", messages, initMsgCount)
	if err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}

	expectedLen := initMsgCount + 1 + (KeepRecentPairs * 2)
	if len(out) != expectedLen {
		t.Fatalf("output length = %d, want %d", len(out), expectedLen)
	}

	// First initMsgCount messages must be unchanged (verbatim prefix).
	for i := 0; i < initMsgCount; i++ {
		if got, want := out[i].Content, messages[i].Content; got != want {
			t.Errorf("prefix[%d] = %q, want %q", i, got, want)
		}
	}

	// The synthetic message is at position initMsgCount.
	synthetic := out[initMsgCount]
	if synthetic.Role != "user" {
		t.Errorf("synthetic msg role = %q, want %q", synthetic.Role, "user")
	}
	content, ok := synthetic.Content.(string)
	if !ok {
		t.Fatalf("synthetic msg content is not a string: %T", synthetic.Content)
	}
	if !strings.Contains(content, "<task_history_summary>") {
		t.Errorf("synthetic msg missing summary wrapper: %q", content)
	}
	if !strings.Contains(content, "GOAL: read the docs") {
		t.Errorf("synthetic msg missing model output: %q", content)
	}

	// Last KeepRecentPairs*2 messages must be unchanged (verbatim tail).
	tail := KeepRecentPairs * 2
	for i := 0; i < tail; i++ {
		got := out[len(out)-tail+i].Content
		want := messages[len(messages)-tail+i].Content
		if got != want {
			t.Errorf("tail[%d] = %q, want %q", i, got, want)
		}
	}

	// The transcript sent to the model must mention the collapsed turns
	// (not the prefix/tail).
	if len(captured) == 0 {
		t.Fatal("did not capture the request body sent to the mock model")
	}
	body := string(captured)
	if !strings.Contains(body, "loop msg") {
		t.Errorf("request body missing collapsed-turn content: %q", body[:min(200, len(body))])
	}
}

// TestCompactorNoOpBelowThreshold: when message count is at or below
// initMsgCount + tail, Compact is a no-op and returns the input
// unchanged. This guards the early-return short-circuit in Compact.
func TestCompactorNoOpBelowThreshold(t *testing.T) {
	server := httptest.NewServer(mockOpenAIHandler(t, "should not be called", nil))
	defer server.Close()

	compactor := newTestCompactor(t, server)

	tail := KeepRecentPairs * 2
	messages := make([]managerclient.Message, 0, 2+tail)
	for i := 0; i < 2+tail; i++ {
		messages = append(messages, managerclient.Message{Role: "user", Content: "msg"})
	}

	out, err := compactor.Compact(context.Background(), "", "", messages, 2)
	if err != nil {
		t.Fatalf("Compact returned error on no-op case: %v", err)
	}
	if len(out) != len(messages) {
		t.Errorf("no-op should return same length, got %d want %d", len(out), len(messages))
	}
}

// TestCompactorFailureReturnsOriginal: when the model call fails, the
// compactor returns the original messages unchanged so the agent loop
// continues. Failure must not lose history.
func TestCompactorFailureReturnsOriginal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	compactor := newTestCompactor(t, server)

	tail := KeepRecentPairs * 2
	messages := make([]managerclient.Message, 0, 3+30)
	for i := 0; i < 3+30; i++ {
		messages = append(messages, managerclient.Message{Role: "user", Content: "msg"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := compactor.Compact(ctx, "", "", messages, 3)
	if err == nil {
		t.Fatal("expected an error from the mock 500, got nil")
	}
	if len(out) != len(messages) {
		t.Errorf("on failure, output should match input length (got %d want %d)", len(out), len(messages))
	}
	_ = tail
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
