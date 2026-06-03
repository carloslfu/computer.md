// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSONRejectsTrailingData(t *testing.T) {
	var out map[string]string
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"ok":"yes"} {"extra":"no"}`))
	if err := readJSON(req, &out); err == nil {
		t.Fatal("expected trailing JSON data to be rejected")
	}
}

func TestReadJSONRejectsOversizedTrailingWhitespace(t *testing.T) {
	var out map[string]string
	body := `{"ok":"yes"}` + strings.Repeat(" ", 1024*1024)
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	if err := readJSON(req, &out); err == nil {
		t.Fatal("expected oversized body to be rejected")
	}
}
