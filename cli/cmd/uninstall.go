// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the VibeCraft CLI and its local config from this machine",
	Long: `Reverse a 'curl ... | sh' install: delete the local config directory
(~/.config/vibecraft/ — credentials + lock file) and the vibecraft
binary itself, then report what was removed.

  vibecraft uninstall

This is a LOCAL teardown. It does not revoke the account or machine key
server-side — revoke that from the dashboard (Settings -> CLI) if the
key should stop working everywhere. On Windows a running .exe cannot
delete itself, so the command reports its path for you to remove.`,
	RunE: runUninstall,
}

func init() {
	rootCmd.AddCommand(uninstallCmd)
}

func runUninstall(cmd *cobra.Command, args []string) error {
	removed := []string{}

	// 1. Local config directory: ~/.config/vibecraft/ holds config.json
	//    (credentials), the .lock file, and any leftover .tmp. This is
	//    the same credential cleanup as `auth logout`, widened to the
	//    whole directory so nothing is left behind.
	home, err := os.UserHomeDir()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "finding home directory: %s", err.Error())
	}
	cfgDir := filepath.Join(home, configDir)
	hadConfig := false
	if _, statErr := os.Stat(cfgDir); statErr == nil {
		if rmErr := os.RemoveAll(cfgDir); rmErr != nil {
			return schema.Newf(schema.CodeInternal, "removing %s: %s", cfgDir, rmErr.Error())
		}
		removed = append(removed, cfgDir)
		hadConfig = true
	}

	// 2. The binary itself. On Unix, unlinking a running executable is
	//    safe — the inode survives until this process exits, so the
	//    command finishes cleanly. On Windows a running .exe cannot be
	//    removed; we report the path so the user can delete it.
	binPath, exeErr := os.Executable()
	if exeErr == nil {
		if resolved, symErr := filepath.EvalSymlinks(binPath); symErr == nil {
			binPath = resolved
		}
	}
	binaryRemoved := false
	if exeErr == nil && runtime.GOOS != "windows" {
		if rmErr := os.Remove(binPath); rmErr == nil {
			binaryRemoved = true
			removed = append(removed, binPath)
		}
	}

	return output.Emit(schema.UninstallData{
		Removed:       removed,
		BinaryPath:    binPath,
		BinaryRemoved: binaryRemoved,
		Note:          uninstallNote(hadConfig, binaryRemoved, binPath, exeErr == nil),
	})
}

// uninstallNote composes the human follow-up: a binary that could not be
// auto-deleted (Windows, a root-owned --system install, or an
// os.Executable failure) needs a manual rm, and credential cleanup is
// always local-only — the server-side key is revoked from the dashboard.
func uninstallNote(hadConfig, binaryRemoved bool, binPath string, knowBinPath bool) string {
	var parts []string
	if !binaryRemoved {
		switch {
		case !knowBinPath:
			parts = append(parts, "Could not locate the vibecraft binary on disk; remove it manually.")
		case runtime.GOOS == "windows":
			parts = append(parts, "A running Windows binary cannot delete itself; remove it manually: "+binPath)
		default:
			parts = append(parts, "Could not remove the binary (it may need sudo); remove it manually: rm "+binPath)
		}
	}
	if hadConfig {
		parts = append(parts, "Local credentials were deleted. This does not revoke them server-side; revoke the key from the dashboard (Settings -> CLI) if it should stop working everywhere.")
	}
	return strings.Join(parts, " ")
}
