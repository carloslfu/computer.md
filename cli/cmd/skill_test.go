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
