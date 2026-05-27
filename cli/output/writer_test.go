// SPDX-License-Identifier: Apache-2.0

package output

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/cli/exit"
	"github.com/carloslfu/computer.md/cli/schema"
)

type sampleData struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type sampleWithText struct {
	Name string `json:"name"`
}

func (s sampleWithText) TextFormat() string { return "name=" + s.Name }

// withMode temporarily swaps the package-global mode for one test and
// restores it on cleanup. Avoids cross-test bleed because Mode is a
// process-global.
func withMode(t *testing.T, m Mode) {
	t.Helper()
	prev := CurrentMode()
	SetMode(m)
	t.Cleanup(func() { SetMode(prev) })
}

func TestEmitJSON(t *testing.T) {
	withMode(t, ModeJSON)
	var buf bytes.Buffer
	if err := EmitTo(&buf, sampleData{Name: "vc-abc", Count: 7}); err != nil {
		t.Fatalf("EmitTo: %v", err)
	}
	want := `{"v":1,"ok":true,"data":{"name":"vc-abc","count":7}}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestEmitText_WithFormatter(t *testing.T) {
	withMode(t, ModeText)
	var buf bytes.Buffer
	if err := EmitTo(&buf, sampleWithText{Name: "vc-abc"}); err != nil {
		t.Fatalf("EmitTo: %v", err)
	}
	if got := buf.String(); got != "name=vc-abc\n" {
		t.Errorf("got %q", got)
	}
}

func TestEmitText_FallbackToJSON(t *testing.T) {
	// Types without TextFormat() in text mode fall back to indented JSON
	// rather than silently emitting nothing. Better to surface the data
	// than to swallow it.
	withMode(t, ModeText)
	var buf bytes.Buffer
	if err := EmitTo(&buf, sampleData{Name: "x", Count: 1}); err != nil {
		t.Fatalf("EmitTo: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"name": "x"`) {
		t.Errorf("expected indented JSON fallback, got %q", got)
	}
}

func TestEmitError_JSON(t *testing.T) {
	withMode(t, ModeJSON)
	var buf bytes.Buffer
	EmitErrorTo(&buf, schema.Newf(schema.CodeAuthRequired, "no creds"))
	want := `{"v":1,"ok":false,"error":{"code":"auth_required","message":"no creds"}}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestEmitError_Text(t *testing.T) {
	withMode(t, ModeText)
	var buf bytes.Buffer
	EmitErrorTo(&buf, schema.Newf(schema.CodeAuthRequired, "no creds").WithHint("run vibecraft auth login"))
	got := buf.String()
	if !strings.Contains(got, "Error: no creds") {
		t.Errorf("missing message line: %q", got)
	}
	if !strings.Contains(got, "Hint:  run vibecraft auth login") {
		t.Errorf("missing hint line: %q", got)
	}
}

func TestEmitError_PlainErrorWrapped(t *testing.T) {
	withMode(t, ModeJSON)
	var buf bytes.Buffer
	EmitErrorTo(&buf, errors.New("boom"))
	got := buf.String()
	if !strings.Contains(got, `"code":"internal_error"`) {
		t.Errorf("plain errors should map to internal_error code: %q", got)
	}
	if !strings.Contains(got, `"message":"boom"`) {
		t.Errorf("plain error message should pass through: %q", got)
	}
}

func TestEmitError_OutcomeIsSilent(t *testing.T) {
	withMode(t, ModeJSON)
	var buf bytes.Buffer
	EmitErrorTo(&buf, &exit.OutcomeError{Code: exit.TaskFailed})
	if got := buf.String(); got != "" {
		t.Errorf("OutcomeError should emit nothing (output already written by command), got %q", got)
	}
}

func TestEmitError_NilIsSafe(t *testing.T) {
	var buf bytes.Buffer
	EmitErrorTo(&buf, nil)
	if got := buf.String(); got != "" {
		t.Errorf("nil error should emit nothing, got %q", got)
	}
}

func TestEmitEvent(t *testing.T) {
	var buf bytes.Buffer
	err := EmitEventTo(&buf, schema.EventStatus, "2026-05-20T00:00:00Z", map[string]any{
		"task":   "abc-123",
		"status": "running",
	})
	if err != nil {
		t.Fatalf("EmitEventTo: %v", err)
	}
	got := buf.String()
	// Field ordering inside a Go map is non-deterministic when marshaled
	// to JSON, but encoding/json sorts map keys alphabetically. Lock that
	// in: every event line is byte-stable for any given input.
	want := `{"event":"status","status":"running","task":"abc-123","ts":"2026-05-20T00:00:00Z","v":1}` + "\n"
	if got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestEmitEvent_RejectsReservedFields(t *testing.T) {
	var buf bytes.Buffer
	for _, reserved := range []string{"v", "event", "ts"} {
		err := EmitEventTo(&buf, schema.EventStatus, "t", map[string]any{reserved: "leak"})
		if err == nil {
			t.Errorf("expected error when caller passes reserved field %q", reserved)
		}
	}
}
