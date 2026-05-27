// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"time"
)

const setupCodeOperatorOpenAIKey = "operator_openai_key_required"

type SetupField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

type SetupStored struct {
	At string `json:"at"`
}

type SetupRequestPayload struct {
	Code         string       `json:"code"`
	MessageID    string       `json:"message_id,omitempty"`
	Title        string       `json:"title"`
	Body         string       `json:"body"`
	Footnote     string       `json:"footnote,omitempty"`
	PrimaryLabel string       `json:"primary_label"`
	SettingsPath string       `json:"settings_path,omitempty"`
	Fields       []SetupField `json:"fields,omitempty"`
	Stored       *SetupStored `json:"stored,omitempty"`
}

func operatorOpenAIKeySetupPayload(messageID string) SetupRequestPayload {
	return SetupRequestPayload{
		Code:      setupCodeOperatorOpenAIKey,
		MessageID: messageID,
		Title:     "Connect this computer's manager",
		Body:      "This connected computer needs your OpenAI API key before the manager can run tasks. Add it once. It stays on this machine.",
		Footnote:  "Connected machines use your OpenAI account directly. VibeCraft hosted credits are not used for these manager calls.",
		Fields: []SetupField{{
			Name:        "openai_key",
			Label:       "OpenAI API key",
			Type:        "token",
			Placeholder: "sk-...",
			Hint:        "Use a key from your OpenAI project.",
		}},
		PrimaryLabel: "Save key",
		SettingsPath: "/settings?tab=manager",
	}
}

func setupPayloadJSON(payload SetupRequestPayload) string {
	b, _ := json.Marshal(payload)
	return string(b)
}

func decodeSetupRequestJSON(raw string) (SetupRequestPayload, bool) {
	var p SetupRequestPayload
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return p, false
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, false
	}
	if p.Code == "" || p.Title == "" {
		return p, false
	}
	return p, true
}

func markSetupStored(payload SetupRequestPayload) SetupRequestPayload {
	payload.Stored = &SetupStored{At: time.Now().UTC().Format(time.RFC3339)}
	return payload
}
