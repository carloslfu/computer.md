// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/manager"
)

func newTestBudgetSvc(capCents int) *budgetService {
	return &budgetService{
		monthlyCap:  capCents,
		persistFunc: func() error { return nil },
		ledger:      aiLedger{Month: utcMonth(time.Now())},
	}
}

// TestBudgetService_PooledCapSupersedesStatic is the regression for
// daemon-manager-ai-2: the proxy cap was a hardcoded $50 (nothing ever wrote
// /etc/vibecraft/ai_budget_cents), so a larger account that paid for a bigger
// usage budget got hosted-tool AI cut off at $50. The proxy must gate
// on the plan-aware pooled budget when available, and only fall back to the
// static cap before the first budget fetch.
func TestBudgetService_PooledCapSupersedesStatic(t *testing.T) {
	bs := newTestBudgetSvc(5000) // static $50

	// No pooled source yet → static cap applies.
	bs.ledger.SpentCents = 4000
	if err := bs.allow(); err != nil {
		t.Fatalf("$40 spent under the $50 static cap should allow: %v", err)
	}
	bs.ledger.SpentCents = 5000
	if err := bs.allow(); err == nil {
		t.Fatal("at the static cap should block")
	}

	// Wire a pooled budget. Variables are captured so the test can move them.
	remaining, unmetered, have := 5000, false, true
	bs.SetPooledBudget(func() (int, bool, bool) { return remaining, unmetered, have })

	// $90 spent — well past the $50 static cap — but the plan's pooled budget
	// still has $100 of account room before this machine's local proxy spend.
	// Pooled budget supersedes the static fallback, while this machine still
	// subtracts its own settled proxy spend locally.
	bs.ledger.SpentCents = 9000
	remaining = 10000
	if err := bs.allow(); err != nil {
		t.Fatalf("pooled budget has room; must allow despite >static-cap spend: %v", err)
	}

	// If the platform's pooled remaining budget is already fully consumed by
	// this machine's local proxy ledger, the proxy must block until the next
	// refresh/top-up instead of reusing the same remote room.
	remaining = 9000
	if err := bs.allow(); err == nil {
		t.Fatal("pooled budget consumed by local proxy spend; must block")
	}

	// Pooled budget exhausted → block.
	remaining = 0
	if err := bs.allow(); err == nil {
		t.Fatal("pooled budget exhausted; must block")
	}

	// Legacy/custom negative budget → no local pooled-budget enforcement.
	unmetered = true
	if err := bs.allow(); err != nil {
		t.Fatalf("custom negative budget must always allow: %v", err)
	}

	// No pooled snapshot (have=false) → fall back to the static cap.
	unmetered, have = false, false
	bs.ledger.SpentCents = 5000
	if err := bs.allow(); err == nil {
		t.Fatal("no pooled snapshot → static cap applies; at cap should block")
	}
}

func TestBudgetService_PooledBudgetSubtractsManagerSpend(t *testing.T) {
	srv := newBudgetGateTestServer(t, 10, true)
	if err := srv.budgetTracker.Record("gpt-5.4-mini", "c1", manager.Usage{
		OutputTokens: 1_000_000, // $4.50
	}); err != nil {
		t.Fatalf("record manager spend: %v", err)
	}

	bs := newTestBudgetSvc(5000)
	bs.SetPooledBudget(pooledBudgetSourceForProxy(srv.budgetTracker))

	// The platform returns $10 excluding this machine. Local manager spend has
	// already used $4.50, and local proxy spend has used the other $5.50. Before
	// the proxy source subtracted manager spend, this incorrectly had $4.50 of
	// room and allowed another hosted-tool AI call.
	bs.ledger.SpentCents = 550
	if err := bs.allow(); err == nil {
		t.Fatal("manager spend plus proxy spend at the pooled budget must block")
	}
}

// TestBudgetService_CurrentSpentCents pins the accessor the BudgetTracker uses
// to bill back hosted-tool AI (daemon-manager-ai-1).
func TestBudgetService_CurrentSpentCents(t *testing.T) {
	bs := newTestBudgetSvc(5000)
	bs.ledger.SpentCents = 1234
	if got := bs.currentSpentCents(); got != 1234 {
		t.Errorf("currentSpentCents: got %d, want 1234", got)
	}
}
