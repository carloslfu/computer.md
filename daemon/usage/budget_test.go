// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/carloslfu/computer.md/daemon/manager"
)

// fakeBudget is a stand-in for the platform's GET /api/machine/budget.
// Every budget test drives the BudgetTracker through its REAL Refresh()
// path against this server — there is no test-only budget setter, so
// the tests exercise exactly what runs in production: HTTP request +
// Bearer auth + JSON parse + cache.
type fakeBudget struct {
	server       *httptest.Server
	mu           sync.Mutex
	budget       float64
	mode         string
	start        string
	end          string
	endExclusive string // period_end_exclusive; emitted only when non-empty
	status       int    // 0 or 200 → serve the budget; anything else → that status code
	gotAuth  string // the Authorization header the daemon actually sent
	gotPath  string // the request path the daemon actually hit
	gotQuery string
	calls    int
}

func newFakeBudget(t *testing.T) *fakeBudget {
	t.Helper()
	start, end := CurrentMonthBounds()
	fb := &fakeBudget{budget: 200, start: start, end: end, status: 200}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.gotAuth = r.Header.Get("Authorization")
		fb.gotPath = r.URL.Path
		fb.gotQuery = r.URL.RawQuery
		fb.calls++
		if fb.status != 0 && fb.status != http.StatusOK {
			w.WriteHeader(fb.status)
			return
		}
		payload := map[string]interface{}{
			"ai_budget_usd":         fb.budget,
			"usage_budget_usd":      fb.budget,
			"monthly_usage_cap_usd": 250.0,
			"cap_reached_reason":    nil,
			"period_start":          fb.start,
			"period_end":            fb.end,
			"period_source":         "subscription",
			"manager_key_mode":      fb.mode,
		}
		// Only emit period_end_exclusive when the test sets it, so the
		// default-fake path also covers an OLDER platform that omits the
		// field (the daemon must fall back to nextDay(period_end)).
		if fb.endExclusive != "" {
			payload["period_end_exclusive"] = fb.endExclusive
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(fb.server.Close)
	return fb
}

func (fb *fakeBudget) set(budget float64) {
	fb.mu.Lock()
	fb.budget = budget
	fb.mu.Unlock()
}

func (fb *fakeBudget) setStatus(code int) {
	fb.mu.Lock()
	fb.status = code
	fb.mu.Unlock()
}

// setPeriod sets the inclusive start, inclusive display end, and the
// exclusive aggregation upper bound (the next period's start) the fake
// platform reports. Pass endExclusive == "" to emulate an older platform
// that doesn't send period_end_exclusive.
func (fb *fakeBudget) setPeriod(start, end, endExclusive string) {
	fb.mu.Lock()
	fb.start = start
	fb.end = end
	fb.endExclusive = endExclusive
	fb.mu.Unlock()
}

func (fb *fakeBudget) setMode(mode string) {
	fb.mu.Lock()
	fb.mode = mode
	fb.mu.Unlock()
}

func (fb *fakeBudget) seenAuth() string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.gotAuth
}

// trackerFor wires a BudgetTracker against the fake platform.
func trackerFor(store *Store, fb *fakeBudget) *BudgetTracker {
	return NewBudgetTracker(store, "vc-test", "health-tok-abc", fb.server.URL)
}

// outTokensFor returns the OpenAI manager output-token count costing ~costUSD
// ($15/MTok). Runtime func so non-integer division is fine.
func outTokensFor(costUSD float64) int {
	return int(costUSD * 1_000_000 / 4.50)
}

// seedSpend records a OpenAI manager call worth ~costUSD into the store.
func seedSpend(t *testing.T, store *Store, convo string, costUSD float64) {
	t.Helper()
	if err := store.Record("gpt-5.4-mini", convo, manager.Usage{
		OutputTokens: outTokensFor(costUSD),
	}); err != nil {
		t.Fatalf("seedSpend: %v", err)
	}
}

// ── The fetch join ──────────────────────────────────────────────────

