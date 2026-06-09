// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestBudgetExhaustsWithoutPause(t *testing.T) {
	ctx, b := newTaskBudget(context.Background(), 200*time.Millisecond)

	select {
	case <-ctx.Done():
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("budget should have cancelled ctx within 500ms")
	}
	if !b.Exhausted() {
		t.Error("Exhausted should be true after budget cancel")
	}
}

func TestBudgetIgnoresPausedTime(t *testing.T) {
	ctx, b := newTaskBudget(context.Background(), 300*time.Millisecond)

	// Pause immediately and stay paused much longer than the budget.
	b.Pause()
	time.Sleep(800 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("ctx should not be cancelled while paused")
	}
	if b.Exhausted() {
		t.Fatal("Exhausted should be false while paused")
	}

	// Resume — budget should now fire ~300ms later.
	b.Resume()
	select {
	case <-ctx.Done():
		if !b.Exhausted() {
			t.Error("Exhausted should be true after fire")
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("budget should have fired within 700ms of resume")
	}
}

func TestBudgetSplitAcrossPauses(t *testing.T) {
	// Spend 100ms active, pause for 500ms, resume, spend another 100ms.
	// Total active is 200ms; budget is 300ms, so should NOT exhaust.
	ctx, b := newTaskBudget(context.Background(), 300*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	b.Pause()
	time.Sleep(500 * time.Millisecond)
	b.Resume()
	time.Sleep(100 * time.Millisecond)

	if ctx.Err() != nil {
		t.Fatalf("ctx should not be cancelled at 200ms active / 300ms budget: %v", ctx.Err())
	}
	if b.Exhausted() {
		t.Error("Exhausted should be false at 200ms active / 300ms budget")
	}

	// 150ms more of active time pushes total to 350ms > 300ms budget.
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("budget should exhaust after another 150ms active")
	}
}

func TestBudgetIdempotentPauseResume(t *testing.T) {
	_, b := newTaskBudget(context.Background(), time.Hour)

	b.Pause()
	b.Pause() // no-op
	b.Resume()
	b.Resume() // no-op
	// Should not panic, should not break invariants.
	b.Pause()
	b.Resume()
}

// TestBudgetStopTearsDownGoroutine is the regression guard for the
// goroutine/timer leak: a budget created with a long timeout (the production
// default is 2h) parks its run() goroutine on a time.NewTimer for the whole
// budget. On the normal-completion path processTask cancels only a CHILD
// context, which does NOT cancel the budget's own context, so run() used to
// keep blocking — leaking a goroutine + multi-hour timer per finished task.
// Stop() must terminate run() immediately.
//
// We assert behaviorally: spawn many long-budget watchdogs, Stop them all,
// and confirm the goroutine count settles back toward baseline. Without
// Stop() (or with a Stop() that doesn't cancel the budget context) every
// run() goroutine stays parked on its 1h timer and the count never drops.
func TestBudgetStopTearsDownGoroutine(t *testing.T) {
	runtime.GC()
	settle := func() { // let scheduler reclaim exited goroutines
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			time.Sleep(2 * time.Millisecond)
		}
	}
	settle()
	base := runtime.NumGoroutine()

	const n = 50
	budgets := make([]*taskBudget, 0, n)
	ctxs := make([]context.Context, 0, n)
	for i := 0; i < n; i++ {
		// 1h budget: run() parks on a 1h timer; nothing reclaims it without Stop.
		ctx, b := newTaskBudget(context.Background(), time.Hour)
		budgets = append(budgets, b)
		ctxs = append(ctxs, ctx)
	}

	// Sanity: the n watchdog goroutines are actually live before Stop, so a
	// passing test can't be a no-op (proves run() is parked, not already gone).
	settle()
	withBudgets := runtime.NumGoroutine()
	if withBudgets < base+n/2 {
		t.Fatalf("expected ~%d extra goroutines for live budgets, base=%d now=%d", n, base, withBudgets)
	}

	for _, b := range budgets {
		b.Stop()
	}

	// run() should return promptly via its <-ctx.Done() arm (Stop cancels the
	// budget context), and Stop must surface the cancellation on the budget's
	// own ctx.
	for i, ctx := range ctxs {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("budget %d: ctx not cancelled after Stop()", i)
		}
		// Stop is a clean teardown, not a budget timeout.
		if budgets[i].Exhausted() {
			t.Fatalf("budget %d: Exhausted() must stay false after Stop()", i)
		}
	}

	// Poll until the goroutine count settles back near baseline. Allow a
	// small slack for unrelated runtime goroutines.
	deadline := time.After(3 * time.Second)
	for {
		settle()
		now := runtime.NumGoroutine()
		if now <= base+5 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("budget goroutines did not tear down after Stop(): base=%d still=%d (leak)", base, now)
		default:
		}
	}
}
