// SPDX-License-Identifier: Apache-2.0

package guardrails

import (
	"strings"
	"testing"
)

func TestSensitiveMasker_OpenAIKeyShapes(t *testing.T) {
	m := NewSensitiveMasker()
	cases := []string{
		"sk-abcdefghijklmnopqrstuvwxyz123456",
		"sk-proj-abcdefghijklmnopqrstuvwxyz1234567890",
		"sk-svcacct-abcdefghijklmnopqrstuvwxyz1234567890",
		"sk-admin-abcdefghijklmnopqrstuvwxyz1234567890",
	}
	for _, key := range cases {
		out := m.Mask("key=" + key)
		if strings.Contains(out, key) {
			t.Fatalf("OpenAI key shape was not redacted: %s -> %s", key, out)
		}
		if !strings.Contains(out, "[OPENAI_KEY_REDACTED]") {
			t.Fatalf("OpenAI key replacement missing for %s: %s", key, out)
		}
	}
}