// TestBudget_RefreshFetchesAndParsesAllFields pins the cross-codebase
// contract: the daemon's Budget struct must parse every field the
// platform's /api/machine/budget emits. A renamed key on either side
// would silently parse to zero → fail-open → enforcement dead. This
// test fails loudly if that drift happens.
func TestBudget_RefreshFetchesAndParsesAllFields(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(200)
	bt := trackerFor(store, fb)

	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.BudgetUSD != 200 {
		t.Errorf("budget didn't parse: got %.2f want 200", st.BudgetUSD)
	}
	if st.UsageBudgetUSD != 200 {
		t.Errorf("usage_budget_usd didn't parse: got %.2f want 200", st.UsageBudgetUSD)
	}
	if st.MonthlyUsageCapUSD == nil || *st.MonthlyUsageCapUSD != 250 {
		t.Errorf("monthly_usage_cap_usd didn't parse: got %+v want 250", st.MonthlyUsageCapUSD)
	}
	if !st.Enforced {
		t.Errorf("a fetched positive budget must engage enforcement")
	}
	if st.ResetsOn != fb.end {
		t.Errorf("period_end didn't parse: got %q want %q", st.ResetsOn, fb.end)
	}
}

// TestBudget_RefreshSendsBearerAuth verifies the daemon authenticates
// the fetch with its healthToken — the join that the middleware-307
// bug broke in production. The fake server captures the header.
func TestBudget_RefreshSendsBearerAuth(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	bt := trackerFor(store, fb)

	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := fb.seenAuth(); got != "Bearer health-tok-abc" {
		t.Errorf("daemon must send the healthToken as Bearer auth; server saw %q", got)
	}
	if fb.gotPath != "/api/machine/budget" {
		t.Errorf("daemon hit the wrong path: %q", fb.gotPath)
	}
}

// TestBudget_RefreshRejectsNon200 is the direct regression test for
// the production bug: the platform middleware 307-redirected the
// daemon's fetch, the daemon must treat that (and any non-200) as a
// failed fetch and stay fail-open — never parse a redirect/error body
// as a budget.
func TestBudget_RefreshRejectsNon200(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusInternalServerError} {
		store := NewStore(openTestDB(t))
		fb := newFakeBudget(t)
		fb.setStatus(code)
		bt := trackerFor(store, fb)

		if err := bt.Refresh(context.Background()); err == nil {
			t.Errorf("status %d: Refresh must return an error, not silently accept", code)
		}
		st, _ := bt.State()
		if st.Enforced {
			t.Errorf("status %d: a failed fetch must leave enforcement OFF (fail-open)", code)
		}
	}
}

// TestBudget_RefreshFailureKeepsLastKnown — once a budget is fetched,
// a later failed Refresh keeps the last-known value (fail-stale), not
// fail-open. We don't want a transient blip to disable enforcement.
func TestBudget_RefreshFailureKeepsLastKnown(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(50)
	bt := trackerFor(store, fb)

	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	// Platform now starts erroring.
	fb.setStatus(http.StatusInternalServerError)
	if err := bt.Refresh(context.Background()); err == nil {
		t.Fatalf("expected the second Refresh to error")
	}

	st, _ := bt.State()
	if !st.Enforced || st.BudgetUSD != 50 {
		t.Errorf("after a failed refresh the last-known $50 budget must persist; got enforced=%v budget=%.2f", st.Enforced, st.BudgetUSD)
	}
}

// ── Enforcement verdict ─────────────────────────────────────────────

// TestBudget_FailOpenWhenNeverFetched — before any successful fetch,
// enforcement is OFF regardless of spend.
func TestBudget_FailOpenWhenNeverFetched(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	bt := trackerFor(store, fb)
	// Deliberately do NOT call Refresh.

	seedSpend(t, store, "c1", 999.0)
	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Enforced || st.Paused {
		t.Errorf("no fetch yet → must fail-open; got %+v", st)
	}
}

// TestBudget_EnforcesWhenSpendExceedsBudget — the core verdict, via
// the real fetch path.
func TestBudget_EnforcesWhenSpendExceedsBudget(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(5)
	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	seedSpend(t, store, "c1", 7.0) // $7 spent against a $5 budget

	st, _ := bt.State()
	if !st.Paused {
		t.Errorf("$7 spend over $5 budget must pause; got %+v", st)
	}
}

