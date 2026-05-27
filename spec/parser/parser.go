// SPDX-License-Identifier: Apache-2.0

// Package parser extracts ## sections from a COMPUTER.md file.
//
// The COMPUTER.md format is plain markdown. The "parser" here is a
// section extractor that lifts ## heading blocks into a map keyed by
// the heading text. It is intentionally minimal: it is not a
// typechecker, it does not enforce section presence, and it does not
// validate the body of any section. Unknown sections pass through.
//
// See SPEC.md in the spec/ directory for the canonical vocabulary
// and reading rules.
package parser

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Document is a parsed COMPUTER.md file.
type Document struct {
	// Title is the H1 (# ...) text, or empty if no H1 was present.
	Title string

	// Preamble is the text between the H1 and the first ## section,
	// trimmed of leading and trailing whitespace.
	Preamble string

	// Sections are the ## blocks in document order. Each Section has
	// the heading text (without the "## " prefix) and its body.
	Sections []Section
}

// Section is one ## block from a COMPUTER.md file.
type Section struct {
	// Heading is the section heading without the "## " prefix.
	// E.g. "Worker preference", "Standing rules".
	Heading string

	// Body is the section content, with leading and trailing
	// whitespace trimmed. Sub-headings (### and deeper) are preserved
	// verbatim inside Body.
	Body string
}

// Find returns the first section with a matching heading (case
// insensitive), or nil if none.
func (d *Document) Find(heading string) *Section {
	for i := range d.Sections {
		if strings.EqualFold(d.Sections[i].Heading, heading) {
			return &d.Sections[i]
		}
	}
	return nil
}

// CanonicalSections is the recognized vocabulary the spec ships at
// v0.1. A reader should recognize these with the semantics described
// in SPEC.md. Other ## headings are valid and treated as ambient
// context.
var CanonicalSections = []string{
	"Owner",
	"Business",
	"Tools",
	"Standing rules",
	"Routines",
	"Out of scope",
	"Worker preference",
	"Preferences",
	"What the manager has learned about this machine",
}

// Parse reads a COMPUTER.md file from r and returns its structured form.
func Parse(r io.Reader) (*Document, error) {
	doc := &Document{}
	scanner := bufio.NewScanner(r)
	// Allow long lines (some sections paste large blocks).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var (
		preamble       strings.Builder
		currentHeading string
		currentBody    strings.Builder
		seenH1         bool
		inSection      bool
	)

	flushSection := func() {
		if !inSection {
			return
		}
		doc.Sections = append(doc.Sections, Section{
			Heading: currentHeading,
			Body:    strings.TrimSpace(currentBody.String()),
		})
		currentHeading = ""
		currentBody.Reset()
		inSection = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case !seenH1 && strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## "):
			doc.Title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			seenH1 = true

		case strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### "):
			flushSection()
			currentHeading = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			inSection = true

		case inSection:
			currentBody.WriteString(line)
			currentBody.WriteByte('\n')

		default:
			preamble.WriteString(line)
			preamble.WriteByte('\n')
		}
	}
	flushSection()

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan COMPUTER.md: %w", err)
	}

	doc.Preamble = strings.TrimSpace(preamble.String())
	return doc, nil
}

// ParseString is a convenience wrapper around Parse.
func ParseString(s string) (*Document, error) {
	return Parse(strings.NewReader(s))
}

// Format renders a Document back to canonical markdown. Round-trip
// is lossy in whitespace (Format normalizes blank-line spacing) but
// preserves all content.
func (d *Document) Format() string {
	var b strings.Builder
	if d.Title != "" {
		fmt.Fprintf(&b, "# %s\n", d.Title)
	}
	if d.Preamble != "" {
		fmt.Fprintf(&b, "\n%s\n", d.Preamble)
	}
	for _, s := range d.Sections {
		fmt.Fprintf(&b, "\n## %s\n", s.Heading)
		if s.Body != "" {
			fmt.Fprintf(&b, "\n%s\n", s.Body)
		}
	}
	return b.String()
}
