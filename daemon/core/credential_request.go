// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CredentialFieldType enumerates the kinds of input a credential field can be.
// The UI picks an input control per type — text for plain strings, password
// with reveal toggle for secrets, url-validated input for endpoints, token
// for opaque keys that look like a password but aren't a user password.
type CredentialFieldType string

const (
	CredFieldText     CredentialFieldType = "text"
	CredFieldPassword CredentialFieldType = "password"
	CredFieldURL      CredentialFieldType = "url"
	CredFieldToken    CredentialFieldType = "token"
)

// CredentialSpec is one field the agent asks the user to fill. The Name is
// the exact vault variable name the agent will reference later (e.g.
// HUBSPOT_API_KEY). Label is the plain-English label shown above the input.
type CredentialSpec struct {
	Name  string              `json:"name"`
	Label string              `json:"label"`
	Type  CredentialFieldType `json:"type"`
	Hint  string              `json:"hint,omitempty"`
}

// CredentialStored records what was stored when the user submitted the form.
// Only names are kept — values live in the vault, not in chat history.
type CredentialStored struct {
	At    string   `json:"at"`
	Names []string `json:"names"`
}

// CredentialRequestPayload is the full card shape: the ask (title/reason),
// the fields to fill, and the stored receipt once the user submits. Stored
// stays nil until then, which is how the UI knows which state to render.
//
// TaskID and MessageID are embedded so a reloaded or reconnected client
// can wire the card up without side-channel lookups. TaskID tells the
// UI where to POST the submission; MessageID lets SSE replay target
// the persisted bubble for in-place updates.
type CredentialRequestPayload struct {
	ToolUseID string            `json:"tool_use_id"`
	TaskID    string            `json:"task_id,omitempty"`
	MessageID string            `json:"message_id,omitempty"`
	Title     string            `json:"title"`
	Reason    string            `json:"reason,omitempty"`
	Fields    []CredentialSpec  `json:"fields"`
	Stored    *CredentialStored `json:"stored,omitempty"`
	// ExpiredAt is set when the card can no longer be acted on — either the
	// agent gave up waiting, or the task ended (timeout, cancel, panic).
	// Mutually exclusive with Stored; the dashboard renders an expired
	// receipt instead of the input form.
	ExpiredAt string `json:"expired_at,omitempty"`
	// ExpiredReason is a short human-readable note explaining why
	// (e.g. "task ended", "no response within 24h"). Goes under the title
	// on the expired card.
	ExpiredReason string `json:"expired_reason,omitempty"`
}

// JSON serialises the payload for persistence as the message.content column.
func (p CredentialRequestPayload) JSON() string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeCredentialRequestJSON parses a stored credential_request payload.
// Returns false for anything that doesn't look like one so rehydration
// consumers can fall through safely.
//
// The discriminator is Title AND Fields — an ApprovalPayload has Title
// but no Fields, so requiring Fields means this never accidentally
// matches an approval. Mirrors the TS decoder in lib/chat/rehydrate.ts.
func DecodeCredentialRequestJSON(s string) (CredentialRequestPayload, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return CredentialRequestPayload{}, false
	}
	var p CredentialRequestPayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return CredentialRequestPayload{}, false
	}
	if p.Title == "" || len(p.Fields) == 0 {
		return CredentialRequestPayload{}, false
	}
	return p, true
}

// credNameRegex enforces the same shape the SecureInput dashboard form
// enforces: upper-case letters, digits, underscores, starts with a letter.
// Keeping the rule in one place avoids a mismatch where the agent asks
// for a name the vault later refuses.
var credNameRegex = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Length caps on free-text fields. These protect the card layout from a
// pathological or mistaken agent response — a 5000-char title would
// blow out the chat. Numbers chosen to comfortably fit real-world
// labels (OAuth provider names, API key descriptions) while rejecting
// paragraph-length strings.
const (
	maxCredNameLen   = 64
	maxCredLabelLen  = 80
	maxCredTitleLen  = 120
	maxCredReasonLen = 300
	maxCredHintLen   = 200
	maxCredFields    = 8
)

// ValidateCredentialSpecs rejects a request with no fields, a duplicate
// name inside the same request, or a name the vault won't accept. The
// agent gets a tool_error with this message when it mis-forms the call.
func ValidateCredentialSpecs(fields []CredentialSpec) error {
	if len(fields) == 0 {
		return fmt.Errorf("at least one field is required")
	}
	if len(fields) > maxCredFields {
		return fmt.Errorf("too many fields (%d): ask for at most %d credentials in one call", len(fields), maxCredFields)
	}
	seen := make(map[string]bool, len(fields))
	for i, f := range fields {
		if f.Name == "" {
			return fmt.Errorf("field %d: name is required", i)
		}
		if len(f.Name) > maxCredNameLen {
			return fmt.Errorf("field %d: name %q exceeds %d chars", i, f.Name, maxCredNameLen)
		}
		if !credNameRegex.MatchString(f.Name) {
			return fmt.Errorf("field %d: name %q must match [A-Z][A-Z0-9_]*", i, f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("field %d: duplicate name %q", i, f.Name)
		}
		seen[f.Name] = true
		if f.Label == "" {
			return fmt.Errorf("field %d (%s): label is required", i, f.Name)
		}
		if len(f.Label) > maxCredLabelLen {
			return fmt.Errorf("field %d (%s): label exceeds %d chars", i, f.Name, maxCredLabelLen)
		}
		if len(f.Hint) > maxCredHintLen {
			return fmt.Errorf("field %d (%s): hint exceeds %d chars", i, f.Name, maxCredHintLen)
		}
		switch f.Type {
		case "", CredFieldText, CredFieldPassword, CredFieldURL, CredFieldToken:
			// ok — empty defaults to text at render time
		default:
			return fmt.Errorf("field %d (%s): invalid type %q", i, f.Name, f.Type)
		}
	}
	return nil
}

// ParseCredentialRequestInput pulls a CredentialRequestPayload out of the
// tool_use input the manager sends us. The fields/title/reason arrive as JSON
// strings in the map because the manager response parser flattens nested values
// this way.
func ParseCredentialRequestInput(input map[string]string) (CredentialRequestPayload, error) {
	var p CredentialRequestPayload
	p.Title = strings.TrimSpace(input["title"])
	p.Reason = strings.TrimSpace(input["reason"])

	if raw := strings.TrimSpace(input["fields"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.Fields); err != nil {
			return p, fmt.Errorf("fields must be a JSON array: %w", err)
		}
	}
	if p.Title == "" {
		return p, fmt.Errorf("title is required")
	}
	if len(p.Title) > maxCredTitleLen {
		return p, fmt.Errorf("title exceeds %d chars", maxCredTitleLen)
	}
	if len(p.Reason) > maxCredReasonLen {
		return p, fmt.Errorf("reason exceeds %d chars", maxCredReasonLen)
	}
	if err := ValidateCredentialSpecs(p.Fields); err != nil {
		return p, err
	}
	return p, nil
}

// CredentialValue is one name/value pair the user submitted. Label is
// optional metadata the vault stores alongside the value.
type CredentialValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// CredentialResponse is the full submission the HTTP handler turns into
// vault writes. Cancelled is true when the user dismissed the card
// without filling it — the agent gets a tool_error and can react.
type CredentialResponse struct {
	Cancelled bool
	Values    []CredentialValue
}
