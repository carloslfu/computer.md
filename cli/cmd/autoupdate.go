// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Background CLI self-update. The daemon already auto-updates on a
// 5-minute systemd timer; the CLI had no equivalent and silently drifted
// stale until it hit a schema_unsupported error mid-task. maybeAutoUpdate
// closes that gap: at most once per autoUpdateInterval it checks the
// release manifest and, if a newer CLI exists, downloads + verifies +
// atomically swaps the binary so the NEXT invocation runs the new one.
//
// It is strictly best-effort — every failure path is swallowed and never
// touches the user's command, its stdout, or its exit code. The only
// visible effect of a successful update is one line on stderr.

const (
	autoUpdateInterval   = 24 * time.Hour
	autoUpdateStampName  = ".last-update-check"
	autoUpdateDisableEnv = "VIBECRAFT_NO_AUTO_UPDATE"
	autoUpdateBudget     = 20 * time.Second
)

// maybeAutoUpdate is invoked once per command from the root
// PersistentPostRun (after the command runs, so it never delays it).
// cmdName is cobra's leaf cmd.Name(), used to skip commands where a
// self-update is redundant or self-defeating.
func maybeAutoUpdate(cmdName string) {
	if os.Getenv(autoUpdateDisableEnv) != "" {
		return
	}
	// `dev` builds have no real version to compare against.
	if cliVersion == "dev" || cliVersion == "" {
		return
	}
	switch cmdName {
	case "update", "uninstall", "version":
		// update: redundant. uninstall: about to delete the binary.
		// version: keep it instant and side-effect-free.
		return
	}

	stamp, err := autoUpdateStampPath()
	if err != nil {
		return
	}
	if !autoUpdateDue(stamp, autoUpdateInterval) {
		return
	}
	// Record the attempt before doing the work: a failed check must not
	// retry on every following command — the next attempt is one full
	// interval out regardless of outcome.
	writeAutoUpdateStamp(stamp)

	done := make(chan schema.UpdateData, 1)
	go func() {
		data, err := selfUpdate(defaultUpdateBaseURL, false)
		if err != nil {
			done <- schema.UpdateData{}
			return
		}
		done <- data
	}()

	select {
	case data := <-done:
		if data.Status == "updated" {
			announceSelfUpdate(data)
		}
	case <-time.After(autoUpdateBudget):
		// Took too long — abandon. The user's command proceeds; the
		// leaked goroutine ends with the process and any partial
		// <bin>.new file is truncated by the next download.
	}
}

// autoUpdateStampPath returns ~/.config/vibecraft/.last-update-check.
func autoUpdateStampPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDir, autoUpdateStampName), nil
}

// autoUpdateDue reports whether the last recorded check is older than
// interval. A missing or unparseable stamp counts as due.
func autoUpdateDue(stampPath string, interval time.Duration) bool {
	data, err := os.ReadFile(stampPath)
	if err != nil {
		return true
	}
	last, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	return time.Since(last) >= interval
}

// writeAutoUpdateStamp records "now" as the last-checked time. Best-effort.
func writeAutoUpdateStamp(stampPath string) {
	if err := os.MkdirAll(filepath.Dir(stampPath), 0700); err != nil {
		return
	}
	_ = os.WriteFile(stampPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0600)
}

// announceSelfUpdate prints the single visible line for a successful
// background update — on stderr, so stdout stays a clean JSON contract.
func announceSelfUpdate(data schema.UpdateData) {
	if output.CurrentMode() == output.ModeJSON {
		notice, _ := json.Marshal(map[string]any{
			"v":      1,
			"notice": "self_updated",
			"from":   data.CurrentVersion,
			"to":     data.LatestVersion,
		})
		fmt.Fprintln(os.Stderr, string(notice))
		return
	}
	fmt.Fprintf(os.Stderr, "vibecraft self-updated %s → %s (active on next run)\n",
		data.CurrentVersion, data.LatestVersion)
}
