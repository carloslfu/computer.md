// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/cli/output"
)

func TestInstallSkillWritesLocalLlmsReference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldTarget := flagSkillTarget
	flagSkillTarget = "claude-code"
	t.Cleanup(func() { flagSkillTarget = oldTarget })
	withOutputMode(t, output.ModeJSON)

	if _, err := captureStdout(t, func() error { return runSkill(false) }); err != nil {
		t.Fatalf("runSkill install: %v", err)
	}

	llmsPath := filepath.Join(home, ".claude", "skills", "vibecraft", "llms.txt")
	body, err := os.ReadFile(llmsPath)
	if err != nil {
		t.Fatalf("llms.txt missing: %v", err)
	}
	if !strings.Contains(string(body), "VibeCraft CLI") {
		t.Fatalf("llms.txt content looks wrong")
	}
}

func TestInstallSkillCodexUsesAgentSkillsFormat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldTarget := flagSkillTarget
	flagSkillTarget = "codex"
	t.Cleanup(func() { flagSkillTarget = oldTarget })
	withOutputMode(t, output.ModeJSON)

	if _, err := captureStdout(t, func() error { return runSkill(false) }); err != nil {
		t.Fatalf("runSkill install: %v", err)
	}

	// Codex reads the open Agent Skills format — a `skills/vibecraft/SKILL.md`
	// folder with frontmatter — NOT a plain `instructions/*.md` file.
	skillPath := filepath.Join(home, ".codex", "skills", "vibecraft", "SKILL.md")
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("codex SKILL.md missing: %v", err)
	}
	if !strings.HasPrefix(string(body), "---\n") {
		t.Fatalf("codex skill must carry YAML frontmatter")
	}
	if !strings.Contains(string(body), "name: vibecraft") {
		t.Fatalf("codex skill frontmatter missing name")
	}
	llmsPath := filepath.Join(home, ".codex", "skills", "vibecraft", "llms.txt")
	if _, err := os.Stat(llmsPath); err != nil {
		t.Fatalf("codex llms.txt sidecar missing: %v", err)
	}

	// And the legacy dead path must NOT be written.
	legacy := filepath.Join(home, ".codex", "instructions", "vibecraft.md")
	if _, err := os.Stat(legacy); err == nil {
		t.Fatalf("legacy ~/.codex/instructions path should no longer be written")
	}
}
