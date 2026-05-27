// SPDX-License-Identifier: Apache-2.0

// computer-md is the reference CLI for the COMPUTER.md format.
//
// Subcommands:
//
//	init [--role <name>]   Generate a new COMPUTER.md
//	validate [<path>]      Parse a file and report structural issues
//	format [<path>]        Re-format a file canonically (round-trips through the parser)
//	sections [<path>]      List the ## sections present
//
// See SPEC.md in the spec/ directory for the format definition.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/carloslfu/computer.md/spec/parser"
)

//go:embed examples/*.md
var embeddedExamples embed.FS

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "init":
		os.Exit(runInit(args))
	case "validate":
		os.Exit(runValidate(args))
	case "format":
		os.Exit(runFormat(args))
	case "sections":
		os.Exit(runSections(args))
	case "examples":
		os.Exit(runExamples(args))
	case "help", "-h", "--help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "computer-md: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `computer-md — reference tooling for the COMPUTER.md format

Usage:
  computer-md init [--role <name>] [--out <path>]   Generate a new COMPUTER.md
  computer-md validate [<path>]                     Parse a file, report structure
  computer-md format [<path>]                       Re-format canonically (writes back)
  computer-md sections [<path>]                     List ## sections present
  computer-md examples                              List bundled role examples

If <path> is omitted, defaults to ./COMPUTER.md in the working directory.
Pass "-" to read from stdin.

Roles available via --role: see "computer-md examples".

Spec:  https://github.com/carloslfu/computer.md/blob/main/spec/SPEC.md
`)
}

// ----- init ---------------------------------------------------------

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	role := fs.String("role", "", "role-flavored starter (e.g. ceo-ops, sales-ops, developer)")
	out := fs.String("out", "COMPUTER.md", "output path")
	_ = fs.Parse(args)

	if _, err := os.Stat(*out); err == nil {
		fmt.Fprintf(os.Stderr, "computer-md: %s already exists; refusing to overwrite\n", *out)
		return 1
	}

	var content []byte
	if *role == "" {
		content = []byte(emptyDefault)
	} else {
		name := strings.TrimSuffix(*role, ".md")
		data, err := embeddedExamples.ReadFile("examples/" + name + ".md")
		if err != nil {
			fmt.Fprintf(os.Stderr, "computer-md: no role example named %q\n", *role)
			fmt.Fprintf(os.Stderr, "available roles:\n")
			listExamples(os.Stderr)
			return 1
		}
		content = data
	}

	if err := os.WriteFile(*out, content, 0o664); err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: write %s: %v\n", *out, err)
		return 1
	}
	fmt.Printf("Wrote %s (%d bytes).\n", *out, len(content))
	if *role == "" {
		fmt.Println("Start by adding your name + role to a ## Owner section.")
	} else {
		fmt.Printf("Started from the %q role template — edit as needed.\n", *role)
	}
	return 0
}

// ----- validate -----------------------------------------------------

func runValidate(args []string) int {
	content, src, err := readPathArg(args, "COMPUTER.md")
	if err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: %v\n", err)
		return 1
	}
	doc, err := parser.ParseString(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: parse %s: %v\n", src, err)
		return 1
	}

	fmt.Printf("Parsed %s: title=%q, %d sections\n", src, doc.Title, len(doc.Sections))
	canonical := map[string]bool{}
	for _, h := range parser.CanonicalSections {
		canonical[strings.ToLower(h)] = true
	}

	var custom []string
	var present []string
	for _, s := range doc.Sections {
		if canonical[strings.ToLower(s.Heading)] {
			present = append(present, s.Heading)
		} else {
			custom = append(custom, s.Heading)
		}
	}
	if len(present) > 0 {
		fmt.Println("Canonical sections present:")
		for _, h := range present {
			fmt.Printf("  - %s\n", h)
		}
	}
	if len(custom) > 0 {
		fmt.Println("Custom sections (valid, treated as context):")
		for _, h := range custom {
			fmt.Printf("  - %s\n", h)
		}
	}

	// Lint: detect duplicate headings.
	counts := map[string]int{}
	for _, s := range doc.Sections {
		counts[strings.ToLower(s.Heading)]++
	}
	dupes := []string{}
	for h, n := range counts {
		if n > 1 {
			dupes = append(dupes, fmt.Sprintf("%q (appears %d times)", h, n))
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		fmt.Println("Warning: duplicate section headings:")
		for _, d := range dupes {
			fmt.Printf("  - %s\n", d)
		}
	}
	return 0
}

// ----- format -------------------------------------------------------

func runFormat(args []string) int {
	content, src, err := readPathArg(args, "COMPUTER.md")
	if err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: %v\n", err)
		return 1
	}
	doc, err := parser.ParseString(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: parse %s: %v\n", src, err)
		return 1
	}
	out := doc.Format()
	if src == "<stdin>" {
		fmt.Print(out)
		return 0
	}
	if err := os.WriteFile(src, []byte(out), 0o664); err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: write %s: %v\n", src, err)
		return 1
	}
	fmt.Printf("Formatted %s (%d bytes).\n", src, len(out))
	return 0
}

// ----- sections -----------------------------------------------------

func runSections(args []string) int {
	content, src, err := readPathArg(args, "COMPUTER.md")
	if err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: %v\n", err)
		return 1
	}
	doc, err := parser.ParseString(content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computer-md: parse %s: %v\n", src, err)
		return 1
	}
	for _, s := range doc.Sections {
		fmt.Println(s.Heading)
	}
	return 0
}

// ----- examples -----------------------------------------------------

func runExamples(_ []string) int {
	listExamples(os.Stdout)
	return 0
}

func listExamples(w io.Writer) {
	entries, err := embeddedExamples.ReadDir("examples")
	if err != nil {
		fmt.Fprintf(w, "(no embedded examples)\n")
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		fmt.Fprintf(w, "  %s\n", name)
	}
}

// ----- helpers ------------------------------------------------------

func readPathArg(args []string, defaultPath string) (string, string, error) {
	path := defaultPath
	if len(args) > 0 {
		path = args[0]
	}
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), "<stdin>", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", path, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", abs, err)
	}
	return string(b), abs, nil
}

// emptyDefault mirrors the daemon's ComputerMDDefault for `init` with
// no --role. Keep in sync with daemon/core/computer_md.go.
const emptyDefault = `# COMPUTER.md

This file is shared between you (the customer) and the manager (the AI agent on this machine).

- The manager reads it at the start of every conversation; whatever is here is part of its context.
- The manager writes here when you express a durable preference ("always X", "from now on Y", "remember that Z about this machine").
- You can edit it directly. Ask the manager "show me COMPUTER.md" to see the current contents in chat.

Keep it short and human-readable. This is a note to yourself and the agent, not a database.

## Worker preference

Default: prefer Claude Code (` + "`claude`" + `) for development builds. If Claude Code is not installed or fails, fall back to Codex (` + "`codex`" + `). If neither is available, the manager builds inline as a last resort.

To override, replace the paragraph above with your preference. Examples:
- "Prefer Codex first, then Claude Code."
- "Always use Claude Code; never use Codex."
- "Use Codex for short tasks, Claude Code for anything multi-file."

## Preferences

(Empty — the manager will append entries here as you teach it things, or edit directly.)

## What the manager has learned about this machine

(Empty — the manager appends durable, machine-specific facts here over time.)
`
