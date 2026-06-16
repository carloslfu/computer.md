// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/sandbox"
)

// handleSpawnWorker is the daemon-side worker-spawn entrypoint
// (D3: explicit helper). The agent calls it via
// /usr/local/bin/vc-spawn-worker. The command ALWAYS runs inside a real
// per-worker sandbox (namespaces + secret-mask + default-deny egress
// allowlist + per-sandbox socket — the daemon/sandbox primitive,
// real-kernel-proven). No feature flag. The only non-sandboxed path is
// the macOS dev build (sandbox.Supported()==false), which never serves
// real traffic.
//
// Localhost-token-gated like /daemon/task: only daemon-spawned shells
// carry $VIBECRAFT_LOCAL_TOKEN (Phase 0b).
func (s *Server) handleSpawnWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name       string   `json:"name"`
		System     string   `json:"system"`
		Cmd        []string `json:"cmd"`
		AllowFQDNs []string `json:"allow_fqdns"`
		AllowCIDRs []string `json:"allow_cidrs"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || len(req.Cmd) == 0 {
		jsonError(w, "name and cmd are required", http.StatusBadRequest)
		return
	}

	// No feature flag: workers are ALWAYS sandboxed when the OS supports
	// it (Linux). The only non-sandboxed path is the macOS dev build
	// (sandbox.Supported()==false via the stub), which never serves real
	// traffic. Per-workload-sandboxing plan: flag removed, full migration.
	sandboxed := sandbox.Supported()

	s.auditLog.Log(audit.Entry{
		Action:   "worker_spawn",
		Category: "sandbox",
		Details:  fmt.Sprintf("name=%s system=%s sandboxed=%t", req.Name, req.System, sandboxed),
	})

	if !sandboxed {
		// Only reached on the macOS dev build (no Linux namespaces).
		// Never serves real traffic; kept so the daemon still builds and
		// behaves on dev.
		out, err := s.shell.Execute(r.Context(), strings.Join(req.Cmd, " "))
		jsonResponse(w, http.StatusOK, map[string]any{
			"sandboxed": false, "output": out, "error": errStr(err),
		})
		return
	}

	// Sandboxed path. Phase 3: a worker spawned WITHIN a system inherits
	// that system's egress allowlist (never the agent's broader scope) —
	// the marketing system's worker can't reach the bookkeeping
	// system's APIs. Plus the worker-CLI backends every spawn needs
	// (Claude Code → api.anthropic.com; Codex → *.openai.com for the
	// API + auth subdomains, chatgpt.com for the OAuth landing used
	// by `codex login`), plus any explicit per-call additions. Without
	// the OpenAI/ChatGPT entries here, spawning a `codex` worker
	// silently fails its first network call — same shape as the
	// pre-fix "Claude Code is Missing" bug, just at the egress layer
	// instead of the install layer. Unmanifested/standalone workers
	// get just this baseline + the explicit list. Provider keys are not
	// injected into worker env.
	egress := sandbox.EgressPolicy{
		AllowFQDNs: append([]string{
			"api.anthropic.com",
			"*.openai.com",
			"chatgpt.com",
		}, req.AllowFQDNs...),
		AllowCIDRs: req.AllowCIDRs,
	}
	if req.System != "" {
		if sysEg, _, serr := sandbox.SystemEgress("/home/vibecraft/systems", req.System); serr == nil {
			egress.AllowFQDNs = append(egress.AllowFQDNs, sysEg.AllowFQDNs...)
			egress.AllowCIDRs = append(egress.AllowCIDRs, sysEg.AllowCIDRs...)
		} else {
			// The system manifest exists but could not be loaded (parse/IO
			// error). The worker falls back to baseline egress — fail-closed
			// (narrower than the system's intended allowlist, never broader),
			// but a silent contract drift: the worker may then fail a network
			// call to an endpoint the system manifest meant to permit. Audit
			// the load failure so it is observable instead of silent.
			s.auditLog.Log(audit.Entry{
				Action:    "worker_spawn_system_egress_load_failed",
				Category:  "sandbox",
				Details:   fmt.Sprintf("name=%s system=%s err=%v (worker continues with baseline egress only)", req.Name, req.System, serr),
				RiskLevel: "low",
			})
		}
	}
	// Parent-system scope for the worker sandbox. A worker manifest MUST
	// declare a non-empty ParentID (sandbox.Validate fails closed
	// otherwise — that is the per-system scoping invariant), so a
	// standalone/no-system spawn cannot pass req.System straight through:
	// it would arrive empty and Create would reject the manifest, failing
	// EVERY standalone spawn on Linux despite the handler doc above
	// promising "Unmanifested/standalone workers get just this baseline".
	// Use a synthetic, syntactically-valid parent for that case. It is
	// purely the manifest's ParentID label (never resolved to a dir,
	// socket, or nftables set), and because the SystemEgress augmentation
	// above is gated on req.System, the standalone worker still gets only
	// the baseline + explicit egress — exactly as documented.
	parentSystem := req.System
	if parentSystem == "" {
		parentSystem = "standalone"
	}
	env := map[string]string{}
	// Codex (the peer worker) authenticates through `codex login`
	// (ChatGPT subscription); the token lives in ~/.codex/ inside the
	// shared $HOME, so a spawned codex worker just reuses it. No
	// OPENAI_API_KEY injection. Claude Code follows the same
	// subscription/session posture; VibeCraft-hosted keys stay out of
	// worker env.
	out, err := sandbox.SpawnWorker(r.Context(), req.Name, parentSystem, egress, req.Cmd, env)
	jsonResponse(w, http.StatusOK, map[string]any{
		"sandboxed": true, "output": out, "error": errStr(err),
	})
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
