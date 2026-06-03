// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	prev := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = prev }()

	runErr := fn()
	_ = w.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), runErr
}

func TestResolveAPIKeyInputWarnsOnLiteralFlag(t *testing.T) {
	stderr, err := captureStderr(t, func() error {
		key, err := resolveAPIKeyInput("vc_machine_literal_test_key", "")
		if err != nil {
			return err
		}
		if key != "vc_machine_literal_test_key" {
			t.Fatalf("key: got %q", key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("resolveAPIKeyInput: %v", err)
	}
	if !strings.Contains(stderr, "visible in shell history") {
		t.Fatalf("stderr missing warning: %q", stderr)
	}
}
