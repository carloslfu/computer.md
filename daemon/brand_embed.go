// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
)

// brand_embed.go ships the brand & design baseline (brand.md, next to
// this file) inside the daemon binary and writes it to
// /usr/share/vibecraft/brand.md on every startup. The manager's
// prompt (daemon/prompt.md) points at that path; every machine on
// every daemon update therefore has the latest design language
// without a cloud-init dance.
//
// IMPORTANT — path choice: this MUST live outside /etc/vibecraft/.
// The daemon's guardrail policy (daemon/guardrails/policies.go) blocks
// every read of /etc/vibecraft/* from the agent shell as defense for
// the credential files (daemon.token, openai.key, vault.key, ...).
// brand.md inside that directory was correctly blocked but defeated
// the whole point (the manager couldn't read it). /usr/share/vibecraft
// is the conventional path for read-only data shipped by the daemon
// binary; no guardrails apply there.
//
// We OVERWRITE on each startup rather than skip-if-present so a
// stale brand.md from an older daemon doesn't outlive its update.
// The file is world-readable (0644) — there are no secrets in
// the brand guide, and Claude Code / Codex workers run as the
// vibecraft user, so they need to be able to `cat` it.

//go:embed brand.md
var embeddedBrandMD []byte

const brandMDPath = "/usr/share/vibecraft/brand.md"

// writeBrandFile drops the embedded brand.md to the canonical path.
// Called once from main() during daemon startup. Failures are
// non-fatal (the manager has a fallback: PRODUCT.md sets the brand
// voice and is shipped via the repo).
func writeBrandFile() {
	if err := os.MkdirAll(filepath.Dir(brandMDPath), 0o755); err != nil {
		log.Printf("brand_embed: mkdir %s: %v (continuing)", filepath.Dir(brandMDPath), err)
		return
	}
	tmp := brandMDPath + ".tmp"
	if err := os.WriteFile(tmp, embeddedBrandMD, 0o644); err != nil {
		log.Printf("brand_embed: write %s: %v (continuing)", tmp, err)
		return
	}
	if err := os.Rename(tmp, brandMDPath); err != nil {
		_ = os.Remove(tmp)
		log.Printf("brand_embed: rename to %s: %v (continuing)", brandMDPath, err)
		return
	}
}
