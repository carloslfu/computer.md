// SPDX-License-Identifier: Apache-2.0

package core

import (
	"sync"
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
