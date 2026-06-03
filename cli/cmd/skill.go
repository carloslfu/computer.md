// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/carloslfu/computer.md/cli/output"
	"github.com/carloslfu/computer.md/cli/schema"
)

var flagSkillTarget string

var installSkillCmd = &cobra.Command{
	Use:   "install-skill",
	Short: "Install a coding-agent skill that teaches your agent this CLI",
	Long: `Drop a skill / tool manifest into your local Claude Code or Codex install
so the agent learns how to use the VibeCraft CLI on its next session.

Default target = autodetect (Claude Code is preferred when both are present).

  vibecraft install-skill                            # autodetect
  vibecraft install-skill --target=claude-code       # explicit
  vibecraft install-skill --target=codex             # explicit

Remove it later with 'vibecraft uninstall-skill'.

The skill body is a thin pointer at 'vibecraft docs' + https://www.vibecraft.so/llms.txt.
The full doc is NOT inlined into the skill file (single source of truth).`,
	RunE: func(cmd *cobra.Command, args []string) error { return runSkill(false) },
}

var uninstallSkillCmd = &cobra.Command{
	Use:   "uninstall-skill",
	Short: "Remove the coding-agent skill that install-skill installed",
	Long: `Remove the VibeCraft skill that 'vibecraft install-skill' wrote from your
local Claude Code or Codex install. Scoped: only deletes what we installed.

  vibecraft uninstall-skill                          # autodetect
  vibecraft uninstall-skill --target=claude-code     # explicit
  vibecraft uninstall-skill --target=codex           # explicit

(This removes the agent skill only. To remove the CLI itself, use
'vibecraft uninstall'.)`,
	RunE: func(cmd *cobra.Command, args []string) error { return runSkill(true) },
}

func init() {
	installSkillCmd.Flags().StringVar(&flagSkillTarget, "target", "", "claude-code | codex (default: autodetect)")
	uninstallSkillCmd.Flags().StringVar(&flagSkillTarget, "target", "", "claude-code | codex (default: autodetect)")
	rootCmd.AddCommand(installSkillCmd)
	rootCmd.AddCommand(uninstallSkillCmd)
}

// runSkill installs the skill (uninstall=false) or removes it (uninstall=true)
// for the resolved target. The two commands share everything but this flag.
func runSkill(uninstall bool) error {
	target := flagSkillTarget
	if target == "" {
		t, err := autodetectSkillTarget()
		if err != nil {
			return err
		}
		target = t
	}

	switch target {
	case "claude-code":
		return doClaudeCodeSkill(uninstall)
	case "codex":
		return doCodexSkill(uninstall)
	default:
		return schema.Newf(schema.CodeValidationError,
			"unknown --target %q", target).
			WithHint("supported: claude-code, codex")
	}
}

// autodetectSkillTarget picks the first installed target. Claude Code
// wins ties (E3 says it's our primary target). If neither is detected
// we still pick claude-code (creating the dir is cheap; the user will
// see it on next session start).
func autodetectSkillTarget() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", schema.Newf(schema.CodeInternal, "finding home directory: %s", err.Error())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		return "claude-code", nil
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); err == nil {
		return "codex", nil
	}
	// Default to claude-code; creating the dir is harmless.
	return "claude-code", nil
}

func doClaudeCodeSkill(uninstall bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "finding home directory: %s", err.Error())
	}
	skillDir := filepath.Join(home, ".claude", "skills", "vibecraft")
	skillFile := filepath.Join(skillDir, "SKILL.md")
	llmsFile := filepath.Join(skillDir, "llms.txt")

	if uninstall {
		if _, err := os.Stat(skillFile); os.IsNotExist(err) {
			return output.Emit(schema.InstallSkillData{
				Target: "claude-code",
				Path:   skillFile,
				Action: "noop",
			})
		}
		if err := os.RemoveAll(skillDir); err != nil {
			return schema.Newf(schema.CodeInternal, "removing skill: %s", err.Error())
		}
		return output.Emit(schema.InstallSkillData{
			Target: "claude-code",
			Path:   skillFile,
			Action: "uninstalled",
		})
	}

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return schema.Newf(schema.CodeInternal, "creating skill directory: %s", err.Error())
	}

	body := skillBody()
	if err := os.WriteFile(skillFile, []byte(body), 0644); err != nil {
		return schema.Newf(schema.CodeInternal, "writing skill: %s", err.Error())
	}
	if err := os.WriteFile(llmsFile, []byte(llmsTxt), 0644); err != nil {
		return schema.Newf(schema.CodeInternal, "writing skill reference: %s", err.Error())
	}

	return output.Emit(schema.InstallSkillData{
		Target: "claude-code",
		Path:   skillFile,
		Action: "installed",
	})
}

