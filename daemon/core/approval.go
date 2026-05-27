// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/guardrails"
)

// Severity tells the dashboard how to frame an approval card. "escalate"
// is the normal neutral framing — the system isn't sure, the human makes
// the call. "soft_deny" is the red-framing path — the system recommends
// against, the human can override with friction proportional to consequence.
type Severity string

const (
	SeverityEscalate Severity = "escalate"  // routine approval, neutral framing
	SeveritySoftDeny Severity = "soft_deny" // red framing, friction override
)

// Friction is how much resistance the dashboard puts between the customer
// and the override button on a Soft Deny card. None = no extra gate (used
// for routine escalations). Delay = the override button is disabled for
// a short countdown so accidental clicks don't slip through. TypeToConfirm
// = the customer must type the target string (a path, a device) before
// the override activates — same pattern as GitHub's repo deletion.
type Friction string

const (
	FrictionNone          Friction = "none"
	FrictionDelay         Friction = "delay"
	FrictionTypeToConfirm Friction = "type_to_confirm"
)

// ApprovalPayload carries everything the UI needs to render a polished
// approval card. Stored as JSON in the approval message's content and on
// task.result, and broadcast in the task:waiting SSE event under the
// "approval" field. The daemon also emits a plain-text "question" field
// in the same event for backward compatibility with older clients.
//
// Backward compatibility: Severity and Friction are optional with
// defaults (escalate / none). Older payloads without these fields render
// as a normal Escalate card on both new and old clients.
type ApprovalPayload struct {
	Tool       string   `json:"tool"`                  // "bash" | "text_editor" | "computer"
	Command    string   `json:"command"`               // exact command or input string shown to the user
	Title      string   `json:"title"`                 // plain-English intent, e.g. "Remove package: nginx"
	Reason     string   `json:"reason"`                // plain-English why approval is needed (classifier-authored when present)
	Severity   Severity `json:"severity,omitempty"`    // escalate (default) | soft_deny
	Friction   Friction `json:"friction,omitempty"`    // none (default) | delay | type_to_confirm
	TypeTarget string   `json:"type_target,omitempty"` // when Friction==type_to_confirm: the exact string the customer must type
	// MessageID is the persisted approval bubble's id, embedded so SSE
	// replay and rehydration can wire up the card after a reconnect or
	// daemon restart without a side-channel lookup. Mirrors the
	// CredentialRequestPayload.MessageID convention.
	MessageID string `json:"message_id,omitempty"`
	// Resolved is "approved" | "denied" once the user has answered. The
	// dashboard reads this to render the receipt state instead of the
	// live form. Empty while the card is still actionable.
	Resolved string `json:"resolved,omitempty"`
	// ExpiredAt + ExpiredReason are set when the card is invalidated
	// without a user response — 24h timeout, task ended, or the user
	// redirected attention to another conversation. The dashboard
	// renders the form as expired (disabled, with the reason) so a
	// returning client doesn't see a stale-but-actionable button.
	ExpiredAt     string `json:"expired_at,omitempty"`
	ExpiredReason string `json:"expired_reason,omitempty"`
}

// PlainText is a last-resort fallback rendering for clients that cannot
// parse the structured payload. Also used for audit-log detail strings
// so historical log readers see the same wording the user saw.
func (a ApprovalPayload) PlainText() string {
	return fmt.Sprintf("%s\n\n%s\n\nCommand: %s", a.Title, a.Reason, a.Command)
}

// JSON returns the payload serialised for persistence / SSE transport.
func (a ApprovalPayload) JSON() string {
	b, err := json.Marshal(a)
	if err != nil {
		return a.PlainText()
	}
	return string(b)
}

// DecodeApprovalJSON parses a stored approval payload back into its
// structured form. Returns false when the input is empty or not a
// valid approval JSON object — callers should treat that case as a
// plain-text legacy prompt.
func DecodeApprovalJSON(s string) (ApprovalPayload, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return ApprovalPayload{}, false
	}
	var a ApprovalPayload
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return ApprovalPayload{}, false
	}
	if a.Command == "" && a.Title == "" {
		return ApprovalPayload{}, false
	}
	return a, true
}

// IsActionable reports whether the approval card can still be answered
// by the user — i.e. neither already resolved nor expired. Used by the
// dashboard and the inbox replay to decide whether to render the live
// form or a receipt.
func (a ApprovalPayload) IsActionable() bool {
	return a.Resolved == "" && a.ExpiredAt == ""
}

// buildApproval turns a guardrail Confirm decision into an ApprovalPayload
// with a plain-English title inferred from the rule and command. This is
// the routine-escalation builder; for classifier-recommended denies, use
// buildSoftDenyApproval which sets red framing and friction.
func buildApproval(tc managerclient.ToolCall, d guardrails.Decision) ApprovalPayload {
	cmd := tc.InputString()
	return ApprovalPayload{
		Tool:     tc.Name,
		Command:  cmd,
		Title:    humanTitle(tc.Name, cmd, d.Rule),
		Reason:   d.Reason,
		Severity: SeverityEscalate,
		Friction: FrictionNone,
	}
}

// buildSoftDenyApproval is the red-framing variant. classifierReason
// (non-empty) becomes the card's customer-facing "why this is dangerous"
// text. Friction tier is picked from the command shape: type-to-confirm
// for operations on a real block device or system-tree path, delay
// (3-second wait) for everything else.
func buildSoftDenyApproval(tc managerclient.ToolCall, d guardrails.Decision, classifierReason string) ApprovalPayload {
	cmd := tc.InputString()
	reason := classifierReason
	if reason == "" {
		reason = d.Reason
	}
	friction, target := pickFriction(cmd)
	return ApprovalPayload{
		Tool:       tc.Name,
		Command:    cmd,
		Title:      humanTitle(tc.Name, cmd, d.Rule),
		Reason:     reason,
		Severity:   SeveritySoftDeny,
		Friction:   friction,
		TypeTarget: target,
	}
}

