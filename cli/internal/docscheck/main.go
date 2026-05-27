// SPDX-License-Identifier: Apache-2.0

// docscheck is a CI guard (Track F12). Parses every cobra.Command Use
// string in cli/cmd/*.go and asserts the bare verb appears in llms.txt.
// Catches the "added a verb, forgot the doc" failure mode.
//
// Best-effort heuristic, not a full Go AST analysis. It looks for
// patterns like:
//
//   Use:   "submit [message]",
//   Use:   "task",
//
// and asserts each verb name appears in the embedded llms_embed.txt.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var useRegex = regexp.MustCompile(`^\s*Use:\s*"([^"]+)"`)

func main() {
	cmdDir, err := filepath.Abs("cmd")
	if err != nil {
		die("resolving cmd dir: %v", err)
	}
	docPath := filepath.Join(cmdDir, "llms_embed.txt")
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		die("reading %s: %v", docPath, err)
	}
	doc := string(docBytes)

	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		die("listing %s: %v", cmdDir, err)
	}

	verbs := map[string]string{} // verb -> file it appeared in
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(cmdDir, name)
		f, err := os.Open(path)
		if err != nil {
			die("opening %s: %v", path, err)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			m := useRegex.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}
			// "submit [message]" → "submit"
			verb := strings.Fields(m[1])[0]
			verb = strings.TrimSpace(verb)
			if verb == "" {
				continue
			}
			// Skip cobra-generated subcommands we don't care about, the
			// root command itself, and anything aliased + hidden.
			if verb == "vibecraft" || verb == "help" || verb == "completion" {
				continue
			}
			// Skip subcommand verbs that are documented as "<parent>
			// <subverb>" in llms.txt rather than as their own table row
			// (set/get/list/delete are generic across vault, memory,
			// task, machine; the parent verb's row covers them).
			subverbs := map[string]bool{
				"set": true, "get": true, "list": true, "delete": true,
				"select": true, "remove": true, "cancel": true, "wait": true,
				"submit": true, "respond": true, "messages": true, "stream": true,
				"login": true, "logout": true, "whoami": true, "print": true,
			}
			if subverbs[verb] {
				continue
			}
			// Skip operator-only local tools that aren't part of the
			// agent-facing surface (H1).
			if verb == "backup" || verb == "restore" {
				continue
			}
			verbs[verb] = name
		}
		_ = f.Close()
	}

	missing := []string{}
	for verb, src := range verbs {
		if !strings.Contains(doc, verb) {
			missing = append(missing, fmt.Sprintf("  %s (from %s)", verb, src))
		}
	}

	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "docs-check: %d verb(s) missing from llms.txt:\n", len(missing))
		for _, m := range missing {
			fmt.Fprintln(os.Stderr, m)
		}
		fmt.Fprintf(os.Stderr, "\nAdd the verb to llms.txt before merging.\n")
		os.Exit(1)
	}

	fmt.Printf("docs-check: %d verbs all present in llms.txt\n", len(verbs))
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "docs-check: "+format+"\n", args...)
	os.Exit(2)
}
