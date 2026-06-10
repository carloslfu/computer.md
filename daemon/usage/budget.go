// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/daemon/manager"
)

// Budget enforcement.
//
// The daemon enforces the customer's monthly AI credit budget: when
// the period's spend reaches the budget, NEW tasks are held with a
// clear in-chat explanation (see the gate in handleTask). This is the
// operational half of PRODUCT.md's "no surprise bills" promise — the
// Usage panel is the visibility half, this is the brake.
//
// Design commitments that make enforcement *smooth*:
//
//   - Fail-open. If we can't reach the platform to learn the budget,
//     enforcement stays OFF. A platform outage must never wedge a
//     customer's machine. Enforcement only engages once we have a
//     real, positive budget number in hand.
//
//   - Submission-time only. The gate runs when a new task is
//     submitted — never mid-task. A task already running always
//     finishes, even if its final turns tip spend past the budget.
//     Killing work in progress is the opposite of smooth.
//
//   - Enterprise is never paused. aiBudget == -1 means unmetered.
//
//   - The customer is warned at 80% (see CheckWarningThreshold), so
//     hitting 100% is never a surprise.

// Budget is the platform's answer to GET /api/machine/budget — the
// plan's monthly AI credit allowance plus the current billing period.
// Track D5 extends it with non-AI plan config (plan_name, backup_cadence,
// region) on the same poll.
type Budget struct {
	BudgetUSD    float64 `json:"ai_budget_usd"`
	PeriodStart  string  `json:"period_start"`
	PeriodEnd    string  `json:"period_end"`
	PeriodSource string  `json:"period_source"`

	// Track D5: non-AI plan config the daemon caches off the same poll.
	// Resolved per-machine (machines.planName) so an add-on machine
	// carries its own class's cadence even when its account's
	// subscription is on a different tier.
	PlanName      string `json:"plan_name"`
	BackupCadence string `json:"backup_cadence"`
	Region        string `json:"region"`

	ManagerKeyMode string `json:"manager_key_mode,omitempty"`
}

// BudgetState is the enforcement verdict the rest of the daemon (the
// task gate, the /api/usage endpoint) reads. It's a snapshot — recompute
// it per decision rather than caching, since spend changes every turn.
type BudgetState struct {
	// BudgetUSD is the effective budget being enforced — the override
	// when one is set (testing / manual control), else the platform
	// value. -1 means unmetered (Enterprise).
	BudgetUSD float64 `json:"budget_usd"`
	// Enforced is true when there's a positive budget to enforce
	// against. False for Enterprise (-1) and for the fail-open case
	// (budget never successfully fetched).
	Enforced bool `json:"enforced"`
	// Paused is true when Enforced and period spend >= budget. When
	// true, new task submissions are rejected.
	Paused bool `json:"paused"`
	// SpentUSD is the period's spend so far.
	SpentUSD float64 `json:"spent_usd"`
	// ResetsOn is the period end (YYYY-MM-DD) — when a paused machine
	// frees up again. Empty when the period is unknown.
	ResetsOn       string `json:"resets_on"`
	ManagerKeyMode string `json:"manager_key_mode"`
}

// NotifyFunc fires a proactive notification to the customer (bell +
// email via the platform). Matches the daemon's existing notifier
// shape. The BudgetTracker calls it when spend crosses the 80% and
// 100% thresholds.
type NotifyFunc func(kind, title, body, priority string)

// WarningThreshold is the fraction of budget at which the daemon fires
// the pre-exhaustion warning — early enough that hitting 100% is never
// a surprise, late enough that it isn't noise.
const WarningThreshold = 0.80

// BudgetTracker fetches + caches the budget, computes enforcement
// verdicts, records usage (forwarding to the Store), and fires the
// 80% / 100% threshold notifications. Safe for concurrent use.
//
// It implements core.UsageRecorder so the engine / compactor /
// summarizer record straight through it — that makes the threshold
// check ride along with every manager call, with no extra wiring.
type BudgetTracker struct {
	store *Store

	mu      sync.RWMutex
	current *Budget // last successful platform fetch; nil until first success

	// Edge-trigger state for the threshold notifications. Each holds
	// the period (ResetsOn date) we last warned for, so a new billing
	// period re-arms the warning. Empty = not yet warned.
	warned80Period  string
	warned100Period string

	machineID    string
	healthToken  string
	platformBase string
	http         *http.Client
	notify       NotifyFunc // nil = thresholds tracked but no notification sent (tests)
	managerMode  string

	// proxySpentFunc, when set, returns this month's hosted-tool AI spend (the
	// ai_proxy ledger) in cents. store.Aggregate only sums usage_records
	// (manager turns), so without this the platform-facing spend report omits
	// every dollar a deployed hosted tool burned through the credits proxy —
	// VibeCraft pays it and never bills it back. nil = no proxy on this build.
	proxySpentFunc func() int
}