// pickFriction inspects a Soft-Deny-tier command and returns the friction
// the override button should require. Type-to-confirm is reserved for the
// cases where typo-equivalent slips could cost the customer the machine
// or its data: writing to a block device, or rm -rf on a system tree.
// Delay is used for everything else — destructive but bounded, where a
// 3-second pause is enough to undo a mis-click.
func pickFriction(cmd string) (Friction, string) {
	// Tier 2: a real block device target. dd of=/dev/sda, mkfs /dev/sdb,
	// fdisk /dev/nvme0n1, etc. Customer must type the device path.
	if m := reBlockDeviceTarget.FindString(cmd); m != "" {
		return FrictionTypeToConfirm, m
	}
	// Tier 2: rm -rf on a system tree. The actual target is the path
	// immediately after the flags, captured by the regex group.
	if m := reSystemTreeRm.FindStringSubmatch(cmd); len(m) >= 2 && m[1] != "" {
		return FrictionTypeToConfirm, m[1]
	}
	return FrictionDelay, ""
}

var (
	// /dev/sda, /dev/sdb1, /dev/nvme0n1, /dev/nvme0n1p2, /dev/xvda,
	// /dev/vda, /dev/mapper/<name>, /dev/loop<N>. Captures the full path
	// so the customer can see and re-type exactly which device is being
	// touched.
	reBlockDeviceTarget = regexp.MustCompile(`/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*|mapper/[A-Za-z0-9_-]+|loop[0-9]+)\b`)
	// rm with at least one -[rR][fF] flag set, followed by a system-tree
	// root path (/etc, /var, /usr, /boot, /lib, /sbin, /bin, /sys, /proc,
	// /opt, /srv, /root). Captures the path itself so the user re-types
	// it. Customer-workspace deletions (/home/vibecraft/...) and /tmp
	// deletions never hit this — they're already in the safer category.
	reSystemTreeRm = regexp.MustCompile(`(?i)\brm\s+(?:-[rRfFv]+\s+)+(/(?:etc|var|usr|boot|lib|sbin|bin|sys|proc|opt|srv|root)[^\s;|&]*)`)
)

// humanTitle produces a plain-English title for the approval card,
// keyed off the policy rule that fired. Falls back to generic wording
// when the specifics can't be cheaply inferred — the full command is
// always visible to the user behind the "Show command" toggle.
func humanTitle(tool, command, rule string) string {
	switch tool {
	case "text_editor":
		return "Edit a file"
	case "computer":
		return "Interact with the desktop"
	}

	stripped := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "sudo "))
	lower := strings.ToLower(stripped)
	fields := strings.Fields(stripped)

	switch rule {
	case "dangerous_command_confirm":
		switch {
		case strings.HasPrefix(lower, "shutdown"):
			return "Shut down the machine"
		case strings.HasPrefix(lower, "reboot"), strings.HasPrefix(lower, "halt"), strings.HasPrefix(lower, "poweroff"):
			return "Reboot the machine"
		case strings.Contains(lower, "ufw disable"):
			return "Disable the firewall"
		case strings.HasPrefix(lower, "chmod"):
			return "Change permissions on a system path"
		case strings.HasPrefix(lower, "chown"):
			return "Change ownership on a system path"
		case strings.HasPrefix(lower, "systemctl"):
			return "Change a critical system service"
		}
		return "Run a potentially destructive command"

	case "filesystem_confirm":
		return "Read a sensitive system file"

	case "network_confirm":
		switch {
		case strings.HasPrefix(lower, "ssh"):
			return "Connect over SSH"
		case strings.HasPrefix(lower, "scp"), strings.HasPrefix(lower, "rsync"):
			return "Copy files to another machine"
		case regexp.MustCompile(`\bnc\b.*-l`).MatchString(stripped):
			return "Open a network listener"
		}
		return "Make a network connection"

	case "process_confirm":
		if len(fields) >= 2 && (fields[0] == "kill" || fields[0] == "pkill" || fields[0] == "killall") {
			target := fields[len(fields)-1]
			return "Stop process: " + target
		}
		if m := reSystemctlTarget.FindStringSubmatch(stripped); m != nil {
			return "Stop service: " + m[2]
		}
		return "Stop a running process or service"

	case "credential_confirm":
		return "List environment variables"

	case "curl_pipe_shell_confirm":
		return "Run a script from the internet"

	case "source_add_confirm":
		return "Add a new software source"

	case "snap_escape_confirm":
		return "Install privileged software"

	case "local_package_confirm":
		return "Install a package from a local file"

	case "package_remove_confirm":
		if m := rePackageRemove.FindStringSubmatch(stripped); m != nil {
			pkg := strings.TrimSpace(m[3])
			if pkg == "" || strings.HasPrefix(pkg, "-") {
				return "Remove software"
			}
			return "Remove software: " + pkg
		}
		return "Remove software"
	}

	return "Run a command"
}

var (
	reSystemctlTarget = regexp.MustCompile(`(?i)systemctl\s+(stop|restart|disable|mask|kill)\s+(\S+)`)
	rePackageRemove   = regexp.MustCompile(`(?i)\b(apt|apt-get|snap|yum|dnf)\s+(remove|purge|autoremove|erase)\s*(\S*)`)
)
