// SPDX-License-Identifier: Apache-2.0

package core

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

type recordingNotifier struct {
	mu    sync.Mutex
	calls []string // kind values, oldest-first
}

func (r *recordingNotifier) Notify(kind, _, _, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, kind)
}

func (r *recordingNotifier) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestCheckDiskHealthy_PausesAndResumes(t *testing.T) {
	db := testDB(t)
	e := testEngine(t, db, nil)

	rec := &recordingNotifier{}
	e.SetNotifier(rec)

	var ratio float64
	e.SetDiskFreeRatio(func() (float64, error) { return ratio, nil })

	// Initially healthy at 20% free.
	ratio = 0.20
	if !e.checkDiskHealthy() {
		t.Fatalf("expected healthy at 20%%")
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("no notifications expected on first healthy check, got %v", got)
	}

	// Crosses the low threshold → pause + high-priority notification.
	ratio = 0.05
	if e.checkDiskHealthy() {
		t.Fatalf("expected paused at 5%%")
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "system:disk_low" {
		t.Fatalf("expected one system:disk_low, got %v", got)
	}

	// Still below low threshold, no further notification (edge-triggered).
	ratio = 0.04
	if e.checkDiskHealthy() {
		t.Fatalf("still expected paused at 4%%")
	}
	if got := rec.seen(); len(got) != 1 {
		t.Fatalf("expected no new notification, got %v", got)
	}

	// Below the resume threshold but above the pause threshold — still paused (hysteresis).
	ratio = 0.12
	if e.checkDiskHealthy() {
		t.Fatalf("expected still paused in hysteresis zone (12%%)")
	}
	if got := rec.seen(); len(got) != 1 {
		t.Fatalf("expected no new notification inside hysteresis, got %v", got)
	}

	// Cross the resume ceiling → resume + recovery notification.
	ratio = 0.20
	if !e.checkDiskHealthy() {
		t.Fatalf("expected resumed at 20%%")
	}
	if got := rec.seen(); len(got) != 2 || got[1] != "system:disk_recovered" {
		t.Fatalf("expected system:disk_recovered as second event, got %v", got)
	}
}

func TestCheckDiskHealthy_ProbeErrorContinues(t *testing.T) {
	db := testDB(t)
	e := testEngine(t, db, nil)

	e.SetDiskFreeRatio(func() (float64, error) {
		// Simulate a transient statfs error.
		return 0, errBoom
	})

	// We'd rather process work than stall on a probe hiccup.
	if !e.checkDiskHealthy() {
		t.Fatalf("expected healthy fallback on probe error")
	}
}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

var errBoom = boomErr{}

// TestCheckDiskHealthy_NoRaceWithIsPaused drives the disk-health writer
// (checkDiskHealthy, on its own goroutine, flapping the free-space ratio
// across the pause/resume thresholds) concurrently with the IsPaused()
// reader (the /metrics surface). Run under `go test -race`, this fails on
// the unsynchronised-field version of the code where checkDiskHealthy
// mutated e.diskPaused without holding e.mu while IsPaused read it under the
// lock. With the locked accessors both sides agree on the lock and the race
// detector stays quiet.
func TestCheckDiskHealthy_NoRaceWithIsPaused(t *testing.T) {
	db := testDB(t)
	e := testEngine(t, db, nil)

	// Ratio flaps below the pause floor and above the resume ceiling so the
	// writer keeps flipping diskPaused on real transitions, not a constant.
	var ratioBits uint64 // float64 bits, swapped atomically by the flapper
	storeRatio := func(f float64) { atomic.StoreUint64(&ratioBits, math.Float64bits(f)) }
	storeRatio(0.20)
	e.SetDiskFreeRatio(func() (float64, error) {
		return math.Float64frombits(atomic.LoadUint64(&ratioBits)), nil
	})

	const iters = 2000
	var wg sync.WaitGroup
	wg.Add(3)

	// Flapper: alternate the free-space ratio across the thresholds.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				storeRatio(0.05) // below pause floor
			} else {
				storeRatio(0.20) // above resume ceiling
			}
		}
	}()

	// Writer: the engine's disk-health check (mutates diskPaused).
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			e.checkDiskHealthy()
		}
	}()

	// Reader: the /metrics observer.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = e.IsPaused()
		}
	}()

	wg.Wait()
}
