// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/sandbox"
)

// systems.go is the daemon-side surface for the manager's authored
// "systems" — the persistent scheduled work that lives under
// ~/systems/<name>/. The manager creates a system by writing files
// (run.sh, manifest.json, crontab) directly; supercronic in the
// per-sandbox scheduler picks them up within ~15s. That happy path
// works inside the sandbox, no API needed.
//
// Uninstall is the part that doesn't work from a flat-file edit. The
// agent-shell catch-all ~/crontab is owned by the sandbox identity
// inside the namespace, but the manager's PER-SYSTEM crontab line is
// frequently mirrored into the catch-all as well (or starts there in
// the first place, when the manager prefers one schedule file). The
// manager also needs to remove the system's directory atomically with
// the crontab cleanup so supercronic never sees a half-removed state.
// One API call, idempotent, with audit — instead of "edit two files
// by hand and hope you got the regex right."
//
// Auth is withLocalhostAuth (same as /daemon/task, /routes, /notify):
// the manager calls via /run/vibecraft.sock with the local-token
// header. The dashboard intentionally cannot uninstall a system —
// the manager owns its own systems and the customer directs the
// manager.

const systemsRoot = "/home/vibecraft/systems"

// systemNameOK validates the {name} path segment. Same restrictions
// the install path quietly enforces (directory under systems/): lowercase
// alphanumerics + dash + underscore, must start with a letter or digit,
// 1-64 chars. Rejects "." / ".." / "" / anything with slashes — all
// the path-traversal vectors. Tight enough that the resolved absolute
// path is guaranteed to live directly under systemsRoot.
var systemNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// crontabHomes are the supercronic crontabs we scrub when uninstalling
// a system. The per-system one is removed by deleting the system's
// directory (which contains its own crontab), so the only top-level
// crontab to clean is the agent-shell catch-all at ~/crontab. Kept as
// a slice so a future per-user catch-all (say, ~/.config/cron/crontab)
// can be added without changing the call sites.
func crontabHomes() []string {
	return []string{"/home/vibecraft"}
}

// systemEntry is the on-the-wire shape for GET /api/systems. Stays
// small so the manager can list dozens of systems in one round trip
// without a paging dance.
type systemEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	HasCrontab   bool   `json:"has_crontab"`
	CrontabLines int    `json:"crontab_lines"` // non-comment, non-blank
	LastModified string `json:"last_modified"` // RFC3339; dir mtime
	HasRunSh     bool   `json:"has_run_sh"`    // run.sh present + executable
	HasManifest  bool   `json:"has_manifest"`  // manifest.json (locked) present
	HasProposed  bool   `json:"has_proposed"`  // manifest.proposed.json present
	LastRunAt    string `json:"last_run_at"`   // most-recent mtime under logs/, RFC3339
}

// handleSystems lists all authored systems. GET only — the manager
// uses this for "show me what I have running" introspection and for
// the dashboard's per-system status panel. Read-only, no auth-gated
// secrets in the response (paths are well-known).
func (s *Server) handleSystems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries, err := os.ReadDir(systemsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			jsonResponse(w, http.StatusOK, map[string]any{"systems": []systemEntry{}})
			return
		}
		jsonError(w, fmt.Sprintf("read systems dir: %v", err), http.StatusInternalServerError)
		return
	}
	out := make([]systemEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !systemNamePattern.MatchString(name) {
			continue // skip anything that doesn't look like a system dir
		}
		out = append(out, describeSystem(name))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	jsonResponse(w, http.StatusOK, map[string]any{"systems": out})
}

func describeSystem(name string) systemEntry {
	dir := filepath.Join(systemsRoot, name)
	e := systemEntry{Name: name, Path: dir}
	if fi, err := os.Stat(dir); err == nil {
		e.LastModified = fi.ModTime().UTC().Format(time.RFC3339)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err == nil {
		e.HasRunSh = true
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		e.HasManifest = true
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.proposed.json")); err == nil {
		e.HasProposed = true
	}
	if data, err := os.ReadFile(filepath.Join(dir, "crontab")); err == nil {
		e.HasCrontab = true
		e.CrontabLines = countCronLines(data)
	}
	if mt, ok := mostRecentMtime(filepath.Join(dir, "logs")); ok {
		e.LastRunAt = mt.UTC().Format(time.RFC3339)
	}
	return e
}

func countCronLines(data []byte) int {
	n := 0
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
	}
	return n
}

