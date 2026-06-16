// SPDX-License-Identifier: Apache-2.0

package guardrails

import "testing"

// TestResolveRuleAction covers the action-enum validation that backs both
// the load-time CustomRulePolicy fail-closed and the /rules write-boundary
// reject (daemon-vault-guard-2).
func TestResolveRuleAction(t *testing.T) {
	cases := []struct {
		in   string
		want ActionType
		ok   bool
	}{
		// Canonical values resolve.
		{"allow", Allow, true},
		{"confirm", Confirm, true},
		{"block", Block, true},
		// Case-insensitive + whitespace-tolerant.
		{"Block", Block, true},
		{"BLOCK", Block, true},
		{"  confirm  ", Confirm, true},
		{"Allow", Allow, true},
		// Unknown / typo / empty all fail (caller must fail closed).
		{"deny", "", false},
		{"confirmm", "", false},
		{"warn", "", false},
		{"", "", false},
		{"blockk", "", false},
	}
	for _, c := range cases {
		got, ok := ResolveRuleAction(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ResolveRuleAction(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestCustomRulePolicy_UnknownActionFailsClosed proves the daemon-vault-guard-2
// regression is fixed: a custom rule whose action is miscased or a typo used to
// be wrapped verbatim as ActionType("Block") / ActionType("deny"), which the
// engine treated as non-restrictive and silently degraded to Allow. The rule
// authored to GUARD the action instead permitted it. Now an unrecognized action
// fails closed to Block.
func TestCustomRulePolicy_UnknownActionFailsClosed(t *testing.T) {
	matchAction := Action{Type: "bash", Command: "do-the-guarded-thing"}

	cases := []struct {
		name       string
		ruleAction string
		want       ActionType
	}{
		// The defect cases: these previously fell through to Allow.
		{"miscased Block", "Block", Block},
		{"uppercase BLOCK", "BLOCK", Block},
		{"typo deny", "deny", Block},
		{"typo confirmm", "confirmm", Block},
		{"garbage", "nonsense", Block},
		// Canonical values are honored exactly (and case-normalized).
		{"canonical block", "block", Block},
		{"canonical confirm", "confirm", Confirm},
		{"miscased Confirm honored", "Confirm", Confirm},
		{"canonical allow", "allow", Allow},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &CustomRulePolicy{rule: Rule{
				Name:    "guard-rule",
				Pattern: "do-the-guarded-thing",
				Action:  c.ruleAction,
				Enabled: true,
			}}
			got := p.Evaluate(matchAction).Action
			if got != c.want {
				t.Fatalf("rule action %q: got %s, want %s", c.ruleAction, got, c.want)
			}
		})
	}
}

// TestCustomRulePolicy_UnknownActionNonMatchStillAllows confirms the fail-closed
// only applies when the rule actually matches the action. A rule that does not
// match must still Allow regardless of how broken its action string is — the
// validation is for matched rules, not a blanket block.
func TestCustomRulePolicy_UnknownActionNonMatchStillAllows(t *testing.T) {
	p := &CustomRulePolicy{rule: Rule{
		Name:    "guard-rule",
		Pattern: "never-matches-this",
		Action:  "Block", // miscased, but the pattern won't match
		Enabled: true,
	}}
	got := p.Evaluate(Action{Type: "bash", Command: "ls /tmp"}).Action
	if got != Allow {
		t.Fatalf("non-matching rule with bad action: got %s, want %s", got, Allow)
	}
}

// TestEngine_UnknownActionRuleBlocks is the end-to-end view through the engine:
// a persisted rule with a bad action must make Evaluate return Block, not Allow.
func TestEngine_UnknownActionRuleBlocks(t *testing.T) {
	e := &Engine{policies: []Policy{
		&CustomRulePolicy{rule: Rule{
			Name:    "typo-rule",
			Pattern: "touch /tmp/guarded",
			Action:  "Block", // miscased
			Enabled: true,
		}},
	}}
	got := e.Evaluate(Action{Type: "bash", Command: "touch /tmp/guarded"}).Action
	if got != Block {
		t.Fatalf("engine with miscased-action rule: got %s, want %s (must not fail open to Allow)", got, Block)
	}
}

// TestFileSystemPolicy_CredentialFilesBlock proves daemon-vault-guard-4 is
// fixed: reads of credential-bearing files (password hashes, SSH private keys)
// now hard-Block instead of Confirm. A Confirm was eligible for the AI
// auto-review pass (whose prompt auto-approves "read-only filesystem
// inspection") and the resulting content was returned masked only against vault
// secrets — so a private key / shadow read could be auto-approved AND leaked.
// Block short-circuits before the classifier and before execution.
func TestFileSystemPolicy_CredentialFilesBlock(t *testing.T) {
	p := &FileSystemPolicy{}
	cases := []policyCase{
		// Block: credential material — never auto-approvable, never read.
		{"cat /etc/shadow", "cat /etc/shadow", "", Block},
		{"less /etc/shadow", "less /etc/shadow", "", Block},
		{"cat /etc/gshadow", "cat /etc/gshadow", "", Block},
		{"cat id_rsa absolute", "cat /home/vibecraft/.ssh/id_rsa", "", Block},
		{"cat id_rsa tilde", "cat ~/.ssh/id_rsa", "", Block},
		{"cat id_ed25519", "cat /home/vibecraft/.ssh/id_ed25519", "", Block},
		{"cat id_ecdsa tilde", "cat ~/.ssh/id_ecdsa", "", Block},
		{"editor view shadow", `{"command":"view","path":"/etc/shadow"}`, "str_replace_based_edit_tool", Block},
		{"editor view id_rsa", `{"command":"view","path":"/home/vibecraft/.ssh/id_rsa"}`, "str_replace_based_edit_tool", Block},

		// Confirm: sensitive but non-secret config — human card as designed.
		{"cat /etc/passwd", "cat /etc/passwd", "", Confirm},
		{"vim sshd_config", "vim /etc/ssh/sshd_config", "", Confirm},
		{"cat authorized_keys", "cat /home/vibecraft/.ssh/authorized_keys", "", Confirm},
		{"cat sudoers", "cat /etc/sudoers", "", Confirm},

		// Allow: benign.
		{"ls /tmp", "ls /tmp", "", Allow},
		{"cat notes", "cat /home/vibecraft/notes.txt", "", Allow},
	}
	runPolicy(t, p, cases)
}
