// SPDX-License-Identifier: Apache-2.0

package guardrails

import (
	"fmt"
	"regexp"
	"strings"
)

// SensitiveMasker detects and masks PII and credential patterns in text.
type SensitiveMasker struct {
	patterns []maskPattern
}

type maskPattern struct {
	regex       *regexp.Regexp
	replacement string
	name        string
}

// NewSensitiveMasker creates a masker with default PII/credential patterns.
func NewSensitiveMasker() *SensitiveMasker {
	return &SensitiveMasker{
		patterns: defaultMaskPatterns(),
	}
}

// Mask replaces all detected PII and credential patterns in text with redacted markers.
func (m *SensitiveMasker) Mask(text string) string {
	result := text
	for _, p := range m.patterns {
		result = p.regex.ReplaceAllString(result, p.replacement)
	}
	return result
}

// ContainsSensitive returns true if the text contains any sensitive patterns.
func (m *SensitiveMasker) ContainsSensitive(text string) bool {
	for _, p := range m.patterns {
		if p.regex.MatchString(text) {
			return true
		}
	}
	return false
}

// DetectSensitive returns a list of detected sensitive data types in the text.
func (m *SensitiveMasker) DetectSensitive(text string) []string {
	var found []string
	seen := make(map[string]bool)
	for _, p := range m.patterns {
		if p.regex.MatchString(text) && !seen[p.name] {
			found = append(found, p.name)
			seen[p.name] = true
		}
	}
	return found
}

// MaskStructured masks sensitive fields in structured text (like JSON output)
// by looking for common key patterns.
func (m *SensitiveMasker) MaskStructured(text string) string {
	result := text

	// Mask common JSON key patterns with sensitive values.
	jsonKeyPatterns := []*regexp.Regexp{
		regexp.MustCompile(`("(?i:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth)")\s*:\s*"([^"]+)"`),
		regexp.MustCompile(`("(?i:ssn|social[_-]?security|credit[_-]?card|card[_-]?number)")\s*:\s*"([^"]+)"`),
	}

	for _, pattern := range jsonKeyPatterns {
		result = pattern.ReplaceAllString(result, `$1: "[REDACTED]"`)
	}

	// Also apply standard masking.
	result = m.Mask(result)

	return result
}

func defaultMaskPatterns() []maskPattern {
	return []maskPattern{
		// Credit card numbers (basic patterns).
		{
			regex:       regexp.MustCompile(`\b(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|3[47][0-9]{13}|6(?:011|5[0-9]{2})[0-9]{12})\b`),
			replacement: "[CREDIT_CARD_REDACTED]",
			name:        "credit_card",
		},
		// SSN (US Social Security Number).
		{
			regex:       regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
			replacement: "[SSN_REDACTED]",
			name:        "ssn",
		},
		// Email addresses (mask the local part).
		{
			regex:       regexp.MustCompile(`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`),
			replacement: "[EMAIL_REDACTED]",
			name:        "email",
		},
		// AWS access key IDs.
		{
			regex:       regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
			replacement: "[AWS_KEY_REDACTED]",
			name:        "aws_access_key",
		},
		// AWS secret keys (40-char base64).
		{
			regex:       regexp.MustCompile(`\b[0-9a-zA-Z/+]{40}\b`),
			replacement: "[AWS_SECRET_REDACTED]",
			name:        "aws_secret_key",
		},
		// Generic API keys (long hex or alphanumeric strings preceded by key-like words).
		{
			regex:       regexp.MustCompile(`(?i)(api[_-]?key|secret[_-]?key|access[_-]?token|auth[_-]?token)\s*[=:]\s*['"]?([a-zA-Z0-9\-_.]{32,})['"]?`),
			replacement: "${1}=[KEY_REDACTED]",
			name:        "api_key",
		},
		// Anthropic API keys.
		{
			regex:       regexp.MustCompile(`\bsk-ant-[a-zA-Z0-9\-_]{20,}\b`),
			replacement: "[ANTHROPIC_KEY_REDACTED]",
			name:        "anthropic_key",
		},
		// OpenAI API keys.
		{
			regex:       regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[a-zA-Z0-9_\-.]{20,}\b`),
			replacement: "[OPENAI_KEY_REDACTED]",
			name:        "openai_key",
		},
		// Stripe keys.
		{
			regex:       regexp.MustCompile(`\b(sk|pk)_(test|live)_[a-zA-Z0-9]{10,}\b`),
			replacement: "[STRIPE_KEY_REDACTED]",
			name:        "stripe_key",
		},
		// GitHub tokens.
		{
			regex:       regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[a-zA-Z0-9]{36,}\b`),
			replacement: "[GITHUB_TOKEN_REDACTED]",
			name:        "github_token",
		},
		// Private keys (PEM format).
		{
			regex:       regexp.MustCompile(`-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----[\s\S]*?-----END\s+(RSA\s+)?PRIVATE\s+KEY-----`),
			replacement: "[PRIVATE_KEY_REDACTED]",
			name:        "private_key",
		},
		// Phone numbers (US format).
		{
			regex:       regexp.MustCompile(`\b(?:\+1[-.\s]?)?\(?[0-9]{3}\)?[-.\s]?[0-9]{3}[-.\s]?[0-9]{4}\b`),
			replacement: "[PHONE_REDACTED]",
			name:        "phone",
		},
		// JWT tokens.
		{
			regex:       regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]{10,}\b`),
			replacement: "[JWT_REDACTED]",
			name:        "jwt",
		},
	}
}

// MaskValue masks a specific known secret value in text.
func MaskValue(text, secret, label string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, fmt.Sprintf("[%s]", label))
}
