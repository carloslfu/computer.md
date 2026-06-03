// SPDX-License-Identifier: Apache-2.0

package routes

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// writeCaddyfileAtomic writes content via a random same-directory temp and
// rename. A predictable temp path in /etc/caddy is unnecessary, and writing
// the live Caddyfile before validation creates an avoidable bad-config window.
func writeCaddyfileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp Caddyfile in %s: %w", dir, err)
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing temp Caddyfile: %w", err)
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod temp Caddyfile: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temp Caddyfile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp Caddyfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename Caddyfile into place: %w", err)
	}
	cleanup = false
	return nil
}

func writeTempCaddyfileForValidation(content string) (string, error) {
	dir := filepath.Dir(caddyfilePath)
	f, err := os.CreateTemp(dir, "."+filepath.Base(caddyfilePath)+".*.validate")
	if err != nil {
		return "", fmt.Errorf("creating temp Caddyfile in %s: %w", dir, err)
	}
	tmpPath := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("writing temp Caddyfile: %w", err)
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("chmod temp Caddyfile: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("closing temp Caddyfile: %w", err)
	}
	return tmpPath, nil
}

// validateCaddy validates the given Caddy configuration path.
func validateCaddy(path string) error {
	validate := exec.Command("caddy", "validate", "--config", path)
	if out, err := validate.CombinedOutput(); err != nil {
		return fmt.Errorf("Caddyfile validation failed: %s", string(out))
	}
	return nil
}

// reloadCaddy reloads the already-written Caddy configuration.
func reloadCaddy() error {
	reload := exec.Command("caddy", "reload", "--config", caddyfilePath)
	if out, err := reload.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy reload failed: %s", string(out))
	}
	return nil
}

// safeWriteAndReload validates the new Caddyfile before it becomes live,
// atomically installs it, and reloads Caddy. If reload fails, the previous
// Caddyfile is restored and reloaded best-effort.
func safeWriteAndReload(content string) error {
	// Read the old Caddyfile for rollback.
	oldContent, readErr := os.ReadFile(caddyfilePath)

	tmpPath, err := writeTempCaddyfileForValidation(content)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	if err := validateCaddy(tmpPath); err != nil {
		return err
	}

	if err := writeCaddyfileAtomic(caddyfilePath, content); err != nil {
		return err
	}

	if err := reloadCaddy(); err != nil {
		// Rollback: restore old Caddyfile if we had one.
		if readErr == nil && len(oldContent) > 0 {
			if writeErr := writeCaddyfileAtomic(caddyfilePath, string(oldContent)); writeErr != nil {
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
