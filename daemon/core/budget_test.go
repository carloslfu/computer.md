// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
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
