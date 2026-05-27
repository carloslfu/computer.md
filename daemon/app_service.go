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
// picks them up.
const systemdUserDir = "/home/vibecraft/.config/systemd/user"

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
	if err := validateAppServiceEnvironment(req.Environment); err != nil {
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
	if err := validateAppServiceEnvironment(req.Environment); err != nil {
		return result, err
	}

	// 1. Make sure the user systemd dir exists, owned by vibecraft.
	//    On a clean machine it's created by cloud-init / install.sh;
	//    older machines or BYOM weirdness might not have it. Cheap to
	//    re-ensure.
	if err := ensureVibecraftUserDir(systemdUserDir); err != nil {
		return result, fmt.Errorf("ensuring systemd-user dir: %w", err)
	}

	// 1b. Auto-inject the AI credits proxy env vars on Managed machines.
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
	//     proxy: VibeCraft-funded local app AI credits require a relay
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

	// 2. Render and write the unit file. Atomic: tmp + rename. systemd
	//    catches incremental writes if you don't.
	unit := renderUnitFile(req)
	tmpPath := result.UnitPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(unit), 0o644); err != nil {
		return result, fmt.Errorf("writing tmp unit file: %w", err)
	}
	if err := chownToVibecraft(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return result, fmt.Errorf("chown tmp unit: %w", err)
	}
	if err := os.Rename(tmpPath, result.UnitPath); err != nil {
		_ = os.Remove(tmpPath)
		return result, fmt.Errorf("rename unit file into place: %w", err)
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
	for _, kv := range req.Environment {
		// Don't quote — systemd's parser does its own. Reject empty
		// pairs to keep the file clean.
		if strings.TrimSpace(kv) == "" {
			continue
		}
		b.WriteString("Environment=" + kv + "\n")
	}
	b.WriteString("ExecStart=" + req.ExecStart + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=3\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// ensureVibecraftUserDir mkdirs the systemd-user dir if missing and
// chowns it to vibecraft. Idempotent.
func ensureVibecraftUserDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return chownToVibecraft(path)
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