// TestBudget_StateIncludesProxySpend pins hosted-tool AI accounting in the
// local task gate. The platform budget response excludes this machine's own
// spend, so State must net both manager-turn spend and proxy spend locally.
func TestBudget_StateIncludesProxySpend(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(5)
	bt := trackerFor(store, fb)
	bt.SetProxySpend(func() int { return 600 })
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.SpentUSD != 6 {
		t.Fatalf("State must include proxy spend; got spent %.2f want 6.00", st.SpentUSD)
	}
	if !st.Paused {
		t.Fatalf("$6 proxy spend over $5 budget must pause; got %+v", st)
	}
}

func TestBudget_LocalUsageRecordSpendCentsExcludesProxy(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(20)
	bt := trackerFor(store, fb)
	bt.SetProxySpend(func() int { return 600 })
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	seedSpend(t, store, "c1", 4.0)
	if got := bt.LocalUsageRecordSpendCents(); got != 400 {
		t.Fatalf("local usage-record spend must exclude proxy ledger: got %d want 400", got)
	}

	bt.SetManagerKeyMode("operator")
	if got := bt.LocalUsageRecordSpendCents(); got != 0 {
		t.Fatalf("operator-paid manager mode must not count VibeCraft credit spend: got %d", got)
	}
}

// TestBudget_DrainedPoolZeroPauses — a fetched budget of exactly 0 (a fully
// drained multi-machine pool, which the platform clamps to 0.0) must pause.
// Regression for the old `budget > 0` guard that let a drained pool keep
// spending on the remaining sibling machines past zero.
func TestBudget_DrainedPoolZeroPauses(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(0)
	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	seedSpend(t, store, "c1", 0.01) // any spend at all against a $0 pool
	st, _ := bt.State()
	if !st.Enforced {
		t.Errorf("a fetched budget of 0 is a KNOWN budget → must enforce; got %+v", st)
	}
	if !st.Paused {
		t.Errorf("a drained $0 pool must pause; got %+v", st)
	}
}

// TestBudget_NotPausedUnderBudget — fetched budget, spend below it.
func TestBudget_NotPausedUnderBudget(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(200)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())

	seedSpend(t, store, "c1", 2.0)
	st, _ := bt.State()
	if !st.Enforced {
		t.Errorf("expected enforced")
	}
	if st.Paused {
		t.Errorf("$2 under $200 must not pause")
	}
}

// TestBudget_NegativeFetchedBudgetPausesAsDrained — the current platform
// contract never sends negative funded budgets. If a stale/custom platform does,
// do not turn VibeCraft-metered usage into free unmetered spend; treat it as a
// drained known budget.
func TestBudget_NegativeFetchedBudgetPausesAsDrained(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(-1)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())

	seedSpend(t, store, "c1", 100_000.0)
	st, _ := bt.State()
	if !st.Enforced {
		t.Errorf("negative fetched budget is known and must enforce")
	}
	if !st.Paused {
		t.Errorf("negative fetched budget must pause as drained")
	}
}

// TestBudget_ExactlyAtBudgetPauses — spend == budget is the boundary;
// the whole allowance is consumed, so it pauses. We seed spend, read
// the exact recorded amount, then re-serve that as the budget.
func TestBudget_ExactlyAtBudgetPauses(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(1_000_000) // huge first, so State reports the raw spend
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())

	seedSpend(t, store, "c1", 10.0)
	probe, _ := bt.State()
	exact := probe.SpentUSD
	if exact <= 0 {
		t.Fatalf("precondition: expected non-zero seeded spend")
	}

	// Re-serve the budget as exactly the spent amount.
	fb.set(exact)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("re-Refresh: %v", err)
	}
	st, _ := bt.State()
	if !st.Paused {
		t.Errorf("spend (%.6f) exactly at budget (%.6f) must pause — boundary is >=", st.SpentUSD, st.BudgetUSD)
	}
}

// TestBudget_StateResetDateFromFetchedPeriod — ResetsOn reflects the
// fetched billing period, not a calendar-month guess.
func TestBudget_StateResetDateFromFetchedPeriod(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.start, fb.end = "2026-07-15", "2026-08-15"
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())

	st, _ := bt.State()
	if st.ResetsOn != "2026-08-15" {
		t.Errorf("ResetsOn should be the fetched period end; got %q", st.ResetsOn)
	}
}

