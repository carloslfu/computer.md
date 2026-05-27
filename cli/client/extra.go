// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// Client methods for the daemon surfaces that close the dashboard-parity
// gap: credential responses, system info, guardrail rules, the audit
// log, AI usage, machine config, COMPUTER.md, conversation listing, and
// hosted apps. Each is a thin 1:1 wrapper over one daemon endpoint.

// CredentialValue is one name/value pair answering a credential request.
type CredentialValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// SubmitCredentials answers a task's request_credentials card. Pass
// cancelled=true (with no values) to dismiss the card instead.
func (c *Client) SubmitCredentials(taskID string, creds []CredentialValue, cancelled bool) error {
	body := map[string]any{}
	if cancelled {
		body["cancelled"] = true
	} else {
		body["credentials"] = creds
	}
	req, err := c.newRequest("POST", "/task/"+url.PathEscape(taskID)+"/credentials", body)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// GetJSON fetches a daemon endpoint and returns the decoded payload as
// a generic value. Used by the read-only inspection verbs (system,
// audit, usage, config) where the CLI passes the daemon's JSON through
// unchanged rather than mirroring every server struct.
func (c *Client) GetJSON(path string) (any, error) {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	var decoded any
	if len(out) > 0 {
		if err := json.Unmarshal(out, &decoded); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}
	}
	return decoded, nil
}

// Rule is a guardrail rule.
type Rule struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action"` // allow | confirm | block
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Priority    int    `json:"priority,omitempty"`
}

// ListRules returns the machine's guardrail rules.
func (c *Client) ListRules() ([]Rule, error) {
	req, err := c.newRequest("GET", "/rules", nil)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if err := c.do(req, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// AddRule creates a guardrail rule and returns it with its assigned id.
func (c *Client) AddRule(r Rule) (*Rule, error) {
	req, err := c.newRequest("POST", "/rules", r)
	if err != nil {
		return nil, err
	}
	var created Rule
	if err := c.do(req, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// DeleteRule removes a guardrail rule by id.
func (c *Client) DeleteRule(id string) error {
	req, err := c.newRequest("DELETE", "/rules/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// GetComputerMD returns the machine's COMPUTER.md content + path.
func (c *Client) GetComputerMD() (content, path string, err error) {
	req, reqErr := c.newRequest("GET", "/computer-md", nil)
	if reqErr != nil {
		return "", "", reqErr
	}
	var resp struct {
		Content string `json:"content"`
		Path    string `json:"path"`
	}
	if err := c.do(req, &resp); err != nil {
		return "", "", err
	}
	return resp.Content, resp.Path, nil
}

// SetComputerMD overwrites COMPUTER.md.
func (c *Client) SetComputerMD(content string) error {
	req, err := c.newRequest("PUT", "/computer-md", map[string]string{"content": content})
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// ListConversations returns the machine's conversations.
func (c *Client) ListConversations() (any, error) {
	return c.GetJSON("/conversations")
}

// ListSystems returns the systems the manager has authored on the
// machine — the scheduled / cron-driven workflows it built — each
// cross-referenced with its hosted-app URL when it has one.
func (c *Client) ListSystems() (any, error) {
	return c.GetJSON("/dashboard/systems")
}

// HostedApp is one deployed app / registered route.
type HostedApp struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	URL        string `json:"url"`
	SSOEnabled bool   `json:"sso_enabled"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// ListHostedApps returns the machine's deployed apps.
func (c *Client) ListHostedApps() ([]HostedApp, error) {
	req, err := c.newRequest("GET", "/hosted-apps", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Routes []HostedApp `json:"routes"`
	}
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return resp.Routes, nil
}

// SetHostedAppSSO toggles SSO protection on a deployed app.
func (c *Client) SetHostedAppSSO(name string, enabled bool) error {
	req, err := c.newRequest("PATCH", "/hosted-apps/"+url.PathEscape(name),
		map[string]bool{"sso_enabled": enabled})
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// RemoveHostedApp unregisters a deployed app's route.
func (c *Client) RemoveHostedApp(name string) error {
	req, err := c.newRequest("DELETE", "/hosted-apps/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// HostedAppRestartResult is returned when the daemon cycles a deployed
// app's host-side service.
type HostedAppRestartResult struct {
	OK             bool   `json:"ok"`
	Name           string `json:"name"`
	Unit           string `json:"unit"`
	Status         string `json:"status"`
	OldPID         int    `json:"old_pid"`
	NewPID         int    `json:"new_pid"`
	Port           int    `json:"port,omitempty"`
	ListeningAfter bool   `json:"listening_after"`
	Detail         string `json:"detail,omitempty"`
}

// RestartHostedApp restarts a deployed app's host-side service and
// returns proof that the process actually cycled.
func (c *Client) RestartHostedApp(name string) (*HostedAppRestartResult, error) {
	req, err := c.newRequest("POST", "/hosted-apps/"+url.PathEscape(name)+"/restart", nil)
	if err != nil {
		return nil, err
	}
	var resp HostedAppRestartResult
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
