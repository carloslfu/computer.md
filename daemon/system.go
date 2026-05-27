// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SystemInfo is the response shape for GET /system. The dashboard's
// Machine settings tab reads this for BYOM machines so it can render
// real hardware specs instead of the (misleading) plan defaults.
//
// JSON tags pinned by tests on both sides — change them and the
// dashboard will silently fall back to "—".
type SystemInfo struct {
	CPU       CPUInfo     `json:"cpu"`
	MemoryGB  int         `json:"memory_gb"`
	Hostname  string      `json:"hostname"`
	Kernel    string      `json:"kernel"`
	UptimeSec int64       `json:"uptime_seconds"`
	Version   string      `json:"daemon_version"`
	Agents    []AgentInfo `json:"agents"`
}

type CPUInfo struct {
	Model   string `json:"model"`
	Cores   int    `json:"cores"`   // physical cores (single-socket assumed)
	Threads int    `json:"threads"` // logical CPUs (cores × SMT factor)
}

// AgentInfo describes a worker agent installed on the machine. The
// dashboard's Settings → Agents panel renders this list. v1 covered
// Claude Code only; the current shape covers both Claude Code and
// OpenAI's Codex CLI — and stays generic so a future customer-
// installed worker (a custom runner, a new vendor CLI) can show up
// without changing the response shape.
type AgentInfo struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Version   string `json:"version,omitempty"`
	Installed bool   `json:"installed"`
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonResponse(w, http.StatusOK, collectSystemInfo())
}

func collectSystemInfo() SystemInfo {
	info := SystemInfo{
		Version: version,
		CPU: CPUInfo{
			Threads: runtime.NumCPU(),
		},
	}
	info.CPU.Model, info.CPU.Cores = readCPUFromFile("/proc/cpuinfo")
	if info.CPU.Cores == 0 {
		// /proc/cpuinfo unreadable (non-Linux? sandbox?) — fall back to
		// logical CPU count so we still render something honest.
		info.CPU.Cores = info.CPU.Threads
	}
	info.MemoryGB = readMemTotalFromFile("/proc/meminfo")
	info.Hostname, _ = os.Hostname()
	info.Kernel = readKernelRelease()
	info.UptimeSec = readUptimeSeconds()
	info.Agents = detectAgents()
	return info
}

// detectAgents returns the list of worker agents the manager can spawn
// on this machine. Covers the two preinstalled worker CLIs (Claude
// Code, Codex); the function is structured so new workers (customer-
// installed runners) can be added without touching callers.
func detectAgents() []AgentInfo {
	return []AgentInfo{
		probeAgent("Claude Code", "claude"),
		probeAgent("Codex", "codex"),
	}
}

func answerLocalMachineFact(instruction string) string {
	q := strings.ToLower(instruction)
	wantsInstall := strings.Contains(q, "installed") ||
		strings.Contains(q, "available") ||
		strings.Contains(q, "there") ||
		strings.Contains(q, "version") ||
		strings.Contains(q, "agents")

	if strings.Contains(q, "codex") && wantsInstall {
		for _, a := range detectAgents() {
			if strings.EqualFold(a.Command, "codex") {
				if a.Installed {
					if a.Version != "" {
						return "Yes. Codex is installed on this computer: " + a.Version + "."
					}
					return "Yes. Codex is installed on this computer."
				}
				return "Codex is not installed on this computer yet."
			}
		}
	}

	if (strings.Contains(q, "claude code") || strings.Contains(q, "claude")) && wantsInstall {
		for _, a := range detectAgents() {
			if strings.EqualFold(a.Command, "claude") {
				if a.Installed {
					if a.Version != "" {
						return "Yes. Claude Code is installed on this computer: " + a.Version + "."
					}
					return "Yes. Claude Code is installed on this computer."
				}
				return "Claude Code is not installed on this computer yet."
			}
		}
	}

	if strings.Contains(q, "daemon") && strings.Contains(q, "version") {
		return "This computer is running VibeCraft daemon " + version + "."
	}

	if strings.Contains(q, "worker") && strings.Contains(q, "agents") {
		agents := detectAgents()
		var installed []string
		for _, a := range agents {
			if a.Installed {
				label := a.Name
				if a.Version != "" {
					label += " (" + a.Version + ")"
				}
				installed = append(installed, label)
			}
		}
		if len(installed) == 0 {
			return "No worker agents are installed on this computer yet."
		}
		return "Installed worker agents: " + strings.Join(installed, "; ") + "."
	}

	return ""
}

// probeShellPATH mirrors the PATH the daemon gives every worker shell
// (daemon/computer/shell.go). Detection MUST resolve binaries the same
// way a spawned worker would or the panel lies: ~/.npm-global/bin (the
// npm-global prefix both install paths pin) is NOT on a stock Ubuntu
// non-interactive login shell's PATH. Its ~/.bashrc returns early for
// non-interactive shells — before the appended PATH line ever runs —
// so a bare `su - vibecraft -c "claude --version"` reports "Missing"
// even when the worker can launch claude fine.
const probeShellPATH = "$HOME/.npm-global/bin:$HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// probeShell builds the `su -c` script that resolves an agent binary
// the same way a worker would. command is a trusted in-process literal
// (see detectAgents), never user input. Split out for unit testing.
func probeShell(command string) string {
	return `export PATH="` + probeShellPATH + `"; command -v ` + command + ` >/dev/null 2>&1 && exec ` + command + ` --version`
}

// probeAgent runs `<cmd> --version` as the vibecraft user with the
// worker-spawn PATH and returns the trimmed output. Bounded by a
// timeout so an unresponsive binary can't hang the /system endpoint.
func probeAgent(name, command string) AgentInfo {
	info := AgentInfo{Name: name, Command: command}

	// 5s, not 2s: `su -` plus node startup for `claude --version` can
	// exceed 2s on a loaded box, and a tight timeout is itself a false
	// "Missing". Still bounded so the endpoint can't hang.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "su", "-", "vibecraft", "-c", probeShell(command))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return info
	}
	info.Installed = true
	info.Version = strings.TrimSpace(string(out))
	return info
}

// readCPUFromFile returns (model, cores) parsed from a /proc/cpuinfo
// formatted file. Cores = highest "cpu cores" value encountered
// (single-socket assumption — fine for the BYOM use case, where
// multi-socket is vanishingly rare). Returns ("", 0) on any read/parse
// failure; caller must fall back.
func readCPUFromFile(path string) (string, int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()

	var model string
	var cores int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "model name":
			if model == "" {
				model = val
			}
		case "cpu cores":
			if n, err := strconv.Atoi(val); err == nil && n > cores {
				cores = n
			}
		}
	}
	return model, cores
}

// readMemTotalFromFile returns total memory in whole GiB (1024-based,
// matching `free -h`). Reads a /proc/meminfo formatted file. Returns
// 0 on failure.
func readMemTotalFromFile(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		// MemTotal is in kB. 1 GiB = 1024 * 1024 kB.
		return int(kb / 1024 / 1024)
	}
	return 0
}

func readKernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUptimeSeconds() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}
