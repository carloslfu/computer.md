// SPDX-License-Identifier: Apache-2.0

package guardrails

import (
	"path/filepath"
	"testing"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	db, err := persistence.Open(filepath.Join(t.TempDir(), "guardrails.db"), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewEngine(db)
}

func TestAddRuleReplacesInMemoryPolicy(t *testing.T) {
	e := newTestEngine(t)
	base := Rule{
		ID:      "rule-1",
		Name:    "tmp writes",
		Pattern: "touch /tmp/replaced-rule",
		Action:  string(Block),
		Enabled: true,
	}
	if err := e.AddRule(base); err != nil {
		t.Fatal(err)
	}
	if got := e.Evaluate(Action{Type: "bash", Command: "touch /tmp/replaced-rule"}).Action; got != Block {
		t.Fatalf("initial rule action = %s, want block", got)
	}
	base.Action = string(Allow)
	if err := e.AddRule(base); err != nil {
		t.Fatal(err)
	}
	if got := e.Evaluate(Action{Type: "bash", Command: "touch /tmp/replaced-rule"}).Action; got != Allow {
		t.Fatalf("replacement rule should remove stale block, got %s", got)
	}
}

func TestAddRuleDisabledRemovesInMemoryPolicy(t *testing.T) {
	e := newTestEngine(t)
	rule := Rule{
		ID:      "rule-2",
		Name:    "tmp writes",
		Pattern: "touch /tmp/disabled-rule",
		Action:  string(Block),
		Enabled: true,
	}
	if err := e.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	rule.Enabled = false
	if err := e.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if got := e.Evaluate(Action{Type: "bash", Command: "touch /tmp/disabled-rule"}).Action; got != Allow {
		t.Fatalf("disabled replacement should remove stale policy, got %s", got)
	}
}
