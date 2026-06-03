// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

func TestFilesCat_JSONEmitsBase64(t *testing.T) {
	resetVersionHandshake()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/files/home/vibecraft/blob.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0xff, 0xfe, 'A'})
	}))
	defer srv.Close()

	isolateEnv(t, srv.URL, "vc_machine_test_test_test_xyz")
	withOutputMode(t, output.ModeJSON)

	out, err := captureStdout(t, func() error {
		return runFilesCat(filesCatCmd, []string{"/home/vibecraft/blob.bin"})
	})
	if err != nil {
		t.Fatalf("runFilesCat: %v", err)
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
			Size     int    `json:"size"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("envelope did not parse: %v\nout=%q", err, out)
	}
	if !env.OK {
		t.Fatalf("ok: got false")
	}
	if env.Data.Encoding != "base64" || env.Data.Content != "//5B" || env.Data.Size != 3 {
		t.Fatalf("data: got encoding=%q content=%q size=%d", env.Data.Encoding, env.Data.Content, env.Data.Size)
	}
}

func TestValidateInboxPushRemote(t *testing.T) {
	tests := []struct {
		name     string
		local    string
		remote   string
		wantCode string
		wantOK   bool
	}{
		{name: "inbox dir", local: "report.pdf", remote: "/home/vibecraft/inbox/", wantOK: true},
		{name: "same basename", local: "report.pdf", remote: "/home/vibecraft/inbox/report.pdf", wantOK: true},
		{name: "stdin chooses name", local: "-", remote: "/home/vibecraft/inbox/data.csv", wantOK: true},
		{name: "outside inbox", local: "report.pdf", remote: "/home/vibecraft/systems/report.pdf", wantCode: schema.CodeValidationError},
		{name: "prefix sibling", local: "report.pdf", remote: "/home/vibecraft/inboxevil/report.pdf", wantCode: schema.CodeValidationError},
		{name: "local rename unsupported", local: "report.pdf", remote: "/home/vibecraft/inbox/final.pdf", wantCode: schema.CodeValidationError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInboxPushRemote(tt.local, tt.remote)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("validateInboxPushRemote: %v", err)
				}
				return
			}
			se, ok := err.(*schema.Error)
			if !ok || se.Code != tt.wantCode {
				t.Fatalf("error: got %T %v, want code %s", err, err, tt.wantCode)
			}
		})
	}
}
