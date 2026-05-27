// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/carloslfu/computer.md/daemon/manager"
)

func contextWithControl(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyCookieAccess, "control")
}

func withTempManagerKeyPaths(t *testing.T) {
	t.Helper()
	oldKeyPath := openAIKeyPath
	oldModePath := managerKeyModePath
	oldModelPath := managerModelPath
	oldConfigDir := managerConfigDirPath
	dir := t.TempDir()
	openAIKeyPath = filepath.Join(dir, "openai.key")
	managerKeyModePath = filepath.Join(dir, "manager_key_mode")
	managerModelPath = filepath.Join(dir, "manager_model")
	managerConfigDirPath = dir
	t.Cleanup(func() {
		openAIKeyPath = oldKeyPath
		managerKeyModePath = oldModePath
		managerModelPath = oldModelPath
		managerConfigDirPath = oldConfigDir
	})
}

func TestHandleManagerKey_StoresOperatorKeyWithoutManualRestart(t *testing.T) {
	withTempManagerKeyPaths(t)
	srv := newBudgetGateTestServer(t, 200.0, true)
	srv.cfg = &Config{
		ManagerKeyMode:           "operator",
		ManagerUnavailableReason: "This connected computer needs your OpenAI API key before the manager can run.",
	}
	srv.manager = manager.NewClient("")

	body, _ := json.Marshal(map[string]string{
		"openai_key": "sk-test-12345678901234567890",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/manager-key", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithControl(req.Context()))
	w := httptest.NewRecorder()
	srv.handleManagerKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if srv.cfg.OpenAIKey != "sk-test-12345678901234567890" {
		t.Fatalf("cfg key not updated")
	}
	if srv.cfg.ManagerKeyMode != "operator" || srv.cfg.ManagerUnavailableReason != "" {
		t.Fatalf("cfg not ready after save: %+v", srv.cfg)
	}
	keyBytes, err := os.ReadFile(openAIKeyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if string(keyBytes) != "sk-test-12345678901234567890" {
		t.Fatalf("key file mismatch")
	}
	st, err := os.Stat(openAIKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("key mode = %v, want 0600", st.Mode().Perm())
	}
	modeBytes, err := os.ReadFile(managerKeyModePath)
	if err != nil {
		t.Fatalf("read mode: %v", err)
	}
	if string(modeBytes) != "operator" {
		t.Fatalf("mode file = %q", string(modeBytes))
	}
}

func TestHandleManagerKey_RequiresControlAccess(t *testing.T) {
	withTempManagerKeyPaths(t)
	srv := newBudgetGateTestServer(t, 200.0, true)
	srv.cfg = &Config{ManagerKeyMode: "operator"}

	body, _ := json.Marshal(map[string]string{"openai_key": "sk-test-12345678901234567890"})
	req := httptest.NewRequest(http.MethodPost, "/api/manager-key", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleManagerKey(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
