// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/carloslfu/computer.md/daemon/audit"
)

// migrateHostCrontab is the Phase 3 one-shot host-crontab migration. The
// pre-sandboxing host crontab is the agent's own loops (the system
// concept didn't formally exist before Phase 3, so entries aren't tagged
// by system). On first boot of a Phase-3 daemon we:
//
//  1. snapshot `crontab -l -u vibecraft`
//  2. move every entry into the AGENT-SHELL sandbox's persisted crontab
//     spool (the catch-all — entries that don't belong to a specific
//     system run as the manager's own scheduled tasks). The manager
//     reclassifies into per-system crontabs over time.
//  3. wipe the host crontab so no host-level vibecraft schedule remains.
//
// Idempotent via a marker so a reboot/restart never re-migrates (which
// would resurrect entries the manager has since moved). Best-effort:
// every failure is logged, the daemon continues — a crontab migration
// hiccup must never block startup. On the macOS dev host there is no
// `vibecraft` crontab; the marker is still written so it's a clean
// no-op.
func migrateHostCrontab(homeHost string, auditLog *audit.Logger) {
	const marker = "/etc/vibecraft/.crontab-migrated"
	if _, err := os.Stat(marker); err == nil {
		return // already done
	}

	defer func() {
		// Always drop the marker, even on a no-op/empty/error path, so we
		// attempt exactly once. (A genuine migration that partially failed
		// is logged + audited below; re-running risks duplicate/zombie
		// entries, which is worse than a logged one-shot.)
		if err := os.WriteFile(marker, []byte("done\n"), 0600); err != nil {
			log.Printf("crontab-migrate: could not write marker: %v", err)
		}
	}()

	out, err := exec.Command("crontab", "-l", "-u", "vibecraft").Output()
	if err != nil {
		// "no crontab for vibecraft" exits non-zero — the common, clean
		// case (nothing to migrate).
		log.Printf("crontab-migrate: no host crontab to migrate (%v)", err)
		return
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		log.Printf("crontab-migrate: host crontab empty — nothing to migrate")
		return
	}

	// The agent-shell scheduler is supercronic reading <home>/crontab
	// (a plain file in the identity-mapped HOME, already bound into the
	// sandbox — no spool dir, no Vixie/setgid machinery). Append the
	// host entries under a provenance header rather than clobbering any
	// crontab the manager already authored.
	crontabFile := filepath.Join(homeHost, "crontab")
	prefix := ""
	if existing, rerr := os.ReadFile(crontabFile); rerr == nil && len(existing) > 0 {
		prefix = string(existing) + "\n"
	}
	merged := prefix + "# --- migrated from the pre-sandboxing host crontab ---\n" + body + "\n"
	if err := os.WriteFile(crontabFile, []byte(merged), 0600); err != nil {
		log.Printf("crontab-migrate: write crontab: %v", err)
		return
	}
	// Owned by the sandbox identity. chownToAgent is the same helper
	// LoadConfig uses for the inbox; a no-op when the vibecraft user
	// doesn't exist (dev).
	chownToAgent(crontabFile)

	if err := exec.Command("crontab", "-r", "-u", "vibecraft").Run(); err != nil {
		log.Printf("crontab-migrate: WARNING host crontab NOT wiped (entries copied to agent-shell; host copy still live — manual `crontab -r -u vibecraft` advised): %v", err)
		if auditLog != nil {
			auditLog.Log(audit.Entry{
				Action: "host_crontab_migrate_partial", Category: "security",
				Details: "entries copied to agent-shell crontab; host wipe failed", RiskLevel: "medium",
			})
		}
		return
	}

	log.Printf("crontab-migrate: moved host crontab into the agent-shell sandbox; host crontab wiped")
	if auditLog != nil {
		auditLog.Log(audit.Entry{
			Action: "host_crontab_migrated", Category: "security",
			Details: "host vibecraft crontab moved into agent-shell sandbox spool", RiskLevel: "low",
		})
	}
}
