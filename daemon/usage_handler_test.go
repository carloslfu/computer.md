// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/carloslfu/computer.md/daemon/usage"
)

// newUsageTestServer returns a minimal Server with a real persistence
// DB + usage.Store wired up. Other handler dependencies stay nil — the
// /api/usage handler only reads from the usage store.
func newUsageTestServer(t *testing.T) (*Server, *usage.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dir, "test.db"), "test-key-32chars-XXXXXXXXXXXXXXXX")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := usage.NewStore(db)
	return &Server{usageStore: store}, store
}

// approxEqual compares dollar amounts with tolerance for floating-point
// noise. The handler uses the same Cost() helpers as the store tests,
// so the same tolerance applies.
func approxEqualHTTP(t *testing.T, got, want float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s: got %.10f want %.10f", msg, got, want)
	}
}

// TestUsageEndpoint_HappyPath verifies the wire JSON shape end-to-end:
// record real usage, hit the handler, parse the response, verify
// per-model + total + top-conversations are all consistent.
func TestUsageEndpoint_HappyPath(t *testing.T) {
	srv, store := newUsageTestServer(t)

	// Two OpenAI manager calls in convo-1, one full model call without a conversation.
	store.Record("gpt-5.4-mini", "convo-1", manager.Usage{
		InputTokens: 10_000, OutputTokens: 1_000,
	})
	store.Record("gpt-5.4-mini", "convo-1", manager.Usage{
		InputTokens: 5_000, OutputTokens: 500,
	})
	store.Record("gpt-5.4", "", manager.Usage{
		InputTokens: 2_000, OutputTokens: 200,
	})

	req := httptest.NewRequest("GET", "/api/usage", nil)
	w := httptest.NewRecorder()
	srv.handleAIUsage(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var sum usage.Summary
	if err := json.Unmarshal(w.Body.Bytes(), &sum); err != nil {
		t.Fatalf("response is not valid Summary JSON: %v\n%s", err, w.Body.String())
	}

	// mini: 15k input x $0.75/M + 1.5k output x $4.50/M = 0.01125 + 0.00675 = 0.018
	// full: 2k input x $2.50/M + 0.2k output x $15/M = 0.005 + 0.003 = 0.008
	// Total: 0.026
	approxEqualHTTP(t, sum.TotalCostUSD, 0.026, "total cost")
	if len(sum.ByModel) != 2 {
		t.Errorf("expected 2 models in by_model, got %d", len(sum.ByModel))
	}
	if len(sum.TopConversations) != 1 {
		t.Errorf("expected 1 top conversation (the full model call had no convo), got %d", len(sum.TopConversations))
	}
	if sum.TopConversations[0].ConversationID != "convo-1" {
		t.Errorf("top convo id: got %q", sum.TopConversations[0].ConversationID)
	}
}

// TestUsageEndpoint_DefaultPeriodIsCurrentMonth verifies that GET
// /api/usage with no query params returns a Summary scoped to the
// current calendar month (UTC). The SPA relies on this default when
// it doesn't have a Stripe-period override.
func TestUsageEndpoint_DefaultPeriodIsCurrentMonth(t *testing.T) {
	srv, _ := newUsageTestServer(t)

	req := httptest.NewRequest("GET", "/api/usage", nil)
	w := httptest.NewRecorder()
	srv.handleAIUsage(w, req)

	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}

	var sum usage.Summary
	_ = json.Unmarshal(w.Body.Bytes(), &sum)

	now := time.Now().UTC()
	wantStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	wantEnd := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	if sum.Period.Start != wantStart {
		t.Errorf("default start: got %q want %q", sum.Period.Start, wantStart)
	}
	if sum.Period.End != wantEnd {
		t.Errorf("default end: got %q want %q", sum.Period.End, wantEnd)
	}
}

// TestUsageEndpoint_PartialPeriodRejected — sending one of {start, end}
// without the other is ambiguous and must 400 rather than silently
// substituting a month boundary the caller didn't ask for.
func TestUsageEndpoint_PartialPeriodRejected(t *testing.T) {
	srv, _ := newUsageTestServer(t)

	for _, qs := range []string{"?start=2026-05-01", "?end=2026-05-31"} {
		req := httptest.NewRequest("GET", "/api/usage"+qs, nil)
		w := httptest.NewRecorder()
		srv.handleAIUsage(w, req)
		if w.Code != 400 {
			t.Errorf("query %q: expected 400, got %d", qs, w.Code)
		}
	}
}

// TestUsageEndpoint_TopCappedAt20 — the top param must clamp at 20 so
// a malicious or buggy client can't request a huge join.
func TestUsageEndpoint_TopCappedAt20(t *testing.T) {
	srv, store := newUsageTestServer(t)

	// 30 distinct conversations so we have plenty to clamp from.
	for i := 0; i < 30; i++ {
		store.Record("gpt-5.4-mini", "c-"+string(rune('a'+i)), manager.Usage{OutputTokens: 1_000})
	}

	req := httptest.NewRequest("GET", "/api/usage?top=100", nil)
	w := httptest.NewRecorder()
	srv.handleAIUsage(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	var sum usage.Summary
	_ = json.Unmarshal(w.Body.Bytes(), &sum)
	if len(sum.TopConversations) > 20 {
		t.Errorf("top should clamp at 20, got %d", len(sum.TopConversations))
	}
}

// TestUsageEndpoint_NoStoreReturns503 — defensive: a misconfigured
// build path must surface a clear "not configured" rather than a
// crashing nil deref or a false-zero report.
func TestUsageEndpoint_NoStoreReturns503(t *testing.T) {
	srv := &Server{} // no usageStore
	req := httptest.NewRequest("GET", "/api/usage", nil)
	w := httptest.NewRecorder()
	srv.handleAIUsage(w, req)
	if w.Code != 503 {
		t.Errorf("expected 503 when usage store missing, got %d", w.Code)
	}
}

// TestUsageEndpoint_MethodNotAllowed — non-GET methods should fail
// fast (no quiet body-eating).
func TestUsageEndpoint_MethodNotAllowed(t *testing.T) {
	srv, _ := newUsageTestServer(t)
	req := httptest.NewRequest("POST", "/api/usage", nil)
	w := httptest.NewRecorder()
	srv.handleAIUsage(w, req)
	if w.Code != 405 {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
