// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"

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

// daemonRunning is a best-effort check based on a pidfile or the systemd
// socket. If neither signal is available, returns nil with running=false
// — the operator's --force flag is the escape hatch.
func daemonRunning() (bool, error) {
	if _, err := os.Stat("/run/vibecraft-daemon.pid"); err == nil {
		return true, nil
	}
	if _, err := os.Stat("/run/systemd/units/vibecraft-daemon.service"); err == nil {
		return true, nil
	}
	return false, nil
}

func restoreSnapshot(name string) error {
	dir, err := backupsDir()
	if err != nil {
		return err
	}
	src := dir + "/" + name
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("snapshot not found: %s", src)
	}
	if info.IsDir() {
		return fmt.Errorf("snapshot path is a directory: %s", src)
	}

	dst := dataDir() + "/vibecraft.db"
	if _, err := os.Stat(dst); err == nil {
		bak := dst + ".bak-" + info.ModTime().UTC().Format("2006-01-02T15-04-05Z")
		if err := os.Rename(dst, bak); err != nil {
			return fmt.Errorf("backing up current db: %w", err)
		}
		fmt.Printf("Existing database moved to %s\n", bak)
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
