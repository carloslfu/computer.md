// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// vibecraft backup list / restore — operator-driven local-machine
// recovery (Workstream B.1).
//
// Restore is intentionally NOT a self-service customer feature. It
// requires SSH access to the machine (Managed: break-glass key pair;
// BYOM: customer-owned access) and runs against the local backups
// directory. The CLI gives a tidy interface for what would otherwise
// be a `cp` over the encrypted file — including a guard against
// restoring against a running daemon.

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Manage local database backups",
	Long: `List and restore VibeCraft database snapshots.

This subcommand operates against the local backups directory and does
NOT need an API key (it's a recovery tool, not a network operation).
Run it with sudo on the machine: 'sudo vibecraft backup ...'`,
}

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available local snapshots",
	RunE: func(_ *cobra.Command, _ []string) error {
		dir, err := backupsDir()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("reading %s: %w", dir, err)
		}
		count := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			fmt.Printf("%s  %10d bytes  %s\n",
				info.ModTime().UTC().Format("2006-01-02 15:04:05Z"),
				info.Size(),
				e.Name(),
			)
			count++
		}
		if count == 0 {
			fmt.Println("No snapshots found in", dir)
		}
		return nil
	},
}

var (
	flagRestoreFile  string
	flagRestoreForce bool
)

var backupRestoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore a snapshot in place of the live database",
	Long: `Replaces /var/lib/vibecraft/vibecraft.db with the named snapshot.

The current database is moved to vibecraft.db.bak-<ts> as a safety net.
The daemon MUST be stopped first ('sudo systemctl stop vibecraft-daemon')
or the restore will refuse to proceed; SQLite WAL state would otherwise
race with the live process and corrupt the restored file.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		if flagRestoreFile == "" {
			return fmt.Errorf("--file <snapshot> required")
		}
		if !flagRestoreForce {
			running, err := daemonRunning()
			if err != nil {
				return fmt.Errorf("checking daemon state: %w", err)
			}
			if running {
				return fmt.Errorf(
					"daemon is running. Stop it first:\n  sudo systemctl stop vibecraft-daemon\nThen retry, or pass --force (NOT recommended).",
				)
			}
		}
		return restoreSnapshot(flagRestoreFile)
	},
}

func init() {
	backupRestoreCmd.Flags().StringVar(&flagRestoreFile, "file", "", "Snapshot filename inside the backups directory")
	backupRestoreCmd.Flags().BoolVar(&flagRestoreForce, "force", false, "Skip the daemon-running check (DANGEROUS)")
	backupCmd.AddCommand(backupListCmd, backupRestoreCmd)
	rootCmd.AddCommand(backupCmd)
}

func backupsDir() (string, error) {
	candidates := []string{
		os.Getenv("VIBECRAFT_BACKUPS_DIR"),
		"/var/lib/vibecraft/backups",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		info, err := os.Stat(c)
		if err == nil && info.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("no backups directory found; try setting VIBECRAFT_BACKUPS_DIR")
}

func dataDir() string {
	if d := os.Getenv("VIBECRAFT_DATA_DIR"); d != "" {
		return d
	}
	return "/var/lib/vibecraft"
}

// Seams for tests. Production uses the real systemctl binary, a live TCP
// dial, and the wall clock; tests override these to exercise the detection
// and naming logic without a running daemon, systemd, or clock dependence.
var (
	// nowUTC returns the current time used to stamp the safety-net backup.
	// Overridable so tests can prove two restores produce distinct names.
	nowUTC = func() time.Time { return time.Now().UTC() }

	// systemctlIsActive reports whether the named systemd unit is active.
	// ok is false when systemctl is unavailable or the unit is unknown, in
	// which case the caller falls through to the port probe.
	systemctlIsActive = func(unit string) (active bool, ok bool) {
		bin, err := exec.LookPath("systemctl")
		if err != nil {
			return false, false
		}
		// `is-active` exits non-zero for inactive/failed/unknown units but
		// still prints the state on stdout. We key off the printed state, not
		// the exit code, so "inactive" is a definitive negative (ok=true)
		// while a missing unit ("unknown"/empty) is inconclusive (ok=false).
		out, _ := exec.Command(bin, "is-active", unit).Output()
		state := strings.TrimSpace(string(out))
		switch state {
		case "active", "activating", "reloading":
			return true, true
		case "inactive", "deactivating", "failed":
			return false, true
		default:
			// "unknown", empty (systemctl errored / not under systemd), etc.
			return false, false
		}
	}

	// daemonPortListening dials the daemon's local HTTP port. A successful
	// connect means a process — almost certainly the daemon — holds the DB
	// open, so a restore would corrupt it.
	daemonPortListening = func() bool {
		port := "8420"
		if p := strings.TrimSpace(os.Getenv("VIBECRAFT_PORT")); p != "" {
			port = p
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 300*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
)

// daemonRunning reports whether the daemon is currently up, so a restore can
// refuse to overwrite a live, DB-holding process.
//
// The systemd unit is Type=simple with no PIDFile, so the old pidfile/socket
// stat checks never fired — daemonRunning was dead and always returned false.
// It now asks the authoritative sources in order:
//  1. `systemctl is-active vibecraft-daemon` — definitive on a managed box.
//  2. A TCP probe of the daemon's local port — catches non-systemd/dev runs,
//     and a daemon that systemd reports inactive but is somehow still bound.
//
// If systemctl gives a definitive "inactive" AND the port is closed, the
// daemon is down. If both signals are inconclusive (no systemd, port closed),
// returns running=false — the operator's --force flag is the escape hatch.
func daemonRunning() (bool, error) {
	if active, ok := systemctlIsActive("vibecraft-daemon"); ok && active {
		return true, nil
	}
	if daemonPortListening() {
		return true, nil
	}
	return false, nil
}

func restoreSnapshot(name string) error {
	dir, err := backupsDir()
	if err != nil {
		return err
	}

	// Path-traversal guard. --file names a snapshot INSIDE the backups
	// directory and nothing else. Without this, a crafted --file
	// ("../../var/lib/vibecraft/vibecraft.db", an absolute path, or any
	// name with a slash) would let the restore read an arbitrary file and
	// copy it over the live daemon DB. Two independent checks:
	//   1. the name must be a bare basename (no separators, no "."/"..")
	//   2. the cleaned join must still resolve under dir
	// Either alone closes the hole; together they're defense in depth and
	// give a precise error for the common "passed a path" mistake.
	if name != filepath.Base(name) || strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) {
		return fmt.Errorf("--file must be a bare snapshot name inside the backups directory, not a path: %q", name)
	}
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("--file is not a valid snapshot name: %q", name)
	}

	cleanDir := filepath.Clean(dir)
	src := filepath.Join(cleanDir, name)
	// Belt-and-suspenders: confirm the resolved path is a direct child of
	// the backups directory even after Clean (catches anything the
	// basename check above might miss on edge-case inputs).
	if filepath.Dir(src) != cleanDir {
		return fmt.Errorf("refusing to restore from outside the backups directory: %s", src)
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("snapshot not found: %s", src)
	}
	if info.IsDir() {
		return fmt.Errorf("snapshot path is a directory: %s", src)
	}

	dst := dataDir() + "/vibecraft.db"
	if _, err := os.Stat(dst); err == nil {
		// Name the safety net with the CURRENT time, not the snapshot's mtime.
		// The backup is "the live DB as it was at restore time" — using the
		// snapshot's mtime would make two restores of the same snapshot
		// produce the same .bak name, clobbering the first safety copy.
		bak := dst + ".bak-" + nowUTC().Format("2006-01-02T15-04-05.000000000Z")
		if err := os.Rename(dst, bak); err != nil {
			return fmt.Errorf("backing up current db: %w", err)
		}
		fmt.Printf("Existing database moved to %s\n", bak)
	}

	// The live DB runs in WAL mode (PRAGMA journal_mode=WAL), so it leaves
	// -wal and -shm sidecars next to vibecraft.db. The snapshot is a clean
	// single-file VACUUM INTO with no WAL of its own. If we leave the OLD
	// DB's sidecars in place, the daemon's next open sees a -wal whose header
	// belongs to the previous database and checkpoints it onto the freshly
	// restored main file — corrupting (or silently reverting) the restore.
	// Remove the stale sidecars so the restored DB opens clean.
	for _, sidecar := range []string{dst + "-wal", dst + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale WAL sidecar %s: %w", sidecar, err)
		}
	}

	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("copying snapshot: %w", err)
	}
	if err := os.Chmod(dst, 0600); err != nil {
		return fmt.Errorf("chmod restored db: %w", err)
	}
	fmt.Printf("Restored %s → %s\n", src, dst)
	fmt.Println("Start the daemon: sudo systemctl start vibecraft-daemon")
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}
