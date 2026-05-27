// SPDX-License-Identifier: Apache-2.0

package parser

import (
	"strings"
	"testing"
)

func TestParseEmptyDefault(t *testing.T) {
	input := `# COMPUTER.md

This file is shared between you and the manager.

## Worker preference

Default: prefer Claude Code; fall back to Codex.

## Preferences

(Empty.)

## What the manager has learned about this machine

(Empty.)
`
	doc, err := ParseString(input)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Title != "COMPUTER.md" {
		t.Errorf("title = %q, want %q", doc.Title, "COMPUTER.md")
	}
	if !strings.Contains(doc.Preamble, "shared between you and the manager") {
		t.Errorf("preamble missing expected text, got: %q", doc.Preamble)
	}
	if len(doc.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(doc.Sections))
	}
	if doc.Sections[0].Heading != "Worker preference" {
		t.Errorf("sections[0].Heading = %q", doc.Sections[0].Heading)
	}
	if !strings.Contains(doc.Sections[0].Body, "Claude Code") {
		t.Errorf("sections[0].Body missing Claude Code, got: %q", doc.Sections[0].Body)
	}
}

func TestFindCaseInsensitive(t *testing.T) {
	doc, err := ParseString("## Worker Preference\n\nFoo\n")
	if err != nil {
		t.Fatal(err)
	}
	if s := doc.Find("worker preference"); s == nil || s.Body != "Foo" {
		t.Errorf("Find should be case-insensitive; got %+v", s)
	}
	if s := doc.Find("nonexistent"); s != nil {
		t.Errorf("Find for nonexistent should return nil; got %+v", s)
	}
}

func TestCustomSectionsPassThrough(t *testing.T) {
	input := `# Test

## Owner

Alice.

## Brand voice

Casual.

## Glossary

ARR = Annual Recurring Revenue.
`
	doc, err := ParseString(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 3 {
		t.Fatalf("want 3 sections, got %d", len(doc.Sections))
	}
	if doc.Find("Brand voice") == nil {
		t.Errorf("custom section 'Brand voice' should be present")
	}
	if doc.Find("Glossary") == nil {
		t.Errorf("custom section 'Glossary' should be present")
	}
}

func TestSubHeadingsPreserved(t *testing.T) {
	input := `## Tools

### CRM
- HubSpot

### Billing
- Stripe
`
	doc, err := ParseString(input)
	if err != nil {
		t.Fatal(err)
	}
	body := doc.Sections[0].Body
	if !strings.Contains(body, "### CRM") || !strings.Contains(body, "### Billing") {
		t.Errorf("sub-headings should be preserved in body, got: %q", body)
	}
}

func TestFormatRoundTrip(t *testing.T) {
	input := `# Test Computer

Preamble.

## Owner

Alice.

## Worker preference

Claude Code.
`
	doc, err := ParseString(input)
	if err != nil {
		t.Fatal(err)
	}
	out := doc.Format()
	doc2, err := ParseString(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if doc2.Title != doc.Title {
		t.Errorf("title mismatch after round-trip")
	}
	if len(doc2.Sections) != len(doc.Sections) {
		t.Errorf("section count mismatch after round-trip")
	}
	for i := range doc.Sections {
		if doc2.Sections[i].Heading != doc.Sections[i].Heading {
			t.Errorf("section %d heading mismatch: %q vs %q",
				i, doc2.Sections[i].Heading, doc.Sections[i].Heading)
		}
		if doc2.Sections[i].Body != doc.Sections[i].Body {
			t.Errorf("section %d body mismatch", i)
		}
	}
}