// TestBudget_StateUsesHalfOpenPeriodEndExclusive — the money-critical
// regression test for the period-boundary double-count. When a Stripe
// subscription's anchor is NOT midnight UTC, period N's exclusive end instant
// lands mid-day on some date D, and period N+1 starts at that same instant.
// Truncated to UTC days that means:
//
//	period N:   period_start=2026-06-01, period_end=2026-06-30 (== D),
//	            period_end_exclusive=2026-06-30 (== D, the next period's start)
//	period N+1: period_start=2026-06-30 (== D)
//
// So period N's INCLUSIVE display end (2026-06-30) and period N+1's start are
// the SAME calendar day — the shared boundary day. The OLD inclusive
// aggregation summed [period_start, period_end] INCLUSIVE, counting D's spend
// in BOTH periods. State() must instead aggregate the HALF-OPEN window
// [period_start, period_end_exclusive) so D belongs to period N+1 only.
//
// The test sets period_end == period_end_exclusive (the real shared-boundary
// shape), so it genuinely distinguishes the half-open fix from the old
// inclusive query: only the half-open path excludes D from period N.
func TestBudget_StateUsesHalfOpenPeriodEndExclusive(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(100)
	// Shared boundary day 2026-06-30: it is period N's inclusive end AND the
	// exclusive aggregation bound (== period N+1's start).
	fb.setPeriod("2026-06-01", "2026-06-30", "2026-06-30")
	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// $0.45 strictly inside period N (06-29), $0.45 ON the shared boundary day
	// (06-30 — belongs to period N+1). 100k output * $4.50/M = $0.45/day.
	insertOnDay(t, store, "2026-06-29", "gpt-5.4-mini", "c1", 100_000)
	insertOnDay(t, store, "2026-06-30", "gpt-5.4-mini", "c1", 100_000)

	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	// Period N's enforced spend must be $0.45 only — the boundary day excluded.
	// The OLD inclusive query would report $0.90 (boundary day double-counted).
	approxEqual(t, st.SpentUSD, 0.45, "period N enforced spend excludes the shared boundary day (half-open end)")
	// The display end stays the inclusive boundary day.
	if st.ResetsOn != "2026-06-30" {
		t.Errorf("ResetsOn (display) should be the inclusive period end; got %q", st.ResetsOn)
	}
	if st.Paused {
		t.Errorf("with $0.45 spent against a $100 budget the period must not be paused")
	}
}

// TestBudget_StateFallsBackToNextDayWhenPlatformOmitsExclusiveEnd — an OLDER
// platform that doesn't send period_end_exclusive must still aggregate the
// full inclusive period (no day silently dropped). The daemon falls back to
// nextDay(period_end). Here the period has no shared boundary day, so the full
// inclusive [start, end] window's spend is enforced.
func TestBudget_StateFallsBackToNextDayWhenPlatformOmitsExclusiveEnd(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(100)
	fb.setPeriod("2026-06-01", "2026-06-30", "") // no period_end_exclusive
	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	insertOnDay(t, store, "2026-06-29", "gpt-5.4-mini", "c1", 100_000)
	insertOnDay(t, store, "2026-06-30", "gpt-5.4-mini", "c1", 100_000) // inclusive last day

	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	// Both days fall inside the inclusive period — the fallback must not drop
	// the inclusive last day.
	approxEqual(t, st.SpentUSD, 0.90, "fallback covers the full inclusive period including its last day")
}

// ── Threshold warnings ──────────────────────────────────────────────

type capturedNote struct{ kind, title, body, priority string }

func capturingNotifier() (NotifyFunc, *[]capturedNote) {
	var notes []capturedNote
	return func(kind, title, body, priority string) {
		notes = append(notes, capturedNote{kind, title, body, priority})
	}, &notes
}

