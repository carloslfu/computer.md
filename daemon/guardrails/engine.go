// SPDX-License-Identifier: Apache-2.0

package guardrails

import (
	"fmt"
	"log"
	"regexp"
	"sync"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

// Decision represents what the guardrail engine decided about an action.
type Decision struct {
	Action ActionType `json:"action"`
	Reason string     `json:"reason"`
	Rule   string     `json:"rule"`
}

// ActionType is what to do with a guarded action.
type ActionType string

const (
	Allow   ActionType = "allow"
	Confirm ActionType = "confirm"
	Block   ActionType = "block"
)

// Action describes an operation the agent wants to perform.
type Action struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	TaskID  string `json:"task_id"`
}

// Rule is a single guardrail policy rule.
type Rule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action"` // "allow", "confirm", "block"
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Priority    int    `json:"priority"`
}

// Engine evaluates actions against guardrail policies.
type Engine struct {
	mu       sync.RWMutex
	policies []Policy
	db       *persistence.DB
}

// NewEngine creates a guardrail engine with default policies and loads
// custom rules from the database.
func NewEngine(db *persistence.DB) *Engine {
	e := &Engine{
		db: db,
	}

	// Register default policies.
	e.policies = DefaultPolicies()

	// Load custom rules from DB.
	customRules, err := e.loadCustomRules()
	if err != nil {
		log.Printf("warning: failed to load custom guardrail rules: %v", err)
	} else {
		for _, rule := range customRules {
			e.policies = append(e.policies, &CustomRulePolicy{rule: rule})
		}
	}

	return e
}

// Evaluate checks an action against all policies. The most restrictive
// decision wins (block > confirm > allow).
func (e *Engine) Evaluate(action Action) Decision {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := Decision{
		Action: Allow,
		Reason: "no policy matched",
	}

	for _, policy := range e.policies {
		d := policy.Evaluate(action)
		if d.Action == Block {
			return d
		}
		if d.Action == Confirm && result.Action != Block {
			result = d
		}
	}

	return result
}

// AddRule adds a custom rule and persists it to the database.
func (e *Engine) AddRule(rule Rule) error {
	// Validate that the pattern is a valid regex before persisting.
	if rule.Pattern != "" {
		if len(rule.Pattern) > 500 {
			return fmt.Errorf("regex pattern too long (max 500 chars)")
		}
		if _, err := regexp.Compile(rule.Pattern); err != nil {
			return fmt.Errorf("invalid regex pattern: %w", err)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	_, err := e.db.Conn().Exec(
		`INSERT OR REPLACE INTO guardrail_rules (id, name, pattern, action, description, enabled, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rule.ID, rule.Name, rule.Pattern, rule.Action, rule.Description,
		boolToInt(rule.Enabled), rule.Priority,
	)
	if err != nil {
		return err
	}

	// Add to in-memory policies.
	e.policies = append(e.policies, &CustomRulePolicy{rule: rule})
	return nil
}

// RemoveRule removes a custom rule by ID.
func (e *Engine) RemoveRule(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	_, err := e.db.Conn().Exec(`DELETE FROM guardrail_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}

	// Remove from in-memory policies.
	var filtered []Policy
	for _, p := range e.policies {
		if crp, ok := p.(*CustomRulePolicy); ok {
			if crp.rule.ID == id {
				continue
			}
		}
		filtered = append(filtered, p)
	}
	e.policies = filtered
	return nil
}

// ListRules returns all custom rules from the database.
func (e *Engine) ListRules() ([]Rule, error) {
	return e.loadCustomRules()
}

// ActiveRuleDescriptions returns human-readable descriptions of all active policies.
func (e *Engine) ActiveRuleDescriptions() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var descs []string
	for _, p := range e.policies {
		descs = append(descs, p.Description())
	}
	return descs
}

func (e *Engine) loadCustomRules() ([]Rule, error) {
	rows, err := e.db.Conn().Query(
		`SELECT id, name, pattern, action, description, enabled, priority
		 FROM guardrail_rules WHERE enabled = 1 ORDER BY priority DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var r Rule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.Pattern, &r.Action, &r.Description, &enabled, &r.Priority); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
