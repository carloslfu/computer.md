// SPDX-License-Identifier: Apache-2.0

package persistence

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func log_noop() *log.Logger { return log.New(io.Discard, "", 0) }

func TestBackup_VacuumInto(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	encKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	db, err := Open(dbPath, encKey)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Write a row so the snapshot has content.
	if err := db.CreateAPIKey("k1", "test", "hash1", "abcd", "user-1"); err != nil {
		t.Fatalf("createkey: %v", err)
	}

	backupDir := filepath.Join(tmp, "backups")
	b, err := NewBackup(db, backupDir, time.Hour)
	if err != nil {
		t.Fatalf("newbackup: %v", err)
	}

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	entries, err := listBackups(backupDir)
	if err != nil {
		t.Fatalf("listBackups: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(entries))
	}

	// Open the snapshot as a SQLCipher DB and verify content survived.
	clone, err := Open(entries[0].path, encKey)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer clone.Close()
	row, err := clone.GetAPIKeyByHash("hash1")
	if err != nil {
		t.Fatalf("read from snapshot: %v", err)
	}
	if row.Name != "test" {
		t.Fatalf("snapshot lost data: name=%q", row.Name)
	}
}

func TestSelectKeepers(t *testing.T) {
	mk := func(s string) backupEntry {
		ts, _ := time.Parse("2006-01-02T15-04-05Z", s)
		return backupEntry{path: s, ts: ts}
	}

	entries := []backupEntry{
		mk("2026-05-14T10-00-00Z"), // newest
		mk("2026-05-14T06-00-00Z"),
		mk("2026-05-14T02-00-00Z"),
		mk("2026-05-13T22-00-00Z"),
		mk("2026-05-13T18-00-00Z"),
		mk("2026-05-13T14-00-00Z"),
		mk("2026-05-13T10-00-00Z"), // 7th most-recent
		mk("2026-05-13T06-00-00Z"), // 8th — same week as #7, should be dropped
		mk("2026-05-07T10-00-00Z"), // week prior — keep one as weekly
		mk("2026-04-30T10-00-00Z"), // 2 weeks prior — keep
		mk("2026-04-23T10-00-00Z"), // 3 weeks prior — keep
		mk("2026-04-16T10-00-00Z"), // 4 weeks prior — keep
		mk("2026-04-09T10-00-00Z"), // 5 weeks prior — drop
	}
	now, _ := time.Parse("2006-01-02T15-04-05Z", "2026-05-14T11-00-00Z")

	keep := selectKeepers(entries, 7, 4, now)

	// Build a set for easy assertion.
	keepSet := map[string]bool{}
	for _, e := range keep {
		keepSet[e.path] = true
	}

	mustKeep := []string{
		"2026-05-14T10-00-00Z",
		"2026-05-14T06-00-00Z",
		"2026-05-14T02-00-00Z",
		"2026-05-13T22-00-00Z",
		"2026-05-13T18-00-00Z",
		"2026-05-13T14-00-00Z",
		"2026-05-13T10-00-00Z",
		"2026-05-07T10-00-00Z",
		"2026-04-30T10-00-00Z",
		"2026-04-23T10-00-00Z",
		"2026-04-16T10-00-00Z",
	}
	for _, p := range mustKeep {
		if !keepSet[p] {
			t.Errorf("expected to keep %s but did not", p)
		}
	}

	mustDrop := []string{
		"2026-05-13T06-00-00Z", // same week as #7 — dropped because keepRecent already covers that week
		"2026-04-09T10-00-00Z", // older than 4 weekly buckets
	}
	for _, p := range mustDrop {
		if keepSet[p] {
			t.Errorf("expected to drop %s but kept", p)
		}
	}
}

func TestBackup_Rotate(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	encKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	db, err := Open(dbPath, encKey)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	backupDir := filepath.Join(tmp, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Pre-create 12 backups across two weeks.
	for _, name := range []string{
		"vibecraft-2026-05-14T10-00-00Z.db",
		"vibecraft-2026-05-14T06-00-00Z.db",
		"vibecraft-2026-05-14T02-00-00Z.db",
		"vibecraft-2026-05-13T22-00-00Z.db",
		"vibecraft-2026-05-13T18-00-00Z.db",
		"vibecraft-2026-05-13T14-00-00Z.db",
		"vibecraft-2026-05-13T10-00-00Z.db",
		"vibecraft-2026-05-13T06-00-00Z.db", // 8th — same week as latest 7
		"vibecraft-2026-05-07T10-00-00Z.db",
		"vibecraft-2026-04-30T10-00-00Z.db",
		"vibecraft-2026-04-23T10-00-00Z.db",
		"vibecraft-2026-04-09T10-00-00Z.db", // 5 weeks back — drop
		"random-noise.db",                   // ignored by rotation
	} {
		if err := os.WriteFile(filepath.Join(backupDir, name), []byte("x"), 0600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	now, _ := time.Parse("2006-01-02T15-04-05Z", "2026-05-14T11-00-00Z")
	b := &Backup{
		db: db, dir: backupDir, keepRecent: 7, keepWeekly: 4,
		logger: log_noop(), clock: func() time.Time { return now },
	}
	if err := b.rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Remaining files we own should be: 7 recent + 3 weekly = 10 + the
	// random-noise file (untouched). Total 11.
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 11 {
		t.Fatalf("expected 11 entries, got %d: %v", len(names), names)
	}
	for _, mustExist := range []string{
		"vibecraft-2026-05-14T10-00-00Z.db",
		"vibecraft-2026-05-07T10-00-00Z.db",
		"vibecraft-2026-04-30T10-00-00Z.db",
		"vibecraft-2026-04-23T10-00-00Z.db",
		"random-noise.db",
	} {
		if _, err := os.Stat(filepath.Join(backupDir, mustExist)); err != nil {
			t.Errorf("expected %s to exist, got: %v", mustExist, err)
		}
	}
	for _, mustNotExist := range []string{
		"vibecraft-2026-04-09T10-00-00Z.db",
		"vibecraft-2026-05-13T06-00-00Z.db",
	} {
		if _, err := os.Stat(filepath.Join(backupDir, mustNotExist)); err == nil {
			t.Errorf("expected %s to be deleted", mustNotExist)
		}
	}
}
