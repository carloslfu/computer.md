// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// Masker scrubs secret values from text, replacing them with their
// secret names in brackets (e.g., "[STRIPE_API_KEY]").
type Masker struct {
	store *Store
}

// NewMasker creates a masker backed by the given vault store.
func NewMasker(store *Store) *Masker {
	return &Masker{store: store}
}

// Mask replaces all known secret values in text with "[SECRET_NAME]" markers.
// It checks all secrets in the vault and replaces their values.
// Longer values are replaced first to avoid partial matches.
func (m *Masker) Mask(text string) string {
	if text == "" {
		return text
	}

	values := m.store.AllValues()
	if len(values) == 0 {
		return text
	}

	// Sort by value length descending to replace longer secrets first.
	// This prevents partial replacement issues.
	type entry struct {
		name  string
		value string
	}

	var entries []entry
	for name, value := range values {
		if value == "" {
			continue
		}
		entries = append(entries, entry{name: name, value: value})
	}

	// Simple insertion sort by value length descending.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && len(entries[j].value) > len(entries[j-1].value); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}

	result := text
	for _, e := range entries {
		replacement := "[" + e.name + "]"
		// Replace the literal value
		result = strings.ReplaceAll(result, e.value, replacement)
		// Also replace base64-encoded form (catches secrets in encoded output)
		if b64 := base64.StdEncoding.EncodeToString([]byte(e.value)); b64 != e.value {
			result = strings.ReplaceAll(result, b64, replacement)
		}
		// Also replace URL-encoded form
		if urlEnc := url.QueryEscape(e.value); urlEnc != e.value {
			result = strings.ReplaceAll(result, urlEnc, replacement)
		}
	}

	return result
}

// MaskBytes scrubs secret values from a byte slice.
func (m *Masker) MaskBytes(data []byte) []byte {
	return []byte(m.Mask(string(data)))
}

// IsMasked checks if text appears to contain masked secret references.
func IsMasked(text string) bool {
	// Look for [ALL_CAPS_NAME] patterns that match secret naming convention.
	for i := 0; i < len(text)-2; i++ {
		if text[i] == '[' {
			end := strings.IndexByte(text[i+1:], ']')
			if end > 0 && end < 64 {
				inner := text[i+1 : i+1+end]
				if isSecretNameFormat(inner) {
					return true
				}
			}
		}
	}
	return false
}

func isSecretNameFormat(s string) bool {
	if len(s) < 2 {
		return false
	}
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}
