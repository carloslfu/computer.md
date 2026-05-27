// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"sync"
	"time"
)

// taskBudget caps how much *agent-active* time a single task may consume.
// Time spent waiting on a human (approval card, credential card) is paused
// and does not count against the budget — the agent isn't doing work while
// the human is thinking, so it shouldn't be charged for it.
//
// The budget runs its own watchdog goroutine: when the active time exceeds
// the cap, it cancels the task context. Callers Pause() before suspending
// on a human-input channel and Resume() after the human replies. Pause /
// Resume are level-triggered and idempotent; a wake channel re-arms the
// timer on every state change so the fire is precise to the budget.
type taskBudget struct {
	mu          sync.Mutex
	budget      time.Duration
	activeStart time.Time     // when the current active window started; meaningless when paused
	accumulated time.Duration // active time accumulated across past pause/resume cycles
	paused      bool
	exhausted   bool
	cancel      context.CancelFunc
	wake        chan struct{} // buffered(1); receives a signal whenever Pause/Resume changes state
}

// newTaskBudget creates a context derived from parent that is cancelled when
// the agent has consumed `budget` of active time. Cancellation is the only
// effect — translating the cancellation into a "task timeout" error is the
// caller's job, via Exhausted().
func newTaskBudget(parent context.Context, budget time.Duration) (context.Context, *taskBudget) {
	ctx, cancel := context.WithCancel(parent)
	b := &taskBudget{
		budget:      budget,
		activeStart: time.Now(),
		cancel:      cancel,
		wake:        make(chan struct{}, 1),
	}
	go b.run(ctx)
	return ctx, b
}

func (b *taskBudget) run(ctx context.Context) {
	for {
		b.mu.Lock()
		remaining := b.remainingLocked()
		paused := b.paused
		b.mu.Unlock()

		if remaining <= 0 {
			b.mu.Lock()
			b.exhausted = true
			b.mu.Unlock()
			b.cancel()
			return
		}

		// While paused the budget can't expire — wait for a state change
		// or shutdown. While running, fire at exactly `remaining` from now.
		if paused {
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
				continue
			}
		}

		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// budget likely exhausted — loop and check under lock
		case <-b.wake:
			timer.Stop()
		}
	}
}

func (b *taskBudget) remainingLocked() time.Duration {
	used := b.accumulated
	if !b.paused {
		used += time.Since(b.activeStart)
	}
	return b.budget - used
}

func (b *taskBudget) signalWake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Pause stops the budget clock. Safe to call repeatedly — second and later
// calls are no-ops. Always paired with Resume() in a defer.
func (b *taskBudget) Pause() {
	b.mu.Lock()
	if b.paused {
		b.mu.Unlock()
		return
	}
	b.accumulated += time.Since(b.activeStart)
	b.paused = true
	b.mu.Unlock()
	b.signalWake()
}

// Resume restarts the budget clock after a Pause. Safe to call when not
// paused — it no-ops.
func (b *taskBudget) Resume() {
	b.mu.Lock()
	if !b.paused {
		b.mu.Unlock()
		return
	}
	b.activeStart = time.Now()
	b.paused = false
	b.mu.Unlock()
	b.signalWake()
}

// Exhausted reports whether the budget was the reason for cancellation.
// Callers check this when ctx.Err() is non-nil to distinguish a budget
// timeout from a user-initiated cancel.
func (b *taskBudget) Exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exhausted
}
