// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"path/filepath"
	"testing"

	"github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// openTestDB stands up a real SQLCipher-encrypted SQLite DB in a temp
// directory. Real over mocked because the schema includes the upsert
// constraint and we want to verify it actually works.
func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dir, "test.db"), "test-key-32chars-XXXXXXXXXXXXXXXX")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// today returns the current UTC date string the store uses internally,
// so tests can construct expected period bounds without time-zone slop.
func today() string {
	start, _ := CurrentMonthBounds()
	_ = start
	// Reuse the store's own formatting (UTC YYYY-MM-DD) by calling
	// Record once and reading back — but for a fixed-string assertion
	// in tests we just hand-build it. Simpler: tests use period
	// [first-of-month, last-of-month] which always brackets today.
	return ""
}

// TestStore_RecordAndAggregate verifies the basic write → read path:
// record a few manager responses, then aggregate them. Pins the cost
// math + the by-model + by-day shapes end-to-end through SQLite.
func TestStore_RecordAndAggregate(t *testing.T) {
	store := NewStore(openTestDB(t))

	// Two OpenAI manager turns in a single conversation.
	err := store.Record("gpt-5.4-mini", "convo-1", manager.Usage{
		InputTokens: 10_000, OutputTokens: 1_000,
		CacheReadInputTokens: 100_000, CacheCreationInputTokens: 5_000,
	})
	if err != nil {
		t.Fatalf("record 1: %v", err)
	}
	err = store.Record("gpt-5.4-mini", "convo-1", manager.Usage{
		InputTokens: 5_000, OutputTokens: 500,
		CacheReadInputTokens: 80_000,
	})
	if err != nil {
		t.Fatalf("record 2: %v", err)
	}

	// One full model summarizer call, no conversation context (compactor-style).
	err = store.Record("gpt-5.4", "", manager.Usage{
		InputTokens: 2_000, OutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("record 3: %v", err)
	}

	// Aggregate over a wide window so today's rows are included.
	start, end := CurrentMonthBounds()
	sum, err := store.Aggregate(start, end, 10)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	// ── by-model: mini should have summed-up tokens; full model stands alone ──
	var mini, full *ModelBreakdown
	for i := range sum.ByModel {
		switch sum.ByModel[i].Model {
		case "gpt-5.4-mini":
			mini = &sum.ByModel[i]
		case "gpt-5.4":
			full = &sum.ByModel[i]
		}
	}
	if mini == nil || full == nil {
		t.Fatalf("expected both models in by_model, got %+v", sum.ByModel)
	}
	if mini.InputTokens != 15_000 {
		t.Errorf("mini input: got %d want 15000", mini.InputTokens)
	}
	if mini.OutputTokens != 1_500 {
		t.Errorf("mini output: got %d want 1500", mini.OutputTokens)
	}
	if mini.CacheReadTokens != 180_000 {
		t.Errorf("mini cache_read: got %d want 180000", mini.CacheReadTokens)
	}
	if mini.CacheCreateTokens != 5_000 {
		t.Errorf("mini cache_create: got %d want 5000", mini.CacheCreateTokens)
	}
	if full.InputTokens != 2_000 {
		t.Errorf("full model input: got %d want 2000", full.InputTokens)
	}

	// ── total cost: hand-compute and compare ──
	// mini: max(15k - 180k - 5k, 0) * 0.75/M + 1.5k * 4.50/M + 180k * 0.075/M
	//       = 0 + 0.00675 + 0.0135
	//       = 0.02025
	// full: 2k * 2.50/M + 0.2k * 15.00/M
	//       = 0.005 + 0.003 = 0.008
	// total = 0.02825
	approxEqual(t, sum.TotalCostUSD, 0.02825, "total cost across models")
	approxEqual(t, mini.CostUSD, 0.02025, "mini cost")
	approxEqual(t, full.CostUSD, 0.008, "full model cost")

	// ── top conversations: only convo-1 has cost (full model call had no convo) ──
	if len(sum.TopConversations) != 1 {
		t.Fatalf("expected 1 top conversation, got %d: %+v", len(sum.TopConversations), sum.TopConversations)
	}
	if sum.TopConversations[0].ConversationID != "convo-1" {
		t.Errorf("top convo id: got %q want convo-1", sum.TopConversations[0].ConversationID)
	}
	approxEqual(t, sum.TopConversations[0].CostUSD, 0.02025, "convo-1 cost = mini total")
}

// TestStore_UpsertDedupesByDayAndModel — two Records to the same
// (day, model, conversation) bucket must accumulate, not create two
// rows. This is the load-bearing correctness invariant for the table.
func TestStore_UpsertDedupesByDayAndModel(t *testing.T) {
	store := NewStore(openTestDB(t))

	// Two calls, same convo, same model. Tokens must SUM.
	for i := 0; i < 5; i++ {
		if err := store.Record("gpt-5.4-mini", "c1", manager.Usage{
			InputTokens: 1_000, OutputTokens: 100,
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	start, end := CurrentMonthBounds()
	sum, err := store.Aggregate(start, end, 10)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(sum.ByModel) != 1 {
		t.Fatalf("expected exactly 1 model row (manager), got %d", len(sum.ByModel))
	}
	if got := sum.ByModel[0].InputTokens; got != 5_000 {
		t.Errorf("expected 5000 summed input tokens, got %d", got)
	}
}

// TestStore_TopConversationsExcludesEmptyConvo — calls outside a chat
// (empty conversation_id) must not appear in the top-conversations
// list. They still contribute to the model and day totals.
func TestStore_TopConversationsExcludesEmptyConvo(t *testing.T) {
	store := NewStore(openTestDB(t))

	store.Record("gpt-5.4", "", manager.Usage{
		InputTokens: 500_000, OutputTokens: 50_000,
	})

	start, end := CurrentMonthBounds()
	sum, _ := store.Aggregate(start, end, 10)
	if len(sum.TopConversations) != 0 {
		t.Errorf("non-conversation usage should not appear in top_conversations, got %+v", sum.TopConversations)
	}
	// But by_model should still account for it.
	if len(sum.ByModel) != 1 {
		t.Errorf("expected by_model to include the full model row, got %+v", sum.ByModel)
	}
}

// TestStore_AggregateEmptyIsZero — fresh store with no records must
// not panic and must return well-formed empty slices, not nil.
func TestStore_AggregateEmptyIsZero(t *testing.T) {
	store := NewStore(openTestDB(t))

	start, end := CurrentMonthBounds()
	sum, err := store.Aggregate(start, end, 10)
	if err != nil {
		t.Fatalf("aggregate empty: %v", err)
	}
	if sum.TotalCostUSD != 0 {
		t.Errorf("empty store should have 0 cost, got %.4f", sum.TotalCostUSD)
	}
	if sum.ByModel == nil {
		t.Errorf("ByModel should be empty slice, not nil")
	}
	if sum.ByDay == nil {
		t.Errorf("ByDay should be empty slice, not nil")
	}
	if sum.TopConversations == nil {
		t.Errorf("TopConversations should be empty slice, not nil")
	}
}

// TestStore_RecordRequiresModel — defensive: an engine bug that
// forgets to pass the model name shouldn't silently lose attribution.
func TestStore_RecordRequiresModel(t *testing.T) {
	store := NewStore(openTestDB(t))
	err := store.Record("", "c1", manager.Usage{InputTokens: 1000})
	if err == nil {
		t.Fatalf("expected error when model is empty")
	}
}

// TestStore_AggregateRequiresPeriod — defensive: a malformed handler
// query shouldn't crash the daemon.
func TestStore_AggregateRequiresPeriod(t *testing.T) {
	store := NewStore(openTestDB(t))
	if _, err := store.Aggregate("", "", 10); err == nil {
		t.Fatalf("expected error when period is empty")
	}
	if _, err := store.Aggregate("2026-05-01", "", 10); err == nil {
		t.Fatalf("expected error when end is empty")
	}
}

// TestStore_UnpricedModelSurfacesInSummary — a row with a model we
// don't have pricing for must show up in UnpricedModels so the UI can
// warn the user instead of silently undercounting.
func TestStore_UnpricedModelSurfacesInSummary(t *testing.T) {
	store := NewStore(openTestDB(t))

	store.Record("gpt-someday-future-7", "c1", manager.Usage{
		InputTokens: 10_000, OutputTokens: 1_000,
	})

	start, end := CurrentMonthBounds()
	sum, _ := store.Aggregate(start, end, 10)
	found := false
	for _, m := range sum.UnpricedModels {
		if m == "gpt-someday-future-7" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unpriced model to be surfaced, got UnpricedModels=%v", sum.UnpricedModels)
	}
	// Cost contribution is 0 for unpriced models (deliberate — see
	// pricing.Cost docstring).
	for _, m := range sum.ByModel {
		if m.Model == "gpt-someday-future-7" && m.CostUSD != 0 {
			t.Errorf("unpriced model should contribute 0 cost, got %.4f", m.CostUSD)
		}
	}
}

// TestStore_TopConvosOrderedByCostDesc — ranking is the only thing
// the UI relies on for the "top 5" list, so pin it explicitly.
func TestStore_TopConvosOrderedByCostDesc(t *testing.T) {
	store := NewStore(openTestDB(t))

	// Three conversations, hand-built so ranking is unambiguous.
	store.Record("gpt-5.4-mini", "small", manager.Usage{OutputTokens: 1_000}) // $0.0045
	store.Record("gpt-5.4-mini", "big", manager.Usage{OutputTokens: 100_000}) // $0.45
	store.Record("gpt-5.4-mini", "mid", manager.Usage{OutputTokens: 10_000})  // $0.045

	start, end := CurrentMonthBounds()
	sum, _ := store.Aggregate(start, end, 10)
	if len(sum.TopConversations) != 3 {
		t.Fatalf("expected 3 conversations, got %d", len(sum.TopConversations))
	}
	wantOrder := []string{"big", "mid", "small"}
	for i, w := range wantOrder {
		if got := sum.TopConversations[i].ConversationID; got != w {
			t.Errorf("rank %d: got %q want %q", i, got, w)
		}
	}
}
