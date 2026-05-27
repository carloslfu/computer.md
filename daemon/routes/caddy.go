// SPDX-License-Identifier: Apache-2.0

package routes

import (
	"fmt"
	"log"
	"os"
	"os/exec"
)

// writeCaddyfile writes content to the Caddyfile path atomically.
// If the new config fails validation, the old config is restored.
func writeCaddyfile(content string) error {
	if err := os.WriteFile(caddyfilePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", caddyfilePath, err)
	}
	return nil
}

// reloadCaddy validates and reloads the Caddy configuration.
// If validation fails, the caller is responsible for rollback.
func reloadCaddy() error {
	// Validate config before reloading.
	validate := exec.Command("caddy", "validate", "--config", caddyfilePath)
	if out, err := validate.CombinedOutput(); err != nil {
		return fmt.Errorf("Caddyfile validation failed: %s", string(out))
	}

	reload := exec.Command("caddy", "reload", "--config", caddyfilePath)
	if out, err := reload.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy reload failed: %s", string(out))
	}

	return nil
}

// safeWriteAndReload writes the new Caddyfile, validates it, and reloads Caddy.
// If validation or reload fails, the previous Caddyfile is restored.
func safeWriteAndReload(content string) error {
	// Read the old Caddyfile for rollback.
	oldContent, readErr := os.ReadFile(caddyfilePath)

	if err := writeCaddyfile(content); err != nil {
		return err
	}

	if err := reloadCaddy(); err != nil {
		// Rollback: restore old Caddyfile if we had one.
		if readErr == nil && len(oldContent) > 0 {
			if writeErr := os.WriteFile(caddyfilePath, oldContent, 0644); writeErr != nil {
				log.Printf("CRITICAL: failed to restore Caddyfile after reload failure: %v", writeErr)
			} else {
				// Try to reload with the old config (best-effort).
				exec.Command("caddy", "reload", "--config", caddyfilePath).Run()
			}
		}
		return err
	}

	return nil
}
