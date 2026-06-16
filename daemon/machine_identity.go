// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/carloslfu/computer.md/daemon/audit"
)

const maxMachineNameRunes = 80

func (s *Server) handleMachineIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{
		"id":   s.cfg.MachineID,
		"name": s.machineDisplayName(),
		"host": s.cfg.MachineHost,
	})
}

func (s *Server) handleManagementMachineName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	// readJSON (vs a bare Decode) bounds the body to 1MB and rejects
	// trailing garbage, matching every other handler in this package.
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name, ok := normalizeMachineDisplayName(req.Name)
	if !ok {
		jsonError(w, "machine name must be 1-80 printable characters", http.StatusBadRequest)
		return
	}
	if err := s.writeMachineDisplayName(name); err != nil {
		jsonError(w, "failed to save machine name", http.StatusInternalServerError)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "machine_name_synced",
		Category: "system",
		Details:  "name=" + name,
	})
	jsonResponse(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) machineDisplayName() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	if s.cfg.MachineNamePath != "" {
		if name, err := readFileString(s.cfg.MachineNamePath); err == nil && name != "" {
			return name
		}
	}
	if s.cfg.MachineName != "" {
		return s.cfg.MachineName
	}
	return s.cfg.MachineID
}

func (s *Server) writeMachineDisplayName(name string) error {
	path := ""
	if s != nil && s.cfg != nil {
		path = s.cfg.MachineNamePath
	}
	if path == "" {
		path = defaultMachineNamePath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(name+"\n"), 0644)
}

func normalizeMachineDisplayName(input string) (string, bool) {
	name := strings.Join(strings.Fields(strings.TrimSpace(input)), " ")
	if name == "" || utf8.RuneCountInString(name) > maxMachineNameRunes {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}
