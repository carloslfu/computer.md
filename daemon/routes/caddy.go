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

// caddyfileCmd builds `caddy <sub> --adapter caddyfile --config <path>`.
//
// The `--adapter caddyfile` flag is required and must be explicit: `caddy
// validate/reload --config <file>` only auto-detects the Caddyfile format when
// the file is literally named "Caddyfile". Our validation temp files
// (".Caddyfile.*.validate") are not, so without the adapter Caddy parses them as
// JSON and fails with "config is not valid JSON ... did you mean the --adapter
// flag?" — which silently broke every validate-before-apply (and thus every
// hosted-tool route change that goes through safeWriteAndReload).
//
// HOME is set so Caddy can resolve its user-config dir; the daemon's process
// environment may define neither $HOME nor $XDG_CONFIG_HOME, which otherwise
// makes Caddy warn and fall back to writing in the current directory.
func caddyfileCmd(sub, configPath string) *exec.Cmd {
	cmd := exec.Command("caddy", sub, "--adapter", "caddyfile", "--config", configPath)
	cmd.Env = append(os.Environ(), "HOME="+os.TempDir())
	return cmd
}

// validateCaddy validates the given Caddy configuration path.
func validateCaddy(path string) error {
	if out, err := caddyfileCmd("validate", path).CombinedOutput(); err != nil {
		return fmt.Errorf("Caddyfile validation failed: %s", string(out))
	}
	return nil
}

// reloadCaddy reloads the already-written Caddy configuration.
func reloadCaddy() error {
	if out, err := caddyfileCmd("reload", caddyfilePath).CombinedOutput(); err != nil {
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
				_ = caddyfileCmd("reload", caddyfilePath).Run()
			}
		}
		return err
	}

	return nil
}
