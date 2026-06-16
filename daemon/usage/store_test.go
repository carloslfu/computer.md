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

// insertOnDay writes a single usage_records row pinned to a specific
// YYYY-MM-DD day, bypassing Record's time.Now() bucketing so boundary
// behavior can be tested deterministically.
func insertOnDay(t *testing.T, store *Store, day, model, convo string, output int64) {
	t.Helper()
	_, err := store.db.Conn().Exec(`
		INSERT INTO usage_records (day, model, conversation_id, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, updated_at)
		VALUES (?, ?, ?, 0, ?, 0, 0, CURRENT_TIMESTAMP)
	`, day, model, convo, output)
	if err != nil {
		t.Fatalf("insert on %s: %v", day, err)
	}
}

// TestStore_AggregateHalfOpenNoBoundaryDoubleCount — the period-boundary
// money bug. Two adjacent billing periods can share a boundary day (period
// N's inclusive last day == period N+1's first day) whenever a Stripe
// subscription anchor is not midnight UTC. AggregateHalfOpen takes the NEXT
// period's start as an EXCLUSIVE upper bound (exactly what the platform now
// emits as period_end_exclusive), so the boundary day belongs to period N+1
// only and is never summed in both periods.
//
// This test feeds the daemon the SAME bounds the platform sends — it does NOT
// decrement the end in test code. With the old inclusive Aggregate (the no-op
// "fix") the boundary day was double-counted; with AggregateHalfOpen it is
// counted exactly once across the two adjacent periods.
func TestStore_AggregateHalfOpenNoBoundaryDoubleCount(t *testing.T) {
	store := NewStore(openTestDB(t))

	// gpt-5.4-mini output-only cost: 100k tokens * $4.50/M = $0.45 per row.
	const perDay = 0.45
	// Three days of usage straddling a shared boundary day (2026-06-30).
	insertOnDay(t, store, "2026-06-29", "gpt-5.4-mini", "c1", 100_000)
	insertOnDay(t, store, "2026-06-30", "gpt-5.4-mini", "c1", 100_000) // boundary day
	insertOnDay(t, store, "2026-07-01", "gpt-5.4-mini", "c1", 100_000)

	// Stripe period N: starts 2026-06-01..., exclusive end instant lands on
	// 2026-06-30 (anchor not midnight). The platform sends:
	//   period_start = 2026-06-01
	//   period_end (inclusive, display) = 2026-06-29
	//   period_end_exclusive = 2026-06-30   <-- next period's start
	// The daemon aggregates [period_start, period_end_exclusive).
	periodN, err := store.AggregateHalfOpen("2026-06-01", "2026-06-30", 0)
	if err != nil {
		t.Fatalf("aggregate period N: %v", err)
	}
	// Period N owns only 06-29; the boundary day 06-30 belongs to period N+1.
	approxEqual(t, periodN.TotalCostUSD, perDay, "period N owns days strictly before its exclusive end")
	if len(periodN.ByDay) != 1 {
		t.Fatalf("period N: expected 1 day (06-29), got %d: %+v", len(periodN.ByDay), periodN.ByDay)
	}
	// The reported inclusive end is endExclusive - 1.
	if periodN.Period.End != "2026-06-29" {
		t.Fatalf("period N reported inclusive end = %q, want 2026-06-29", periodN.Period.End)
	}

	// Stripe period N+1: starts 2026-06-30, exclusive end 2026-07-31. Platform
	// sends period_start = 2026-06-30, period_end_exclusive = 2026-07-31.
	periodNext, err := store.AggregateHalfOpen("2026-06-30", "2026-07-31", 0)
	if err != nil {
		t.Fatalf("aggregate period N+1: %v", err)
	}
	// Period N+1 owns the boundary day 06-30 AND 07-01 → 2 * $0.45.
	approxEqual(t, periodNext.TotalCostUSD, 2*perDay, "period N+1 owns the shared boundary day plus 07-01")
	if len(periodNext.ByDay) != 2 {
		t.Fatalf("period N+1: expected 2 days (06-30, 07-01), got %d: %+v", len(periodNext.ByDay), periodNext.ByDay)
	}

	// The no-double-count invariant: the two adjacent periods, summed, equal
	// the full recorded spend with NO day counted twice. Critically, these are
	// the platform's real shared-boundary bounds — no test-side decrement.
	combined := periodN.TotalCostUSD + periodNext.TotalCostUSD
	approxEqual(t, combined, 3*perDay, "adjacent periods stitched at the shared boundary sum to the full recorded spend, no day double-counted")
}

