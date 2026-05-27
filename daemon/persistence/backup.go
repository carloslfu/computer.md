// SPDX-License-Identifier: Apache-2.0

package persistence

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Backup periodically copies the daemon's encrypted SQLite database to
// a rotating snapshot directory using SQLite's VACUUM INTO.
//
// VACUUM INTO is online-safe — it can run while the daemon serves traffic
// — and writes a fully-consistent SQLCipher-encrypted copy using the same
// encryption key. No separate "backup" credential needed.
//
// Rotation policy:
//   - Always keep the 7 most recent snapshots.
//   - Additionally keep one snapshot per ISO week for the 4 most recent
//     weeks not already covered by the latest-7.
//
// This gives ~28 days of history while bounding disk use at ~11 copies.
type Backup struct {
	db         *DB
	dir        string
	interval   time.Duration
	keepRecent int
	keepWeekly int
	logger     *log.Logger
	clock      func() time.Time
	uploader   Uploader // optional off-machine push (S3, etc.)

	// /metrics observability (Workstream K). Read by main.handleMetrics
	// via LastSuccessUnix and FailureCount.
	mLastSuccess atomic.Int64
	mFailure     atomic.Int64
}

// Uploader is an optional hook called after each successful local snapshot.
// On Managed plans the platform-provided uploader pushes to S3; on BYOM
// it's nil unless the operator wires one up via Workstream B.6.
type Uploader interface {
	Upload(ctx context.Context, path string) error
}

// NewBackup wires a backup runner against the given DB. dir is created
// with mode 0700 if missing. interval defaults to 4h when zero.
func NewBackup(db *DB, dir string, interval time.Duration) (*Backup, error) {
	if interval <= 0 {
		interval = 4 * time.Hour
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("backup: create dir %s: %w", dir, err)
	}
	return &Backup{
		db:         db,
		dir:        dir,
		interval:   interval,
		keepRecent: 7,
		keepWeekly: 4,
		logger:     log.Default(),
		clock:      time.Now,
	}, nil
}

// SetUploader installs an off-machine uploader. Safe to call before Start.
func (b *Backup) SetUploader(u Uploader) { b.uploader = u }

// SetLogger overrides the default logger (used in tests).
func (b *Backup) SetLogger(l *log.Logger) { b.logger = l }

// Start runs one snapshot immediately, then loops on b.interval. Exits
// cleanly when ctx is cancelled.
func (b *Backup) Start(ctx context.Context) {
	go b.loop(ctx)
}

func (b *Backup) loop(ctx context.Context) {
	// Initial snapshot a few seconds after boot so daemon startup isn't
	// IO-bound on first launch.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	for {
		if err := b.Run(ctx); err != nil {
			b.logger.Printf("backup: snapshot failed: %v", err)
			b.mFailure.Add(1)
		} else {
			b.mLastSuccess.Store(b.clock().Unix())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(b.interval):
		}
	}
}

// LastSuccessUnix returns the unix timestamp of the most recent
// successful snapshot, or 0 if none has succeeded yet.
func (b *Backup) LastSuccessUnix() int64 { return b.mLastSuccess.Load() }

// FailureCount returns the number of snapshot attempts that errored
// since process start.
func (b *Backup) FailureCount() int64 { return b.mFailure.Load() }

// Run executes a single backup cycle: snapshot, rotate, optionally
// upload. Exposed for tests + the management channel.
func (b *Backup) Run(ctx context.Context) error {
	path, err := b.snapshot()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	b.logger.Printf("backup: wrote %s", path)

	if b.uploader != nil {
		uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := b.uploader.Upload(uctx, path); err != nil {
			// Don't propagate — local snapshot succeeded, off-machine
			// push is best-effort. Surface in logs for monitoring.
			b.logger.Printf("backup: upload %s failed: %v", path, err)
		}
	}

	if err := b.rotate(); err != nil {
		b.logger.Printf("backup: rotate failed: %v", err)
	}
	return nil
}

