// SPDX-License-Identifier: Apache-2.0

package guardrails

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	managerclient "github.com/carloslfu/computer.md/daemon/manager"
)

// ClassifierModel is the model we use for the auto-review pass. Launch keeps
// classifier/main-loop/summarizer on one tested OpenAI manager model to avoid
// extra routing complexity.
const ClassifierModel = managerclient.DefaultModelID

// ClassifierTimeout caps the auto-review wait. The agent loop already
// pauses on Confirm (we haven't shown the human card yet); we add at
// most this much time before falling back to the human. Generous to
// absorb p99 latency without forcing escalation on a slow round-trip.
const ClassifierTimeout = 8 * time.Second

// ClassifierVerdict is the auto-review's decision. Three states:
//   - approve:  silently safe, skip the human card entirely
//   - escalate: classifier isn't sure, ask the human as a peer ("your call")
//   - deny:     classifier sees genuine risk; default to no, but the human
//     can override with friction proportional to consequence.
//
// Note: deny is *not* the same as a hard System Block. System Block is
// non-negotiable (credentials, killing the daemon, fork bomb). Deny is
// "system thinks this is a bad idea, here's why — proceed if you mean it."
type ClassifierVerdict string

const (
	VerdictApprove  ClassifierVerdict = "approve"  // safe enough to skip the human card
	VerdictEscalate ClassifierVerdict = "escalate" // route to human as usual
	VerdictDeny     ClassifierVerdict = "deny"     // classifier-recommended deny; user can override with friction
)

// ClassifierResult is what RiskClassifier.Classify returns. Reason is
// shown to the customer if escalated, or attached to the audit log if
// auto-approved — either way, the customer can read what was decided
// and why.
type ClassifierResult struct {
	Verdict ClassifierVerdict
	Reason  string
}

// RiskClassifier is the second-stage AI review that sits between the
// guardrail engine's pattern-match Confirm decision and the human
// approval card. It can ONLY downgrade Confirm to Allow (auto-approve);
// it never softens a Block, and it never escalates an Allow.
//
// The bet: most of the actions that hit Confirm are structurally safe
// variants of patterns that look risky on regex but aren't on read —
// e.g. `curl URL | python3 -c '<inline bs4>'` to read a doc page. A
// power-user customer would auto-approve those without thinking. We
// automate the inevitable yes and reserve the human card for genuine
// risk.
type RiskClassifier struct {
	manager *managerclient.Client
	model   string
}

// NewRiskClassifier returns a classifier wired against the manager client.
func NewRiskClassifier(manager *managerclient.Client, model string) *RiskClassifier {
	if model == "" {
		model = ClassifierModel
	}
	return &RiskClassifier{manager: manager, model: model}
}

// classifierSystemPrompt frames the task for the model. Stable across
// every call — perfect cache shape if we later turn on prompt caching
// for SendText. Be opinionated: vague prompts produce vague verdicts.
const classifierSystemPrompt = `You are a safety reviewer for a desktop agent that operates a customer's Linux machine on their behalf. A guardrail rule just flagged an action. You have the recent conversation in front of you and you're judging the action in context — not pattern-matching the command.

Your job is to decide between three verdicts. Be calibrated, not paranoid.

CORE PRINCIPLE — context, reversibility, intent-alignment:
- **Context** — does this command make sense given what the customer has been doing in this conversation? An obviously-test command (e.g. fake device path, the customer explicitly says they're verifying UI) is different from an unexpected destructive turn.
- **Reversibility** — would the customer be able to undo this if it ran by mistake? An apt-remove can be re-installed. A dd to a real disk cannot. Friction should scale with permanence.
- **Intent-alignment** — does the command match what the customer was asking for? If they said "clean up /tmp/foo" and the agent generated rm -rf /tmp/foo, that's aligned. If they said "clean up /tmp/foo" and the agent generated rm -rf /etc, that's misaligned and almost certainly a mistake — friction should fire even if the customer happened to click through.

THREE VERDICTS:

verdict "approve" — structurally safe; skip the human card entirely.
- Read-only fetches piped into local text processors (lynx -dump, pandoc, python3 -c '<inline parse>', jq, grep, sed, awk)
- Standard package operations from default repos (apt install / apt-get install <pkg>)
- Read-only filesystem inspection (ls, cat, find, du, tree, stat, file)
- Operations confined to the customer's workspace (/home/vibecraft/...) or /tmp
- Standard developer workflow (git clone of public repos, npm/pip/cargo)
- Diagnostic commands (ps, top, df, ip, ss, ip route)
- Commands that target a clearly fake/non-existent resource as part of an explicit test the customer described in the conversation (e.g. customer says "verify the UI on a non-existent device" and the command targets /dev/sdz which doesn't exist on this machine)

verdict "escalate" — intent is the open question; route to the human as a routine approval.
- apt remove <pkg> on a non-critical package
- systemctl stop <non-system-service> (nginx, redis, etc.)
- SSH/SCP/rsync to an external host
- Adding a known apt source from a recognized vendor (Node, GitHub Container Registry)
- Configuration changes in /etc that the customer reasonably owns
- Removing a sub-path of /etc, /var, /usr that the customer indicated they want to clean up

verdict "deny" — strongly recommend against; default to denial but allow override with friction. Use this only when the action is genuinely high-risk in this context.
- curl | sh / curl | sudo bash piping from unknown URLs
- Adding a new apt source from a placeholder/suspicious domain that doesn't match what the customer asked for
- Disabling security features (ufw disable, iptables -F, chmod 777 on a system path) WITHOUT the customer giving a concrete reason
- Destructive operations whose target doesn't match the customer's stated intent
- Operations sending arbitrary data to unfamiliar external destinations

NOTE: catastrophically irreversible operations (dd to a real /dev/sd*, mkfs on a real disk, rm -rf of /etc or /var as a whole tree, tampering with the VibeCraft daemon itself) are HARD-BLOCKED by the system before your judgment is called. You will never see them. Don't waste a deny verdict trying to catch them; they're already gone.

OUTPUT
Strict JSON only, no prose, no code fences:
{"verdict": "approve" | "escalate" | "deny", "reason": "<one short concrete sentence>"}

The reason is the customer-facing "why" if you escalate or deny, and the audit-log "why" if you approve. Be specific: cite the actual command shape and the actual contextual reason. Avoid filler.`