// TestBudget_Warn80FiresOnceOnCrossing — the 80% warning fires exactly
// once when spend crosses the threshold, then not again.
func TestBudget_Warn80FiresOnceOnCrossing(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(10)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())
	notify, notes := capturingNotifier()
	bt.SetNotifier(notify)

	// $7 — under 80% ($8). No warning.
	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(7.0)})
	if len(*notes) != 0 {
		t.Fatalf("no warning expected under 80%%, got %+v", *notes)
	}
	// +$1.50 → $8.50, crosses 80%. One warning.
	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(1.5)})
	if len(*notes) != 1 || (*notes)[0].title != "Usage credit 80% used" {
		t.Fatalf("expected exactly one 80%% warning, got %+v", *notes)
	}
	// +$0.30 → still 80–100%. No repeat.
	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(0.3)})
	if len(*notes) != 1 {
		t.Errorf("80%% warning must fire once per period, got %d", len(*notes))
	}
}

// TestBudget_Warn100FiresHighPriority — crossing 100% fires the
// budget-reached notification at high priority.
func TestBudget_Warn100FiresHighPriority(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(5)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())
	notify, notes := capturingNotifier()
	bt.SetNotifier(notify)

	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(20.0)})

	var got *capturedNote
	for i := range *notes {
		if (*notes)[i].title == "Usage credit exhausted" {
			got = &(*notes)[i]
		}
	}
	if got == nil {
		t.Fatalf("expected a 'Usage credit exhausted' notification, got %+v", *notes)
	}
	if got.priority != "high" {
		t.Errorf("credits-exhausted must be high priority, got %q", got.priority)
	}
}

func TestBudget_NoLocalWarningWhenNegativeFetchedBudgetIsUsed(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(-1)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())
	notify, notes := capturingNotifier()
	bt.SetNotifier(notify)

	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(9999.0)})
	if len(*notes) != 0 {
		t.Fatalf("negative fetched budget should pause without noisy $0 warning, got %+v", *notes)
	}
}

func TestBudget_ReserveRequiresHeadroomAndReleases(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(2)
	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	seedSpend(t, store, "c1", 0.50)

	release, err := bt.Reserve("gpt-5.4-mini", "c1")
	if err != nil {
		t.Fatalf("reserve should allow with more than $1 headroom: %v", err)
	}
	st, _ := bt.State()
	if st.SpentUSD < 0.49 || st.SpentUSD > 0.51 {
		t.Fatalf("reserved headroom should not be reported as actual spend, got %.2f", st.SpentUSD)
	}
	if got := bt.LocalReservedSpendCents(); got != managerCallReserveCents {
		t.Fatalf("reserved headroom should be available to proxy budgeting, got %d", got)
	}
	release()
	st, _ = bt.State()
	if st.SpentUSD < 0.49 || st.SpentUSD > 0.51 {
		t.Fatalf("release should remove reserved headroom, got %.2f", st.SpentUSD)
	}

	fb.set(1)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := bt.Reserve("gpt-5.4-mini", "c1"); err == nil {
		t.Fatal("reserve must block when less than $1 headroom remains")
	}
}

func TestBudget_RefreshManagerKeyModeFromPlatformIsAuthoritative(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.setMode("operator")
	bt := trackerFor(store, fb)
	bt.SetManagerKeyMode("platform")
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	seedSpend(t, store, "c1", 20)
	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.ManagerKeyMode != "operator" || st.Enforced || st.Paused {
		t.Fatalf("platform manager_key_mode should switch stale local mode to operator, got %+v", st)
	}
}

// TestBudget_NoWarningWhenNeverFetched — recording usage before any
// budget fetch must not warn or crash.
func TestBudget_NoWarningWhenNeverFetched(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	bt := trackerFor(store, fb) // no Refresh
	notify, notes := capturingNotifier()
	bt.SetNotifier(notify)

	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(500.0)})
	if len(*notes) != 0 {
		t.Errorf("fail-open (no budget) must not warn, got %+v", *notes)
	}
}

