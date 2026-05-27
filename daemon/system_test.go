// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression for May 2026: dashboard's Machine settings tab was
// hard-coding the AWS plan defaults (8 vCPU, 32 GB) for every machine,
// including BYOM. A 16-core/32-thread 128 GB Hetzner box rendered as
// "8 vCPU, 32 GB RAM". Daemon now exposes /system; these tests pin
// the parser behavior so the response stays honest.

func TestCollectSystemInfoReturnsRealValues(t *testing.T) {
	info := collectSystemInfo()

	// Threads must be at least 1 (runtime.NumCPU never returns 0 on a
	// running system).
	if info.CPU.Threads < 1 {
		t.Fatalf("expected at least 1 thread, got %d", info.CPU.Threads)
	}
	// Cores fallback path: when /proc/cpuinfo is unreadable (macOS dev
	// machines), Cores collapses to Threads — never zero.
	if info.CPU.Cores < 1 {
		t.Errorf("expected cores >= 1 (fallback to threads), got %d", info.CPU.Cores)
	}
	if info.Hostname == "" {
		t.Errorf("expected non-empty hostname")
	}
}

// Verify the /proc/cpuinfo parser correctly extracts cores AND model
// from a realistic multi-line sample (the user's actual machine —
// AMD Ryzen 9 7950X3D, 16 physical cores, 32 threads).
func TestReadCPUFromProcParses7950X3D(t *testing.T) {
	tmp := t.TempDir()
	procPath := filepath.Join(tmp, "cpuinfo")
	// Truncated but realistic /proc/cpuinfo. The key lines repeat per
	// logical CPU; the parser should pick the highest "cpu cores"
	// value seen (16) and the first "model name".
	sample := strings.Join([]string{
		"processor	: 0",
		"model name	: AMD Ryzen 9 7950X3D 16-Core Processor",
		"cpu cores	: 16",
		"",
		"processor	: 1",
		"model name	: AMD Ryzen 9 7950X3D 16-Core Processor",
		"cpu cores	: 16",
		"",
		"processor	: 31",
		"model name	: AMD Ryzen 9 7950X3D 16-Core Processor",
		"cpu cores	: 16",
		"",
	}, "\n")
	if err := os.WriteFile(procPath, []byte(sample), 0644); err != nil {
		t.Fatal(err)
	}
	model, cores := readCPUFromFile(procPath)
	if cores != 16 {
		t.Errorf("expected 16 cores, got %d", cores)
	}
	if !strings.Contains(model, "7950X3D") {
		t.Errorf("expected model to mention 7950X3D, got %q", model)
	}
}

// /proc/meminfo parse — must convert kB → GiB correctly and not
// over/under-report by an order of magnitude.
func TestReadMemTotal128GB(t *testing.T) {
	tmp := t.TempDir()
	procPath := filepath.Join(tmp, "meminfo")
	// 131768432 kB = ~125 GiB (a real 128 GB Hetzner box, after kernel
	// reservations). Parser must not return 0 or 128 — must return 125.
	sample := strings.Join([]string{
		"MemTotal:       131768432 kB",
		"MemFree:         24123456 kB",
		"MemAvailable:   117654321 kB",
	}, "\n")
	if err := os.WriteFile(procPath, []byte(sample), 0644); err != nil {
		t.Fatal(err)
	}
	gb := readMemTotalFromFile(procPath)
	if gb != 125 {
		t.Errorf("expected 125 GiB from 131768432 kB, got %d", gb)
	}
}

// Regression for May 2026: the Agents panel reported "Claude Code —
// Missing" on a machine where the worker could launch fine. The probe
// ran `su - vibecraft -c "claude --version"` and trusted ~/.bashrc to
// add ~/.npm-global/bin to PATH — but Ubuntu's stock .bashrc returns
// early for non-interactive shells, before that appended line. The fix
// sets PATH explicitly, mirroring the worker-spawn env. These pin it
// for every worker the probe runs against (claude + codex).
func TestProbeShellResolvesLikeAWorker(t *testing.T) {
	for _, bin := range []string{"claude", "codex"} {
		t.Run(bin, func(t *testing.T) {
			s := probeShell(bin)

			if !strings.Contains(s, ".npm-global/bin") {
				t.Errorf("probe must put ~/.npm-global/bin on PATH (worker-spawn parity); got %q", s)
			}
			if !strings.Contains(s, bin+" --version") {
				t.Errorf("probe must run %s --version; got %q", bin, s)
			}
			// The load-bearing assertion: the probe must set PATH itself,
			// not depend on shell rc files. If this regresses to relying
			// on .bashrc the panel lies again.
			if !strings.Contains(s, "export PATH=") {
				t.Errorf("probe must export PATH explicitly (not rely on .bashrc); got %q", s)
			}
		})
	}
}

// detectAgents is what the /system endpoint hands the dashboard's
// Agents panel. It must list both preinstalled worker CLIs (Claude
// Code + Codex). If either is silently dropped, the panel shows the
// missing one as "Not configured" and the customer cannot tell whether
// the worker is genuinely absent or whether the daemon just forgot to
// look — same false-Missing class of bug the probeShell test pins
// from a different angle.
func TestDetectAgentsListsBothPreinstalledWorkers(t *testing.T) {
	agents := detectAgents()
	if len(agents) != 2 {
		t.Fatalf("expected 2 preinstalled workers (Claude Code + Codex), got %d", len(agents))
	}
	wantCommands := map[string]bool{"claude": false, "codex": false}
	for _, a := range agents {
		if _, ok := wantCommands[a.Command]; ok {
			wantCommands[a.Command] = true
		}
	}
	for cmd, seen := range wantCommands {
		if !seen {
			t.Errorf("detectAgents missing worker with command=%q", cmd)
		}
	}
}

// /system HTTP handler — must return JSON with the exact tag names
// the dashboard reads. Schema drift would silently render "—".
func TestSystemEndpointJSONShape(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("GET", "/api/system", nil)
	w := httptest.NewRecorder()
	srv.handleSystem(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Pin the JSON tag names. If any of these get renamed, the
	// dashboard fails open with no data.
	var parsed map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	required := []string{"cpu", "memory_gb", "hostname", "kernel", "uptime_seconds", "daemon_version"}
	for _, k := range required {
		if _, ok := parsed[k]; !ok {
			t.Errorf("missing required top-level field %q", k)
		}
	}
	cpu, ok := parsed["cpu"].(map[string]interface{})
	if !ok {
		t.Fatal("cpu field is not an object")
	}
	for _, k := range []string{"model", "cores", "threads"} {
		if _, ok := cpu[k]; !ok {
			t.Errorf("missing required cpu.%s", k)
		}
	}
}