// NewBudgetTracker wires a tracker. machineID + healthToken + platformBase
// are the same credentials the daemon uses to POST notifications.
// notify may be nil (tests) — the threshold edge-state is still tracked.
func NewBudgetTracker(store *Store, machineID, healthToken, platformBase string) *BudgetTracker {
	if platformBase == "" {
		platformBase = "https://www.vibecraft.so"
	}
	return &BudgetTracker{
		store:        store,
		machineID:    machineID,
		healthToken:  healthToken,
		platformBase: platformBase,
		http:         &http.Client{Timeout: 10 * time.Second},
		managerMode:  "platform",
	}
}

// SetNotifier wires the threshold-warning notification sink. Called
// once at startup. Nil is acceptable — thresholds are still tracked.
func (bt *BudgetTracker) SetNotifier(n NotifyFunc) {
	bt.mu.Lock()
	bt.notify = n
	bt.mu.Unlock()
}

// SetProxySpend wires the hosted-tool AI spend source (the ai_proxy ledger) so
// Refresh reports it to the platform alongside manager-turn spend. Nil leaves
// reporting to manager turns only.
func (bt *BudgetTracker) SetProxySpend(fn func() int) {
	bt.mu.Lock()
	bt.proxySpentFunc = fn
	bt.mu.Unlock()
}

// SetManagerKeyMode controls whether VibeCraft credits are enforced for
// manager calls. platform means VibeCraft paid upstream and credits apply;
// operator/relay usage is recorded locally but not deducted locally as
// VibeCraft credit spend by this daemon.
func (bt *BudgetTracker) SetManagerKeyMode(mode string) {
	bt.mu.Lock()
	switch mode {
	case "operator", "relay":
		bt.managerMode = mode
	default:
		bt.managerMode = "platform"
	}
	bt.mu.Unlock()
}

// Record forwards a manager call's usage to the Store, then checks the
// budget thresholds. Implements core.UsageRecorder so the engine
// records straight through the tracker. A threshold-check failure
// never blocks recording — usage accounting is the load-bearing part.
func (bt *BudgetTracker) Record(model, conversationID string, u manager.Usage) error {
	recErr := bt.store.Record(model, conversationID, u)
	bt.mu.RLock()
	managerMode := bt.managerMode
	bt.mu.RUnlock()
	if managerMode != "platform" {
		return recErr
	}
	// Check thresholds even if Record errored — the prior rows still
	// count, and we'd rather warn slightly stale than not at all.
	bt.maybeWarn()
	return recErr
}

// maybeWarn fires the 80% / 100% notifications, edge-triggered once per
// billing period. Cheap enough to call after every manager turn.
func (bt *BudgetTracker) maybeWarn() {
	st, err := bt.State()
	if err != nil || !st.Enforced || st.BudgetUSD <= 0 {
		return
	}
	frac := st.SpentUSD / st.BudgetUSD

	bt.mu.Lock()
	notify := bt.notify
	period := st.ResetsOn
	fire80 := frac >= WarningThreshold && frac < 1.0 && bt.warned80Period != period
	fire100 := frac >= 1.0 && bt.warned100Period != period
	if fire80 {
		bt.warned80Period = period
	}
	if fire100 {
		bt.warned100Period = period
		// Crossing 100% implies 80% is moot — mark it so we don't also
		// fire the 80% notice on some later sub-100% recompute.
		bt.warned80Period = period
	}
	bt.mu.Unlock()

	if notify == nil {
		return
	}
	if fire100 {
		notify(
			"budget",
			"AI credits exhausted",
			fmt.Sprintf(
				"You've used your monthly AI credits. New tasks are paused. Top up at any amount to resume right away, or wait for your billing cycle to renew on %s.",
				st.ResetsOn,
			),
			"high",
		)
	} else if fire80 {
		notify(
			"budget",
			"AI budget 80% used",
			fmt.Sprintf(
				"You've used $%.2f of your $%.0f monthly AI budget. New tasks will pause if you reach the limit before %s.",
				st.SpentUSD, st.BudgetUSD, st.ResetsOn,
			),
			"normal",
		)
	}
}

