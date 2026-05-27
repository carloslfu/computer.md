// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineIdentityReadsLocalNameMirror(t *testing.T) {
	ts := newTestServer(t)
	namePath := filepath.Join(t.TempDir(), "machine.name")
	ts.server.cfg.MachineNamePath = namePath
	if err := os.WriteFile(namePath, []byte("Operations Box\n"), 0644); err != nil {
		t.Fatalf("write name mirror: %v", err)
	}

	w := ts.do(t, http.MethodGet, "/machine", "Bearer "+ts.signJWT(t, "user-1"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /machine = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "Operations Box" {
		t.Fatalf("name = %q, want Operations Box", body["name"])
	}
	if body["id"] != ts.machineID || body["host"] == "" {
		t.Fatalf("identity missing id/host: %#v", body)
	}
}

func TestManagementMachineNameWritesLocalMirror(t *testing.T) {
	ts := newTestServer(t)
	namePath := filepath.Join(t.TempDir(), "machine.name")
	ts.server.cfg.MachineNamePath = namePath

	w := ts.do(t, http.MethodPost, "/management/machine-name", "", `{"name":"Ops"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST = %d, want 401", w.Code)
	}

	w = ts.do(
		t,
		http.MethodPost,
		"/management/machine-name",
		"Bearer "+ts.healthTok,
		`{"name":"  Sales   Ops  "}`,
	)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /management/machine-name = %d: %s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(namePath)
	if err != nil {
		t.Fatalf("read name mirror: %v", err)
	}
	if string(data) != "Sales Ops\n" {
		t.Fatalf("mirror = %q, want normalized name", string(data))
	}

	w = ts.do(t, http.MethodGet, "/machine", "Bearer "+ts.signJWT(t, "user-1"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /machine = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "Sales Ops" {
		t.Fatalf("name = %q, want Sales Ops", body["name"])
	}

	w = ts.do(
		t,
		http.MethodPost,
		"/management/machine-name",
		"Bearer "+ts.healthTok,
		`{"name":"`+strings.Repeat("x", 81)+`"}`,
	)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("overlong name POST = %d, want 400", w.Code)
	}
}
