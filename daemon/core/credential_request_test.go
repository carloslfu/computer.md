// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateCredentialSpecs(t *testing.T) {
	tests := []struct {
		name    string
		fields  []CredentialSpec
		wantErr string // substring match on the error message; empty = expect no error
	}{
		{
			name:    "rejects empty list",
			fields:  nil,
			wantErr: "at least one field",
		},
		{
			name: "accepts a single valid field",
			fields: []CredentialSpec{
				{Name: "API_KEY", Label: "API key", Type: CredFieldToken},
			},
		},
		{
			name: "accepts default type (empty string)",
			fields: []CredentialSpec{
				{Name: "EMAIL", Label: "Email"},
			},
		},
		{
			name: "rejects too many fields",
			fields: []CredentialSpec{
				{Name: "A", Label: "l"}, {Name: "B", Label: "l"}, {Name: "C", Label: "l"},
				{Name: "D", Label: "l"}, {Name: "E", Label: "l"}, {Name: "F", Label: "l"},
				{Name: "G", Label: "l"}, {Name: "H", Label: "l"}, {Name: "I", Label: "l"},
			},
			wantErr: "too many fields",
		},
		{
			name:    "rejects missing name",
			fields:  []CredentialSpec{{Name: "", Label: "X"}},
			wantErr: "name is required",
		},
		{
			name:    "rejects lowercase name",
			fields:  []CredentialSpec{{Name: "apiKey", Label: "X"}},
			wantErr: "must match",
		},
		{
			name:    "rejects name starting with digit",
			fields:  []CredentialSpec{{Name: "1KEY", Label: "X"}},
			wantErr: "must match",
		},
		{
			name:    "rejects name with dash",
			fields:  []CredentialSpec{{Name: "API-KEY", Label: "X"}},
			wantErr: "must match",
		},
		{
			name: "rejects name longer than cap",
			fields: []CredentialSpec{
				{Name: strings.Repeat("A", maxCredNameLen+1), Label: "X"},
			},
			wantErr: "exceeds",
		},
		{
			name: "rejects duplicate names",
			fields: []CredentialSpec{
				{Name: "API_KEY", Label: "one"},
				{Name: "API_KEY", Label: "two"},
			},
			wantErr: "duplicate",
		},
		{
			name:    "rejects missing label",
			fields:  []CredentialSpec{{Name: "KEY", Label: ""}},
			wantErr: "label is required",
		},
		{
			name: "rejects label longer than cap",
			fields: []CredentialSpec{
				{Name: "KEY", Label: strings.Repeat("L", maxCredLabelLen+1)},
			},
			wantErr: "label exceeds",
		},
		{
			name: "rejects hint longer than cap",
			fields: []CredentialSpec{
				{Name: "KEY", Label: "Key", Hint: strings.Repeat("H", maxCredHintLen+1)},
			},
			wantErr: "hint exceeds",
		},
		{
			name: "rejects invalid type",
			fields: []CredentialSpec{
				{Name: "KEY", Label: "Key", Type: "secret"},
			},
			wantErr: "invalid type",
		},
		{
			name: "accepts all valid types",
			fields: []CredentialSpec{
				{Name: "A", Label: "a", Type: CredFieldText},
				{Name: "B", Label: "b", Type: CredFieldPassword},
				{Name: "C", Label: "c", Type: CredFieldURL},
				{Name: "D", Label: "d", Type: CredFieldToken},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCredentialSpecs(tc.fields)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseCredentialRequestInput(t *testing.T) {
	t.Run("parses a well-formed request", func(t *testing.T) {
		fields := []CredentialSpec{
			{Name: "HUBSPOT_EMAIL", Label: "Email", Type: CredFieldText},
			{Name: "HUBSPOT_PASSWORD", Label: "Password", Type: CredFieldPassword, Hint: "same as your portal login"},
		}
		fieldsJSON, _ := json.Marshal(fields)
		input := map[string]string{
			"title":  "HubSpot login",
			"reason": "To log in and pull your contacts",
			"fields": string(fieldsJSON),
		}
		p, err := ParseCredentialRequestInput(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Title != "HubSpot login" {
			t.Errorf("title mismatch: %q", p.Title)
		}
		if len(p.Fields) != 2 {
			t.Fatalf("expected 2 fields, got %d", len(p.Fields))
		}
		if p.Fields[1].Type != CredFieldPassword {
			t.Errorf("field type mismatch: %q", p.Fields[1].Type)
		}
		if p.Fields[1].Hint == "" {
			t.Error("hint dropped")
		}
	})

	t.Run("rejects missing title", func(t *testing.T) {
		input := map[string]string{
			"fields": `[{"name":"KEY","label":"Key"}]`,
		}
		_, err := ParseCredentialRequestInput(input)
		if err == nil || !strings.Contains(err.Error(), "title is required") {
			t.Fatalf("expected title-required error, got: %v", err)
		}
	})

	t.Run("rejects title longer than cap", func(t *testing.T) {
		input := map[string]string{
			"title":  strings.Repeat("T", maxCredTitleLen+1),
			"fields": `[{"name":"KEY","label":"Key"}]`,
		}
		_, err := ParseCredentialRequestInput(input)
		if err == nil || !strings.Contains(err.Error(), "title exceeds") {
			t.Fatalf("expected title-exceeds error, got: %v", err)
		}
	})

	t.Run("rejects malformed fields JSON", func(t *testing.T) {
		input := map[string]string{
			"title":  "X",
			"fields": "not an array {{{",
		}
		_, err := ParseCredentialRequestInput(input)
		if err == nil || !strings.Contains(err.Error(), "fields must be a JSON array") {
			t.Fatalf("expected JSON parse error, got: %v", err)
		}
	})

	t.Run("surfaces validator errors", func(t *testing.T) {
		input := map[string]string{
			"title":  "X",
			"fields": `[{"name":"lowercase","label":"x"}]`,
		}
		_, err := ParseCredentialRequestInput(input)
		if err == nil || !strings.Contains(err.Error(), "must match") {
			t.Fatalf("expected name-pattern error, got: %v", err)
		}
	})

	t.Run("trims whitespace from title and reason", func(t *testing.T) {
		input := map[string]string{
			"title":  "  Title with padding  ",
			"reason": "\n\treason\t\n",
			"fields": `[{"name":"K","label":"l"}]`,
		}
		p, err := ParseCredentialRequestInput(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Title != "Title with padding" {
			t.Errorf("title not trimmed: %q", p.Title)
		}
		if p.Reason != "reason" {
			t.Errorf("reason not trimmed: %q", p.Reason)
		}
	})
}

func TestDecodeCredentialRequestJSON(t *testing.T) {
	t.Run("decodes a full payload", func(t *testing.T) {
		p := CredentialRequestPayload{
			ToolUseID: "toolu_abc",
			TaskID:    "task-1",
			MessageID: "msg-1",
			Title:     "T",
			Reason:    "R",
			Fields:    []CredentialSpec{{Name: "K", Label: "l"}},
		}
		decoded, ok := DecodeCredentialRequestJSON(p.JSON())
		if !ok {
			t.Fatal("expected decode success")
		}
		if decoded.MessageID != "msg-1" {
			t.Errorf("lost message_id on roundtrip")
		}
		if decoded.Fields[0].Name != "K" {
			t.Errorf("lost field name on roundtrip")
		}
	})

	t.Run("preserves stored receipt", func(t *testing.T) {
		p := CredentialRequestPayload{
			Title:  "T",
			Fields: []CredentialSpec{{Name: "K", Label: "l"}},
			Stored: &CredentialStored{At: "2026-04-24T00:00:00Z", Names: []string{"K"}},
		}
		decoded, ok := DecodeCredentialRequestJSON(p.JSON())
		if !ok {
			t.Fatal("expected decode success")
		}
		if decoded.Stored == nil {
			t.Fatal("stored receipt dropped")
		}
		if len(decoded.Stored.Names) != 1 || decoded.Stored.Names[0] != "K" {
			t.Errorf("stored names mismatch: %v", decoded.Stored.Names)
		}
	})

	t.Run("rejects non-JSON input", func(t *testing.T) {
		for _, bad := range []string{"", "   ", "not json", "[array not object]"} {
			if _, ok := DecodeCredentialRequestJSON(bad); ok {
				t.Errorf("expected decode failure for %q", bad)
			}
		}
	})

	t.Run("rejects approval-shaped payloads", func(t *testing.T) {
		// An ApprovalPayload has title + command but no fields. Must NOT
		// be mistakenly decoded as a credential request, or the tasks[]
		// rehydration path on the UI would render the wrong card.
		approval := `{"tool":"bash","command":"ls","title":"List","reason":"x"}`
		if _, ok := DecodeCredentialRequestJSON(approval); ok {
			t.Error("approval payload incorrectly decoded as credential_request")
		}
	})

	t.Run("rejects payload with empty fields array", func(t *testing.T) {
		bad := `{"title":"T","fields":[]}`
		if _, ok := DecodeCredentialRequestJSON(bad); ok {
			t.Error("empty-fields payload should not decode")
		}
	})
}