// Refresh fetches the budget from the platform and updates the cache.
// On error the previous cached value is kept (fail-open / fail-stale).
//
// Track G3: appends the current period spend so the platform can pool
// credits across the account. We compute it inline against the current
// cached period (or the calendar-month fallback when no budget has been
// fetched yet) and attach it as `?spent_cents=&period_start=`. The
// platform ignores unknown query params, so this is forward-compatible
// against a not-yet-deployed platform.
func (bt *BudgetTracker) Refresh(ctx context.Context) error {
	if bt.machineID == "" || bt.healthToken == "" {
		return fmt.Errorf("budget: missing machine_id or health_token")
	}

	// Resolve the period to report against. Prefer the last-known
	// period; fall back to the calendar month so we always have a value.
	bt.mu.RLock()
	periodStart, periodEnd := "", ""
	if bt.current != nil {
		periodStart, periodEnd = bt.current.PeriodStart, bt.current.PeriodEnd
	}
	bt.mu.RUnlock()
	if periodStart == "" || periodEnd == "" {
		periodStart, periodEnd = CurrentMonthBounds()
	}

	// Compute current-period spend in cents. Best-effort — on error we
	// still call the endpoint, just without the spent_cents param.
	bt.mu.RLock()
	managerMode := bt.managerMode
	proxySpent := bt.proxySpentFunc
	bt.mu.RUnlock()
	spentCents := -1
	if managerMode != "platform" {
		spentCents = 0
	} else if summary, err := bt.store.Aggregate(periodStart, periodEnd, 0); err == nil {
		// Round to the nearest cent.
		spentCents = int(summary.TotalCostUSD*100 + 0.5)
		if spentCents < 0 {
			spentCents = 0
		}
		// Add hosted-tool AI spend (the ai_proxy ledger). Aggregate only sums
		// manager-turn usage_records; without this the customer is never
		// billed for the AI a deployed hosted tool consumed through the
		// platform-paid credits proxy.
		if proxySpent != nil {
			if pc := proxySpent(); pc > 0 {
				spentCents += pc
			}
		}
	}

	u := fmt.Sprintf(
		"%s/api/machine/budget?machine_id=%s&period_start=%s",
		bt.platformBase, bt.machineID, periodStart,
	)
	if spentCents >= 0 {
		u += fmt.Sprintf("&spent_cents=%d", spentCents)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("budget: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bt.healthToken)

	resp, err := bt.http.Do(req)
	if err != nil {
		return fmt.Errorf("budget: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("budget: platform returned %d", resp.StatusCode)
	}

	var b Budget
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return fmt.Errorf("budget: decode: %w", err)
	}

	bt.mu.Lock()
	if b.ManagerKeyMode == "" {
		b.ManagerKeyMode = bt.managerMode
	}
	bt.current = &b
	bt.mu.Unlock()
	return nil
}

// budgetRefreshInterval is the steady-state cadence once a fetch has
// succeeded. Budgets change rarely (plan upgrade/downgrade, billing
// period rollover) so hourly is plenty.
const budgetRefreshInterval = time.Hour

// budgetRetryInterval is the cadence while we have NEVER successfully
// fetched a budget. A failed first fetch must not leave enforcement
// fail-open for a full hour — if the platform had a blip at daemon
// startup, we want to self-heal in minutes. Once any fetch succeeds we
// settle into budgetRefreshInterval.
const budgetRetryInterval = 2 * time.Minute

// budgetPausedInterval is the cadence while enforcement is paused
// (Track G10 — fast resume). A customer who tops up should see the
// manager wake back up in a minute, not an hour. A BYOM box behind
// NAT can't be pushed to, so the daemon shortens its own poll. The
// portable mechanism is the daemon's own interval; no platform→daemon
// push exists.
const budgetPausedInterval = 60 * time.Second

// StartRefreshLoop fetches the budget on startup and keeps it current.
// Run in a goroutine at daemon startup.
//
// Cadence is adaptive: until the FIRST successful fetch it retries
// every budgetRetryInterval (so a startup-time platform blip costs
// minutes of fail-open, not an hour); after the first success it
// settles to hourly. A later failure keeps the last-known value and
// does not drop back to the fast cadence — we already have a budget.
func (bt *BudgetTracker) StartRefreshLoop(ctx context.Context) {
	haveFetched := false
	if err := bt.Refresh(ctx); err != nil {
		log.Printf("budget: initial refresh failed (enforcement stays off, retrying every %s until it succeeds): %v", budgetRetryInterval, err)
	} else {
		haveFetched = true
	}

	for {
		interval := budgetRefreshInterval
		if !haveFetched {
			interval = budgetRetryInterval
		} else if st, _ := bt.State(); st.Paused {
			// G10 fast resume: poll quickly while paused so a top-up
			// resumes the manager in seconds rather than waiting an hour.
			interval = budgetPausedInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			if err := bt.Refresh(ctx); err != nil {
				if haveFetched {
					log.Printf("budget: refresh failed (keeping last-known value): %v", err)
				} else {
					log.Printf("budget: refresh still failing (enforcement stays off): %v", err)
				}
			} else {
				haveFetched = true
			}
		}
	}
}

// snapshot returns the cached budget + period under the lock. The
// budget comes solely from the last successful platform fetch — there
// is no manual override path. If a budget genuinely needs correcting,
// the fix belongs on the plan (the platform), and the daemon picks it
// up on the next Refresh.
func (bt *BudgetTracker) snapshot() (budget float64, haveBudget bool, periodStart, periodEnd string) {
	bt.mu.RLock()
	defer bt.mu.RUnlock()

	if bt.current == nil {
		// No successful fetch yet — fail-open. Period falls back to the
		// calendar month so any caller that still wants a window has one.
		periodStart, periodEnd = CurrentMonthBounds()
		return 0, false, periodStart, periodEnd
	}

	periodStart, periodEnd = bt.current.PeriodStart, bt.current.PeriodEnd
	if periodStart == "" || periodEnd == "" {
		periodStart, periodEnd = CurrentMonthBounds()
	}
	return bt.current.BudgetUSD, true, periodStart, periodEnd
}

// Snapshot returns the last successfully-fetched budget config. nil
// when no fetch has succeeded yet. Read-only callers (the backup
// module, the active-key selector) use this to pick up plan-derived
// config without an extra poll loop.
func (bt *BudgetTracker) Snapshot() *Budget {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	if bt.current == nil {
		return nil
	}
	clone := *bt.current
	return &clone
}

// State computes the current enforcement verdict. Recompute per
// decision — spend moves every turn.
func (bt *BudgetTracker) State() (BudgetState, error) {
	budget, haveBudget, periodStart, periodEnd := bt.snapshot()
	bt.mu.RLock()
	managerMode := bt.managerMode
	bt.mu.RUnlock()

	st := BudgetState{
		BudgetUSD:      budget,
		ResetsOn:       periodEnd,
		ManagerKeyMode: managerMode,
	}

	if managerMode != "platform" {
		if summary, err := bt.store.Aggregate(periodStart, periodEnd, 0); err == nil {
			st.SpentUSD = summary.TotalCostUSD
		}
		return st, nil
	}

	// Fail-open: no budget known → don't enforce.
	if !haveBudget {
		return st, nil
	}
	// Enterprise / unmetered.
	if budget < 0 {
		return st, nil
	}

	summary, err := bt.store.Aggregate(periodStart, periodEnd, 0)
	if err != nil {
		// Can't read spend → fail-open. Better to let a task through
		// than to wrongly block on a transient DB hiccup.
		return st, fmt.Errorf("budget: aggregate spend: %w", err)
	}

	st.SpentUSD = summary.TotalCostUSD
	st.Enforced = true
	// budget >= 0 here (the budget < 0 unmetered/Enterprise case returned
	// above). A budget of exactly 0 is a fully-drained pool — the platform
	// clamps a drained multi-machine pool to 0.0 — so it must pause too. The
	// old `budget > 0` guard let a drained pool keep spending past zero on the
	// remaining sibling machines.
	st.Paused = summary.TotalCostUSD >= budget
	return st, nil
}