// Classify runs the auto-review pass. Any failure (timeout, network,
// malformed reply) returns Escalate with the original policy reason —
// the human card is the safe fallback.
//
// taskInstruction is the customer's original request — the "what they
// asked for" anchor. recentChat is a plain-text rendering of the recent
// conversation (the engine builds this from GetMessages on the
// conversation, filtered to text + a one-line summary of any tool
// activity). Both let the classifier judge intent-alignment instead of
// just pattern-matching the command.
func (rc *RiskClassifier) Classify(parentCtx context.Context, action Action, policy Decision, taskInstruction string, recentChat string) ClassifierResult {
	if rc == nil || rc.manager == nil {
		return ClassifierResult{Verdict: VerdictEscalate, Reason: policy.Reason}
	}

	ctx, cancel := context.WithTimeout(parentCtx, ClassifierTimeout)
	defer cancel()

	chatBlock := ""
	if trimmed := strings.TrimSpace(recentChat); trimmed != "" {
		chatBlock = fmt.Sprintf("\n\nRecent conversation (oldest to newest):\n%s\n", trimmed)
	}

	userPrompt := fmt.Sprintf(`Customer's original task: %s
%s
Action the agent is about to run:
- Tool: %s
- Command: %s

Guardrail that flagged it:
- Rule: %s
- Stated concern: %s

Decide.`, strings.TrimSpace(taskInstruction), chatBlock, action.Type, action.Command, policy.Rule, policy.Reason)

	start := time.Now()
	resp, err := rc.manager.SendText(ctx, rc.model, classifierSystemPrompt, userPrompt, 256)
	elapsed := time.Since(start)
	if err != nil {
		log.Printf("classifier: SendText failed after %s: %v — escalating", elapsed, err)
		return ClassifierResult{Verdict: VerdictEscalate, Reason: policy.Reason}
	}

	verdict, reason := parseClassifierResponse(resp.TextContent)
	if reason == "" {
		reason = policy.Reason
	}
	log.Printf(
		"classifier: verdict=%s elapsed=%s model=%s rule=%q reason=%q",
		verdict, elapsed, rc.model, policy.Rule, reason,
	)
	return ClassifierResult{Verdict: verdict, Reason: reason}
}

// parseClassifierResponse pulls the JSON object out of the model's
// reply. The model may wrap it in code fences or chatter despite the
// strict-JSON instruction; we handle that and fall back to escalation
// on anything we can't parse. Escalation is always the safe default.
func parseClassifierResponse(text string) (ClassifierVerdict, string) {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(t, "```")
	t = strings.TrimSpace(t)

	start := strings.Index(t, "{")
	end := strings.LastIndex(t, "}")
	if start < 0 || end < 0 || end < start {
		return VerdictEscalate, ""
	}
	t = t[start : end+1]

	var parsed struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(t), &parsed); err != nil {
		return VerdictEscalate, ""
	}

	switch strings.ToLower(strings.TrimSpace(parsed.Verdict)) {
	case "approve":
		return VerdictApprove, strings.TrimSpace(parsed.Reason)
	case "deny":
		return VerdictDeny, strings.TrimSpace(parsed.Reason)
	default:
		// Any unrecognised string falls to escalate — the safe middle
		// ground. A garbled "deny" arriving as "denyy" should not
		// silently auto-approve, but it also shouldn't escalate to the
		// stronger Soft Deny path without intent.
		return VerdictEscalate, strings.TrimSpace(parsed.Reason)
	}
}
