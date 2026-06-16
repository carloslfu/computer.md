// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOneLine_CollapsesWhitespace(t *testing.T) {
	got := oneLine("  hello\t\nworld  ", 80)
	if got != "hello world" {
		t.Errorf("oneLine collapse: got %q, want %q", got, "hello world")
	}
}

func TestOneLine_NoTruncationUnderBudget(t *testing.T) {
	got := oneLine("short", 80)
	if got != "short" {
		t.Errorf("oneLine should not truncate under budget: got %q", got)
	}
}

func TestOneLine_TruncatesWithEllipsis(t *testing.T) {
	got := oneLine("abcdefghij", 8)
	if got != "abcde..." {
		t.Errorf("oneLine truncate: got %q, want %q", got, "abcde...")
	}
	if len([]rune(got)) != 8 {
		t.Errorf("oneLine truncate rune length: got %d, want 8", len([]rune(got)))
	}
}

// The truncation budget is a rune budget, and truncation must never split a
// multibyte UTF-8 sequence (which would emit U+FFFD in the rendered table).
func TestOneLine_TruncationIsRuneAware(t *testing.T) {
	// Each "é" is two bytes; a byte-offset slice at max-3 could land mid-rune.
	got := oneLine("ééééééééé", 6)
	if !utf8.ValidString(got) {
		t.Errorf("oneLine produced invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("oneLine produced a replacement character: %q", got)
	}
	if got != "ééé..." {
		t.Errorf("oneLine rune-aware truncate: got %q, want %q", got, "ééé...")
	}
}

// With a tiny budget (<= 3) there is no room for an ellipsis; the result is a
// hard rune-boundary cut, still valid UTF-8.
func TestOneLine_TinyBudgetRuneAware(t *testing.T) {
	got := oneLine("日本語テスト", 2)
	if !utf8.ValidString(got) {
		t.Errorf("oneLine produced invalid UTF-8 at tiny budget: %q", got)
	}
	if got != "日本" {
		t.Errorf("oneLine tiny budget: got %q, want %q", got, "日本")
	}
}