// TestBudget_RecordForwardsToStore — the tracker's Record must persist
// usage (it IS the engine's UsageRecorder; a dropped write would
// silently break all spend tracking).
func TestBudget_RecordForwardsToStore(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	bt := trackerFor(store, fb)

	if err := bt.Record("gpt-5.4-mini", "c1", manager.Usage{
		InputTokens: 1000, OutputTokens: 500,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	start, end := CurrentMonthBounds()
	sum, _ := store.Aggregate(start, end, 0)
	if sum.TotalCostUSD <= 0 {
		t.Errorf("Record must forward to the store; aggregate shows zero spend")
	}
}

func TestBudget_OperatorKeyModeRecordsButDoesNotEnforceOrReportSpend(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(1)
	bt := trackerFor(store, fb)
	bt.SetManagerKeyMode("operator")
	notify, notes := capturingNotifier()
	bt.SetNotifier(notify)

	if err := bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(20)}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	st, err := bt.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Enforced || st.Paused {
		t.Fatalf("operator-owned manager key usage must not enforce VibeCraft credits: %+v", st)
	}
	if st.SpentUSD <= 0 {
		t.Fatalf("operator-owned usage should still be locally visible, got %.2f", st.SpentUSD)
	}
	if len(*notes) != 0 {
		t.Fatalf("operator-owned usage should not fire VibeCraft credit warnings, got %+v", *notes)
	}
	fb.mu.Lock()
	gotQuery := fb.gotQuery
	fb.mu.Unlock()
	if !strings.Contains(gotQuery, "spent_cents=0") {
		t.Fatalf("operator-owned usage should report zero VibeCraft spend to platform, query=%q", gotQuery)
	}
}

// TestBudget_RefreshIncludesProxySpend is the regression for
// daemon-manager-ai-1: hosted-tool AI spend (the ai_proxy ledger) must be
// reported to the platform alongside manager-turn spend, or it never bills
// back. With no manager turns recorded, the reported spend is exactly the
// proxy spend.
func TestBudget_RefreshIncludesProxySpend(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(100)
	bt := trackerFor(store, fb)
	bt.SetManagerKeyMode("platform")
	bt.SetProxySpend(func() int { return 700 }) // $7 of hosted-tool AI

	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	fb.mu.Lock()
	gotQuery := fb.gotQuery
	fb.mu.Unlock()
	if !strings.Contains(gotQuery, "spent_cents=700") {
		t.Fatalf("hosted-tool AI proxy spend must be reported to the platform; query=%q", gotQuery)
	}
}

// J12 — TestBudget_RefreshParsesNonAIConfig verifies that the non-AI
// plan-config fields (plan_name, backup_cadence, region) come through
// the same poll the usage budget rides on (Track D5). The daemon's
// backup module + dashboard read these off the cached Snapshot().
func TestBudget_RefreshParsesNonAIConfig(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := &fakeBudget{
		budget: 100, start: "2026-05-02", end: "2026-06-02", status: 200,
	}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ai_budget_usd":    100.0,
			"usage_budget_usd": 100.0,
			"period_start":     "2026-05-02",
			"period_end":       "2026-06-02",
			"period_source":    "subscription",
			"plan_name":        "Business",
			"backup_cadence":   "aws-dlm-hourly-daily-weekly-s3-continuous-30d",
			"region":           "us-east-1",
			"manager_key_mode": "platform",
		})
	}))
	t.Cleanup(fb.server.Close)

	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := bt.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot is nil after successful refresh")
	}
	if snap.PlanName != "Business" {
		t.Errorf("PlanName: got %q want Business", snap.PlanName)
	}
	if snap.BackupCadence != "aws-dlm-hourly-daily-weekly-s3-continuous-30d" {
		t.Errorf("BackupCadence: got %q", snap.BackupCadence)
	}
	if snap.Region != "us-east-1" {
		t.Errorf("Region: got %q want us-east-1", snap.Region)
	}
	if snap.ManagerKeyMode != "platform" {
		t.Errorf("ManagerKeyMode: got %q want platform", snap.ManagerKeyMode)
	}
}

// J12 — TestBudget_RefreshHandlesMissingNonAIFields verifies the
// backward-compat path: an older platform that doesn't return the D5
// extension fields still works — the usage-budget refresh succeeds and
// the non-AI fields default to empty strings (daemon callers cope).
func TestBudget_RefreshHandlesMissingNonAIFields(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	bt := trackerFor(store, fb)
	if err := bt.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := bt.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot is nil")
	}
	if snap.PlanName != "" || snap.BackupCadence != "" || snap.Region != "" {
		t.Errorf("non-AI fields should be empty when platform omits them; got %+v", snap)
	}
}