func mostRecentMtime(dir string) (time.Time, bool) {
	var best time.Time
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().After(best) {
			best = info.ModTime()
			found = true
		}
		return nil
	})
	return best, found
}

// uninstallResult tells the manager exactly what changed. Helps the
// follow-up confirmation message ("removed 1 cron line from
// ~/crontab; deleted ~/systems/foo/") without needing a second
// round-trip to verify.
type uninstallResult struct {
	Status           string   `json:"status"` // "uninstalled" | "not_found"
	DirectoryRemoved bool     `json:"directory_removed"`
	CrontabsEdited   []string `json:"crontabs_edited"` // absolute paths
	LinesRemoved     int      `json:"lines_removed"`
	Name             string   `json:"name"`
}

// handleSystemByName dispatches /api/systems/{name} and
// /api/systems/{name}/uninstall. GET on /{name} returns a single
// entry (mirrors the list shape). POST on /{name}/uninstall does the
// idempotent teardown. Anything else returns 405.
//
// Idempotency: a re-uninstall of an already-gone system returns 200
// with directory_removed=false, lines_removed=0, status="not_found".
// The manager can fire-and-forget without distinguishing "already
// cleaned up" from "never existed."
func (s *Server) handleSystemByName(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/systems/")
	if rest == "" {
		jsonError(w, "system name is required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	if !systemNamePattern.MatchString(name) {
		jsonError(w, "invalid system name (must match [a-z0-9][a-z0-9_-]*)", http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		// /api/systems/{name} → single-entry describe
		dir := filepath.Join(systemsRoot, name)
		if _, err := os.Stat(dir); err != nil {
			jsonError(w, "system not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, http.StatusOK, describeSystem(name))

	case action == "uninstall" && r.Method == http.MethodPost:
		result := s.doUninstallSystem(name)
		jsonResponse(w, http.StatusOK, result)

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// doUninstallSystem is the actual teardown. Split out from the HTTP
// handler so it's straight to unit-test (no httptest dance) and so
// future callers (a daemon-internal "stop all systems on disk pause"
// path, say) can reuse it without re-implementing the crontab regex.
//
// Order matters: scrub crontabs FIRST, then delete the directory. If
// we delete the dir first and the crontab edit fails, supercronic
// keeps trying to exec a missing script every interval until the next
// daemon restart. Scrubbing first means a partial failure leaves a
// usable system: the cron stops firing the moment the crontab is
// rewritten (mtime change → supercronic reload within 15s), and the
// stale directory can be cleaned up on retry. The reverse order
// leaves the worst-case "scheduled job spamming 'file not found'
// logs forever" state we just shipped a fix for.
func (s *Server) doUninstallSystem(name string) uninstallResult {
	result := uninstallResult{Name: name, Status: "uninstalled"}

	dir := filepath.Join(systemsRoot, name)
	dirExists := false
	if _, err := os.Stat(dir); err == nil {
		dirExists = true
	}

	// 1. Strip any line whose command path lives under this system's dir.
	//    We match the path substring, not the cron schedule itself —
	//    `*/10 * * * * /home/vibecraft/systems/foo/run.sh` is matched by
	//    "/systems/foo/" regardless of schedule expression. Comment lines
	//    that reference the system (one line above) are also dropped if
	//    they're immediately followed by a stripped command line.
	for _, home := range crontabHomes() {
		path := filepath.Join(home, "crontab")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // file doesn't exist or unreadable; nothing to scrub
		}
		newData, removed := stripSystemFromCrontab(data, name)
		if removed == 0 {
			continue
		}
		// Atomic write: tmp + rename. supercronic's mtime poll sees a
		// single transition, never a partial file.
		tmp := path + ".vc-uninstall.tmp"
		if err := os.WriteFile(tmp, newData, 0600); err != nil {
			s.auditLog.Log(audit.Entry{
				Action: "system_uninstall_crontab_write_failed", Category: "systems",
				Details: fmt.Sprintf("name=%s path=%s err=%v", name, path, err), RiskLevel: "medium",
			})
			continue
		}
		// Repair ownership in case the file was created root-owned by
		// a pre-fix daemon — without this the rename succeeds but
		// inside-namespace the manager still can't edit it on the next
		// install/uninstall.
		sandbox.EnsureCrontabOwnership(home)
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			s.auditLog.Log(audit.Entry{
				Action: "system_uninstall_crontab_rename_failed", Category: "systems",
				Details: fmt.Sprintf("name=%s path=%s err=%v", name, path, err), RiskLevel: "medium",
			})
			continue
		}
		sandbox.EnsureCrontabOwnership(home)
		result.CrontabsEdited = append(result.CrontabsEdited, path)
		result.LinesRemoved += removed
	}

	// 2. Remove the system's directory (also drops its per-system
	//    crontab, manifests, logs, state). rm -rf semantics.
	if dirExists {
		if err := os.RemoveAll(dir); err != nil {
			// Audit + return a partial result. Don't error the request:
			// the crontab is already cleaned (if it had entries), so the
			// system has stopped running. Stale directory can be removed
			// by retrying.
			s.auditLog.Log(audit.Entry{
				Action: "system_uninstall_rmdir_failed", Category: "systems",
				Details: fmt.Sprintf("name=%s dir=%s err=%v", name, dir, err), RiskLevel: "medium",
			})
		} else {
			result.DirectoryRemoved = true
		}
	}

	// 3. Tear down the per-system sandbox. Each system has its own
	//    persistent bwrap sandbox supervising its own supercronic
	//    (sandbox/system_linux.go::GetOrCreateSystemSandbox). Without
	//    this teardown, supercronic INSIDE the sandbox keeps firing
	//    the (now-deleted) ~/systems/<name>/run.sh every interval until
	//    the next daemon restart — logging "file not found" forever
	//    even though the filesystem is clean. The pre-v0.40.4 uninstall
	//    happened to look clean only because the daemon auto-update
	//    that shipped the API itself restarted the daemon; subsequent
	//    uninstalls would have left zombie supercronics. Idempotent:
	//    no-op if no sandbox was ever spawned for this name.
	sandbox.DestroySystemSandbox(name)

	if !dirExists && result.LinesRemoved == 0 {
		result.Status = "not_found"
	}

	s.auditLog.Log(audit.Entry{
		Action: "system_uninstalled", Category: "systems",
		Details: fmt.Sprintf("name=%s dir_removed=%v lines_removed=%d crontabs=%v",
			name, result.DirectoryRemoved, result.LinesRemoved, result.CrontabsEdited),
		RiskLevel: "low",
	})
	return result
}

// stripSystemFromCrontab returns crontab content with every line whose
// command references this system removed, plus the count. Comment
// lines that are immediately followed by a removed command line are
// also dropped (the manager's install pattern is to write a `#
// comment\n*/10 * * * * /path` pair, and leaving the orphan comment
// in place is ugly).
//
// Conservative match: requires "/systems/<name>/" as a path segment —
// not just substring "<name>" — so a system named "log" doesn't
// accidentally match an unrelated `/var/log/...` cron job.
func stripSystemFromCrontab(data []byte, name string) ([]byte, int) {
	needle := "/systems/" + name + "/"
	lines := strings.Split(string(data), "\n")

	// First pass: mark removable lines (the command itself).
	removable := make([]bool, len(lines))
	removed := 0
	for i, line := range lines {
		// Strip trailing CR for Windows-style line endings in case a
		// pasted-in crontab snuck them through.
		trimmed := strings.TrimRight(line, "\r")
		if strings.Contains(trimmed, needle) && !strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
			removable[i] = true
			removed++
		}
	}
	if removed == 0 {
		return data, 0
	}
	// Second pass: also remove immediately-preceding comment lines that
	// belong to the marked command. Walk backwards from each marked line
	// and keep marking contiguous comment lines until we hit a non-
	// comment, non-blank line. Stop at blank lines so unrelated comment
	// blocks above aren't dragged in.
	for i := range removable {
		if !removable[i] {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			trimmed := strings.TrimSpace(strings.TrimRight(lines[j], "\r"))
			if trimmed == "" {
				break
			}
			if strings.HasPrefix(trimmed, "#") && !removable[j] {
				removable[j] = true
				continue
			}
			break
		}
	}
	// Rebuild.
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if removable[i] {
			continue
		}
		out = append(out, line)
	}
	// Collapse runs of 3+ blank lines down to 2 — purely cosmetic but
	// keeps the file from growing slack over many install/uninstall
	// cycles.
	collapsed := make([]string, 0, len(out))
	blank := 0
	for _, line := range out {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank <= 2 {
				collapsed = append(collapsed, line)
			}
			continue
		}
		blank = 0
		collapsed = append(collapsed, line)
	}
	return []byte(strings.Join(collapsed, "\n")), removed
}
