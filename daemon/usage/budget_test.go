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
	server  *httptest.Server
	mu      sync.Mutex
	budget  float64
	start   string
	end     string
	status  int    // 0 or 200 → serve the budget; anything else → that status code
	gotAuth string // the Authorization header the daemon actually sent
	gotPath string // the request path the daemon actually hit
	gotQuery string
	calls   int
}

func newFakeBudget(t *testing.T) *fakeBudget {
	t.Helper()
	fb := &fakeBudget{budget: 200, start: "2026-05-02", end: "2026-06-02", status: 200}
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ai_budget_usd": fb.budget,
			"period_start":  fb.start,
			"period_end":    fb.end,
			"period_source": "subscription",
		})
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
	if !st.Enforced {
		t.Errorf("a fetched positive budget must engage enforcement")
	}
	if st.ResetsOn != "2026-06-02" {
		t.Errorf("period_end didn't parse: got %q want 2026-06-02", st.ResetsOn)
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

// TestBudget_EnterpriseUnmeteredNeverPauses — a fetched budget of -1
// (Enterprise) never pauses, regardless of spend.
func TestBudget_EnterpriseUnmeteredNeverPauses(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(-1)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())

	seedSpend(t, store, "c1", 100_000.0)
	st, _ := bt.State()
	if st.Enforced {
		t.Errorf("unmetered (-1) must not be enforced")
	}
	if st.Paused {
		t.Errorf("unmetered (-1) must never pause")
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
	if len(*notes) != 1 || (*notes)[0].title != "AI budget 80% used" {
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
		if (*notes)[i].title == "AI credits exhausted" {
			got = &(*notes)[i]
		}
	}
	if got == nil {
		t.Fatalf("expected an 'AI credits exhausted' notification, got %+v", *notes)
	}
	if got.priority != "high" {
		t.Errorf("credits-exhausted must be high priority, got %q", got.priority)
	}
}

// TestBudget_NoWarningWhenUnmetered — Enterprise never warns.
func TestBudget_NoWarningWhenUnmetered(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := newFakeBudget(t)
	fb.set(-1)
	bt := trackerFor(store, fb)
	bt.Refresh(context.Background())
	notify, notes := capturingNotifier()
	bt.SetNotifier(notify)

	bt.Record("gpt-5.4-mini", "c1", manager.Usage{OutputTokens: outTokensFor(9999.0)})
	if len(*notes) != 0 {
		t.Errorf("unmetered must never warn, got %+v", *notes)
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

// J12 — TestBudget_RefreshParsesNonAIConfig verifies that the non-AI
// plan-config fields (plan_name, backup_cadence, region) come through
// the same poll the AI budget rides on (Track D5). The daemon's
// backup module + dashboard read these off the cached Snapshot().
func TestBudget_RefreshParsesNonAIConfig(t *testing.T) {
	store := NewStore(openTestDB(t))
	fb := &fakeBudget{
		budget: 100, start: "2026-05-02", end: "2026-06-02", status: 200,
	}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ai_budget_usd":       100.0,
			"period_start":        "2026-05-02",
			"period_end":          "2026-06-02",
			"period_source":       "subscription",
			"plan_name":           "Business",
			"backup_cadence":      "aws-dlm-hourly-daily-weekly-s3-continuous-30d",
			"region":              "us-east-1",
			"manager_key_mode":    "platform",
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
// extension fields still works — the AI budget refresh succeeds and
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