// snapshot writes a VACUUM INTO of the database to a timestamped file in
// b.dir and returns its path. Goes through a .tmp suffix + atomic rename
// so a partial write can't be picked up as a valid backup.
func (b *Backup) snapshot() (string, error) {
	ts := b.clock().UTC().Format("2006-01-02T15-04-05Z")
	final := filepath.Join(b.dir, fmt.Sprintf("vibecraft-%s.db", ts))
	tmp := final + ".tmp"

	// SQLCipher accepts VACUUM INTO with a quoted destination path; the
	// resulting file is encrypted with the same key as the source.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("clear stale tmp: %w", err)
	}
	b.db.mu.Lock()
	defer b.db.mu.Unlock()

	// Single-quote the path; SQLite expects an SQL string literal.
	_, err := b.db.conn.Exec(fmt.Sprintf("VACUUM INTO '%s'", escapeSQLPath(tmp)))
	if err != nil {
		return "", fmt.Errorf("vacuum into: %w", err)
	}

	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename: %w", err)
	}
	if err := os.Chmod(final, 0600); err != nil {
		return "", fmt.Errorf("chmod: %w", err)
	}
	return final, nil
}

// rotate deletes snapshots beyond the keep policy.
func (b *Backup) rotate() error {
	entries, err := listBackups(b.dir)
	if err != nil {
		return err
	}

	keep := selectKeepers(entries, b.keepRecent, b.keepWeekly, b.clock())
	keepSet := make(map[string]struct{}, len(keep))
	for _, e := range keep {
		keepSet[e.path] = struct{}{}
	}

	for _, e := range entries {
		if _, ok := keepSet[e.path]; ok {
			continue
		}
		if err := os.Remove(e.path); err != nil {
			b.logger.Printf("backup: remove %s: %v", e.path, err)
		}
	}
	return nil
}

type backupEntry struct {
	path string
	ts   time.Time
}

var backupFileRe = regexp.MustCompile(`^vibecraft-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z)\.db$`)

func listBackups(dir string) ([]backupEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []backupEntry
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		m := backupFileRe.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		ts, err := time.Parse("2006-01-02T15-04-05Z", m[1])
		if err != nil {
			continue
		}
		out = append(out, backupEntry{
			path: filepath.Join(dir, ent.Name()),
			ts:   ts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts.After(out[j].ts) })
	return out, nil
}

// selectKeepers returns the snapshots to retain:
//   - keepRecent most recent regardless of age
//   - plus one per ISO week for the keepWeekly most recent weeks not
//     already represented in the recent slice.
//
// Pure function for easy testing.
func selectKeepers(entries []backupEntry, keepRecent, keepWeekly int, now time.Time) []backupEntry {
	if len(entries) == 0 {
		return nil
	}

	keep := make([]backupEntry, 0, keepRecent+keepWeekly)
	seen := make(map[string]struct{}, keepRecent+keepWeekly)

	// 1. Latest keepRecent.
	for i, e := range entries {
		if i >= keepRecent {
			break
		}
		keep = append(keep, e)
		seen[e.path] = struct{}{}
	}

	// 2. Weekly: keep one snapshot per ISO week, bounded by a sliding
	// window of keepWeekly ISO weeks before the current week. Older
	// snapshots are dropped even if no other bucket fills the slot.
	nowMonday := isoWeekStart(now)

	type weekKey struct {
		year int
		week int
	}
	buckets := make(map[weekKey][]backupEntry)
	weeks := []weekKey{}
	for _, e := range entries {
		y, w := e.ts.ISOWeek()
		k := weekKey{y, w}
		if _, exists := buckets[k]; !exists {
			weeks = append(weeks, k)
		}
		buckets[k] = append(buckets[k], e)
	}
	// `weeks` already ordered newest-first because `entries` is.
	added := 0
	for _, wk := range weeks {
		if added >= keepWeekly {
			break
		}
		latest := buckets[wk][0] // entries are newest-first inside the bucket too
		// Distance in ISO weeks: 0 = current, 1 = last week, etc.
		entryMonday := isoWeekStart(latest.ts)
		weeksBack := int(nowMonday.Sub(entryMonday).Hours()/24/7 + 0.5)
		if weeksBack < 1 || weeksBack > keepWeekly {
			continue // outside the rolling weekly window
		}
		if _, ok := seen[latest.path]; ok {
			continue
		}
		keep = append(keep, latest)
		seen[latest.path] = struct{}{}
		added++
	}

	return keep
}

// isoWeekStart returns the Monday at 00:00 of the ISO week containing t.
func isoWeekStart(t time.Time) time.Time {
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7 // Sunday → end of ISO week
	}
	daysBack := wd - 1
	y, m, d := t.Date()
	return time.Date(y, m, d-daysBack, 0, 0, 0, 0, t.Location())
}

func escapeSQLPath(p string) string {
	// SQLite string literal: double up single quotes.
	return strings.ReplaceAll(p, "'", "''")
}
