// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
)

type systemSchedulerStarter func(name string) error

func scheduledSystemNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !systemNamePattern.MatchString(entry.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "crontab"))
		if err != nil || countCronLines(data) == 0 {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func startSystemSchedulerReconciler(ctx context.Context, root string, auditLog *audit.Logger, start systemSchedulerStarter, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}

	go func() {
		started := map[string]bool{}
		reconcile := func() {
			names, err := scheduledSystemNames(root)
			if err != nil {
				log.Printf("system scheduler: scan failed: %v", err)
				if auditLog != nil {
					auditLog.Log(audit.Entry{
						Action:    "system_scheduler_scan_failed",
						Category:  "systems",
						Details:   err.Error(),
						RiskLevel: "low",
					})
				}
				return
			}

			current := make(map[string]bool, len(names))
			for _, name := range names {
				current[name] = true
				if started[name] {
					continue
				}
				if err := start(name); err != nil {
					log.Printf("system scheduler: start %s failed: %v", name, err)
					if auditLog != nil {
						auditLog.Log(audit.Entry{
							Action:    "system_scheduler_start_failed",
							Category:  "systems",
							Details:   fmt.Sprintf("name=%s err=%v", name, err),
							RiskLevel: "medium",
						})
					}
					continue
				}
				started[name] = true
				log.Printf("system scheduler: started %s", name)
				if auditLog != nil {
					auditLog.Log(audit.Entry{
						Action:    "system_scheduler_started",
						Category:  "systems",
						Details:   "name=" + name,
						RiskLevel: "low",
					})
				}
			}

			for name := range started {
				if !current[name] {
					delete(started, name)
				}
			}
		}

		reconcile()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcile()
			}
		}
	}()
}