// TestStore_HalfOpenIsNotANoOpVsInclusive — the money regression guard the
// adversarial review demanded. It pins the difference between the inclusive
// Aggregate contract and the half-open billing contract on the SAME
// shared-boundary bounds, so the fix can never silently regress to a no-op.
//
// Scenario: two adjacent Stripe periods that share calendar boundary day
// 2026-06-30. The platform hands BOTH the inclusive end (06-30) and the
// exclusive end (next period's start, 06-30) — for a shared boundary these are
// the same string, which is exactly what makes the bug subtle.
//
//   - If billing had (wrongly) used inclusive Aggregate with those bounds —
//     period N = [06-01, 06-30] and period N+1 = [06-30, 07-31] — the boundary
//     day 06-30 satisfies day <= end for period N AND day >= start for period
//     N+1, so it is summed in BOTH periods. That is the double-count the review
//     flagged, reproduced here against the live SQL.
//   - AggregateHalfOpen [start, endExclusive) with the same bounds attributes
//     06-30 to period N+1 only. Each day belongs to exactly one period.
//
// The assertion is the gap between the two contracts: inclusive over-counts by
// exactly one boundary day; half-open does not. If a future edit makes the
// half-open form equivalent to inclusive (the "no-op" failure mode), the
// half-open combined total would jump to 4*perDay and this test fails.
func TestStore_HalfOpenIsNotANoOpVsInclusive(t *testing.T) {
	store := NewStore(openTestDB(t))

	const perDay = 0.45 // gpt-5.4-mini, 100k output tokens * $4.50/M
	insertOnDay(t, store, "2026-06-29", "gpt-5.4-mini", "c1", 100_000)
	insertOnDay(t, store, "2026-06-30", "gpt-5.4-mini", "c1", 100_000) // shared boundary day
	insertOnDay(t, store, "2026-07-01", "gpt-5.4-mini", "c1", 100_000)

	// The BUGGY contract: inclusive both-ends Aggregate on shared-boundary
	// bounds. period N inclusive end == period N+1 start == 2026-06-30.
	inclN, err := store.Aggregate("2026-06-01", "2026-06-30", 0)
	if err != nil {
		t.Fatalf("inclusive period N: %v", err)
	}
	inclNext, err := store.Aggregate("2026-06-30", "2026-07-31", 0)
	if err != nil {
		t.Fatalf("inclusive period N+1: %v", err)
	}
	inclusiveCombined := inclN.TotalCostUSD + inclNext.TotalCostUSD
	// Three days of real spend, but the inclusive contract counts the boundary
	// day twice → 4 days of attributed spend. This documents the bug.
	approxEqual(t, inclusiveCombined, 4*perDay,
		"inclusive Aggregate on shared-boundary bounds double-counts the boundary day (the bug)")

	// The FIXED contract: half-open billing aggregation with the exclusive end
	// the platform emits as period_end_exclusive (= next period's start).
	hoN, err := store.AggregateHalfOpen("2026-06-01", "2026-06-30", 0)
	if err != nil {
		t.Fatalf("half-open period N: %v", err)
	}
	hoNext, err := store.AggregateHalfOpen("2026-06-30", "2026-07-31", 0)
	if err != nil {
		t.Fatalf("half-open period N+1: %v", err)
	}
	halfOpenCombined := hoN.TotalCostUSD + hoNext.TotalCostUSD
	// Exactly the three recorded days, no day counted twice or dropped.
	approxEqual(t, halfOpenCombined, 3*perDay,
		"half-open AggregateHalfOpen counts the boundary day exactly once across adjacent periods (the fix)")

	// The fix must remove exactly one boundary day of over-count; if the
	// half-open form ever becomes a no-op equivalent to inclusive, this gap
	// collapses and the test fails.
	approxEqual(t, inclusiveCombined-halfOpenCombined, perDay,
		"half-open eliminates exactly the one boundary-day double-count vs inclusive")
}

// TestStore_AggregateHalfOpenSingleDay — a one-day half-open window
// [d, d+1) must include exactly day d.
func TestStore_AggregateHalfOpenSingleDay(t *testing.T) {
	store := NewStore(openTestDB(t))
	insertOnDay(t, store, "2026-06-15", "gpt-5.4-mini", "c1", 100_000) // $0.45
	insertOnDay(t, store, "2026-06-16", "gpt-5.4-mini", "c1", 100_000) // must be excluded

	sum, err := store.AggregateHalfOpen("2026-06-15", "2026-06-16", 0)
	if err != nil {
		t.Fatalf("single-day half-open aggregate: %v", err)
	}
	approxEqual(t, sum.TotalCostUSD, 0.45, "half-open [d, d+1) must include day d and exclude d+1")
	if len(sum.ByDay) != 1 {
		t.Fatalf("expected exactly 1 day, got %d: %+v", len(sum.ByDay), sum.ByDay)
	}
	if sum.Period.End != "2026-06-15" {
		t.Fatalf("reported inclusive end = %q, want 2026-06-15", sum.Period.End)
	}
}

// TestStore_AggregateHalfOpenRequiresBounds — defensive: empty bounds error
// rather than silently aggregating everything.
func TestStore_AggregateHalfOpenRequiresBounds(t *testing.T) {
	store := NewStore(openTestDB(t))
	if _, err := store.AggregateHalfOpen("", "2026-06-30", 0); err == nil {
		t.Fatalf("expected error when start is empty")
	}
	if _, err := store.AggregateHalfOpen("2026-06-01", "", 0); err == nil {
		t.Fatalf("expected error when endExclusive is empty")
	}
	if _, err := store.AggregateHalfOpen("2026-06-01", "not-a-date", 0); err == nil {
		t.Fatalf("expected error when endExclusive is malformed")
	}
}

// TestStore_AggregateIncludesInclusiveEndDay — the SPA display contract for
// Aggregate stays INCLUSIVE on the end, so a single-day window [d, d] must
// include day d.
func TestStore_AggregateIncludesInclusiveEndDay(t *testing.T) {
	store := NewStore(openTestDB(t))
	insertOnDay(t, store, "2026-06-15", "gpt-5.4-mini", "c1", 100_000) // $0.45

	sum, err := store.Aggregate("2026-06-15", "2026-06-15", 0)
	if err != nil {
		t.Fatalf("single-day aggregate: %v", err)
	}
	approxEqual(t, sum.TotalCostUSD, 0.45, "single inclusive day [d,d] must include day d")
	if len(sum.ByDay) != 1 {
		t.Fatalf("expected exactly 1 day, got %d: %+v", len(sum.ByDay), sum.ByDay)
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