func doCodexSkill(uninstall bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return schema.Newf(schema.CodeInternal, "finding home directory: %s", err.Error())
	}
	// Codex reads the same open Agent Skills format as Claude Code:
	// `~/.codex/skills/<name>/SKILL.md` with name/description frontmatter,
	// discovered on startup. Same skill folder shape as the Claude Code path.
	skillDir := filepath.Join(home, ".codex", "skills", "vibecraft")
	skillFile := filepath.Join(skillDir, "SKILL.md")
	llmsFile := filepath.Join(skillDir, "llms.txt")

	if uninstall {
		if _, err := os.Stat(skillFile); os.IsNotExist(err) {
			return output.Emit(schema.InstallSkillData{
				Target: "codex",
				Path:   skillFile,
				Action: "noop",
			})
		}
		if err := os.RemoveAll(skillDir); err != nil {
			return schema.Newf(schema.CodeInternal, "removing skill: %s", err.Error())
		}
		return output.Emit(schema.InstallSkillData{
			Target: "codex",
			Path:   skillFile,
			Action: "uninstalled",
		})
	}

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return schema.Newf(schema.CodeInternal, "creating skill directory: %s", err.Error())
	}

	body := skillBody()
	if err := os.WriteFile(skillFile, []byte(body), 0644); err != nil {
		return schema.Newf(schema.CodeInternal, "writing skill: %s", err.Error())
	}
	if err := os.WriteFile(llmsFile, []byte(llmsTxt), 0644); err != nil {
		return schema.Newf(schema.CodeInternal, "writing skill reference: %s", err.Error())
	}

	return output.Emit(schema.InstallSkillData{
		Target: "codex",
		Path:   skillFile,
		Action: "installed",
	})
}

// skillBody is the one cross-agent Agent Skill body — the open SKILL.md format
// (name/description frontmatter + markdown), identical for Claude Code and Codex.
// It is a thin pointer at `vibecraft docs` / the llms.txt sidecar, never an
// inlined copy, so it cannot drift from the CLI's own reference.
func skillBody() string {
	return fmt.Sprintf(`---
name: vibecraft
description: Drive a VibeCraft agentic computer via its CLI. Run 'vibecraft docs' for the full reference, or read https://www.vibecraft.so/llms.txt.
---

# VibeCraft CLI

You have the %[1]svibecraft%[1]s binary on PATH. It is the agent-native interface to a
customer's VibeCraft computer. Every command returns a JSON envelope on
stdout (or JSON Lines for streams); errors go to stderr with stable %[1]scode%[1]s
strings; exit codes signal task outcome (0/1/2/3/4).

**Before doing anything else: read the reference once per session.**

%[2]s
vibecraft docs
%[2]s

The reference lives in %[1]sllms.txt%[1]s next to this skill, and at
%[1]shttps://www.vibecraft.so/llms.txt%[1]s — same content.

## Cheat sheet (most common moves)

%[2]s
# Auth — ONCE. One browser approval authorizes every machine on the
# account. Tell the human to approve the window that opens:
vibecraft auth login

# See every machine you can drive:
vibecraft machine list

# State check (a single machine, or all of them):
vibecraft status
vibecraft --machine all status

# Send a task and wait for the answer (add --machine <id> to pick one):
vibecraft task submit "do the thing"

# Background submit + later wait:
id=$(vibecraft task submit "long thing" --no-wait | jq -r .data.id)
vibecraft task wait "$id"

# Multi-turn (task ends in waiting_for_input → exit 3):
out=$(vibecraft task submit "ship the deploy")
if [ $? -eq 3 ]; then
  id=$(echo "$out" | jq -r .data.id)
  vibecraft task respond "$id" "yes, ship it"
fi

# Stream events:
vibecraft task stream "$id"   # JSON Lines

# Read the transcript:
vibecraft task messages "$id" | jq .data.messages

# Headless (CI / no browser): set VIBECRAFT_ACCOUNT_KEY, or a single
# machine's VIBECRAFT_MACHINE_URL + VIBECRAFT_API_KEY.
%[2]s

## Exit code dispatch (memorize this)

%[2]s
0  ok / task completed
1  CLI error (auth, network, validation, daemon 5xx) — see stderr envelope
2  task FAILED — TaskData on stdout
3  task NEEDS INPUT — call 'vibecraft task respond <id>'
4  task CANCELLED
%[2]s

## Asking for help

If you hit %[1]sauth_required%[1]s / %[1]sauth_invalid%[1]s / %[1]sauth_forbidden%[1]s / repeated %[1]sserver_error%[1]s,
surface to the human:

> "I tried %[1]svibecraft <cmd>%[1]s on %[1]s<machine>%[1]s and got %[1]s<error_code>%[1]s. Suggested next step:
> %[1]s<hint>%[1]s. Should I %[1]s<hint>%[1]s or do you want to handle it?"
`, "`", "```")
}
