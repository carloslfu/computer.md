// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
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
// /etc/vibecraft/ai_budget_cents), so a Business/Enterprise machine that paid
// for a large AI budget got hosted-tool AI cut off at $50. The proxy must gate
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
	// has room, so the Business machine must NOT be blocked.
	bs.ledger.SpentCents = 9000
	if err := bs.allow(); err != nil {
		t.Fatalf("pooled budget has room; must allow despite >static-cap spend: %v", err)
	}

	// Pooled budget exhausted → block.
	remaining = 0
	if err := bs.allow(); err == nil {
		t.Fatal("pooled budget exhausted; must block")
	}

	// Unmetered (Enterprise, -1 → unmetered=true) → always allow.
	unmetered = true
	if err := bs.allow(); err != nil {
		t.Fatalf("unmetered plan must always allow: %v", err)
	}

	// No pooled snapshot (have=false) → fall back to the static cap.
	unmetered, have = false, false
	bs.ledger.SpentCents = 5000
	if err := bs.allow(); err == nil {
		t.Fatal("no pooled snapshot → static cap applies; at cap should block")
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
