// SPDX-License-Identifier: Apache-2.0

package guardrails

import (
	"regexp"
	"strings"
)

// Policy evaluates a single guardrail rule against an action.
type Policy interface {
	Evaluate(action Action) Decision
	Description() string
}

// DefaultPolicies returns the built-in safety policies.
func DefaultPolicies() []Policy {
	return []Policy{
		&DangerousCommandPolicy{},
		&FileSystemPolicy{},
		&NetworkPolicy{},
		&ProcessPolicy{},
		&PackageInstallPolicy{},
		&CredentialAccessPolicy{},
		&BrowserSafetyPolicy{},
	}
}

// DangerousCommandPolicy blocks or requires confirmation for destructive shell commands.
type DangerousCommandPolicy struct{}

var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[rfRF]+\s+)?/`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\s+.*of=/dev/`),
	regexp.MustCompile(`>\s*/dev/sd`),
	regexp.MustCompile(`(?i)\bformat\s+[A-Z]:`),
	regexp.MustCompile(`(?i)\bshutdown\b`),
	regexp.MustCompile(`(?i)\breboot\b`),
	regexp.MustCompile(`(?i)\bhalt\b`),
	regexp.MustCompile(`(?i)\bpoweroff\b`),
	regexp.MustCompile(`(?i)\binit\s+0\b`),
	regexp.MustCompile(`(?i)\bsystemctl\s+(disable|mask|stop)\s+(sshd|ssh|networking|systemd-resolved)`),
	regexp.MustCompile(`(?i)\biptables\s+.*(-F|--flush|-X|--delete-chain)`),
	regexp.MustCompile(`(?i)\bufw\s+disable\b`),
	regexp.MustCompile(`(?i)\bchmod\s+(-R\s+)?777\s+/`),
	regexp.MustCompile(`(?i)\bchown\s+(-R\s+)?.*\s+/`),
	// Disk-partitioning tools targeting a real block device. Previously
	// these were a hard Block; now they Confirm so the classifier can
	// recommend deny (Soft Deny card with type-to-confirm override).
	regexp.MustCompile(`(?i)\b(fdisk|parted|sfdisk|gdisk|cfdisk)\b[^|;&]*\s/dev/`),
}

// blockPatterns is the *non-negotiable* System Block list. The product
// principle: hard-stop the actions that are both **irreversible** AND
// **highly likely to damage the whole setup**, plus anything that
// tampers with VibeCraft itself (the daemon, the updater, the reverse
// proxy that exposes customer apps). For everything else — destructive
// but bounded, or reversible — the classifier decides and the customer
// can override with friction.
//
// Things that are NOT here (intentionally — they live in Confirm and
// the classifier may recommend Soft Deny):
//   - rm -rf on a *sub-path* of /etc, /var, /usr (legitimate cleanup)
//   - dd / mkfs / fdisk on /dev/loop* or /dev/mapper/* (user-controlled)
//   - ufw disable, chmod 777 (reversible)
//   - apt remove / install / source-add (recoverable)
var blockPatterns = []*regexp.Regexp{
	// ── Catastrophic process operations ────────────────────────
	regexp.MustCompile(`:(){ :|:& };:`), // fork bomb — wedges the machine
	// Killing PID 1 (init) breaks the machine with no recovery. The
	// literal `-9` form is not the only spelling: `kill -s KILL 1`,
	// `kill -SIGKILL 1`, `kill -KILL 1`, and even a bare `kill 1` all
	// signal init. The POSIX `--` end-of-options separator (`kill -- 1`,
	// `kill -9 -- 1`) is another spelling that must not slip through to a
	// mere Confirm. Match a `kill` whose target operand is exactly PID 1,
	// with any signal flag and/or a `--` separator (or none) — but NOT
	// other PIDs like `kill 1234`.
	regexp.MustCompile(`(?i)\bkill\s+(?:-[A-Za-z0-9]+\s+|-s\s+\S+\s+|--\s+)*1\b`),
	regexp.MustCompile(`(?i)\bkillall\s+-9\b`), // killing every process — same

	// ── Disk-level destruction on real block devices ───────────
	// Loopback (/dev/loop*) and mapper (/dev/mapper/*) deliberately
	// excluded — those are user-controlled and not catastrophic.
	// Writing to /dev/sd*, /dev/nvme*, /dev/vd*, /dev/xvd* destroys
	// the boot/data disk irrecoverably.
	regexp.MustCompile(`(?i)\bdd\s+[^|;&]*\bof=/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*)\b`),
	regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\s+[^|;&]*/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*)\b`),
	regexp.MustCompile(`(?i)\b(fdisk|parted|sfdisk|gdisk|cfdisk)\s+[^|;&]*/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*)\b`),
	regexp.MustCompile(`>\s*/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*)\b`),
	// wipefs erases all filesystem signatures; shred overwrites the raw
	// device. On a real block device both are catastrophic + irreversible,
	// same as dd/mkfs above. Loopback (/dev/loop*) and mapper (/dev/mapper/*)
	// are deliberately excluded — those are user-controlled, not the boot disk.
	regexp.MustCompile(`(?i)\bwipefs\s+[^|;&]*/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*)\b`),
	regexp.MustCompile(`(?i)\bshred\s+[^|;&]*/dev/(sd[a-z][0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|xvd[a-z][0-9]*|vd[a-z][0-9]*)\b`),

	// ── Wholesale system-tree destruction ──────────────────────
	// Matches `rm -rf /etc` (the whole tree) but NOT `rm -rf /etc/subdir`
	// (a specific dir under it). The (?:\s|;|&|\||$) requires the path
	// to *end* at the system root, not extend with a `/`.
	regexp.MustCompile(`(?i)\brm\s+(-[rfRF]+\s+)+/(etc|var|usr|boot|lib|sbin|bin|sys|proc|opt|srv)(?:\s|;|&|\||$)`),
	regexp.MustCompile(`(?i)\brm\s+(-[rfRF]+\s+)+/(?:\s|;|&|\||$)`),

	// ── Platform integrity: VibeCraft daemon + Caddy ───────────
	// Tampering with the daemon binary, its systemd unit, the
	// auto-updater, or Caddy (which routes customer apps via the
	// /routes API) breaks the contract that VibeCraft can manage
	// the machine. These are blocked regardless of intent — the
	// platform is non-negotiable. Daemon process kills are caught
	// separately in ProcessPolicy.
	regexp.MustCompile(`/usr/local/bin/vibecraft-daemon\b`),
	regexp.MustCompile(`/usr/local/bin/vibecraft-update\.sh\b`),
	regexp.MustCompile(`/etc/systemd/system/vibecraft-daemon\.service\b`),
	regexp.MustCompile(`/etc/systemd/system/vibecraft-updater\.(service|timer)\b`),
	regexp.MustCompile(`/etc/caddy/Caddyfile\b`),
	regexp.MustCompile(`(?i)\b(systemctl|service)\s+(stop|disable|mask|kill|reload|restart)\s+(caddy|vibecraft-updater)\b`),
	// `service caddy stop` — sysvinit-style ordering (service NAME COMMAND).
	// On Ubuntu it delegates to systemctl; equivalent effect, different shape,
	// so we have to catch it explicitly.
	regexp.MustCompile(`(?i)\bservice\s+(caddy|vibecraft-updater)\s+(stop|disable|mask|kill|reload|restart)\b`),
}

func (p *DangerousCommandPolicy) Evaluate(action Action) Decision {
	if action.Type != "bash" {
		return Decision{Action: Allow}
	}

	cmd := action.Command

	// Short-circuit: if the command is a known-safe recipe chain, skip
	// the dangerous-patterns scan so benign `rm -f /tmp/.cell-*.done`
	// (which starts with `/`) doesn't hit the generic `rm .../` rule.
	// Security ordering: blockPatterns still run first below to catch
	// truly destructive shapes (rm -rf /etc, mkfs, etc.), but none of
	// those can match a chain made of the safe fragments defined in
	// safeRecipeChainPattern anyway.
	if safeRecipeChainPattern.MatchString(cmd) {
		return Decision{
			Action: Allow,
			Reason: "safe recipe on user-space GUI apps",
			Rule:   "dangerous_safe_recipe_allow",
		}
	}

	for _, pattern := range blockPatterns {
		if pattern.MatchString(cmd) {
			return Decision{
				Action: Block,
				Reason: "command matches a blocked destructive pattern",
				Rule:   "dangerous_command_block",
			}
		}
	}

	// Flag-order-agnostic rm check. The regex matchers above anchor the flag
	// group to the short-flag class -[rfRF]+, so GNU long-form and interleaved
	// flags bypass them entirely: `rm --recursive --force /` and the canonical
	// `rm -rf --no-preserve-root /` (the form that actually deletes / on modern
	// coreutils) were NOT matched and fell through to Allow — irreversible loss
	// of the whole machine, or of /home/vibecraft (the db.md company brain,
	// worker creds, ~/systems), reachable via prompt injection. This token-scans
	// every rm in the command and recognises recursive/force in any form.
	if d, ok := rmDecision(cmd); ok {
		return d
	}

	for _, pattern := range dangerousPatterns {
		if pattern.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this could modify or shut down the machine",
				Rule:   "dangerous_command_confirm",
			}
		}
	}

	return Decision{Action: Allow}
}

// rmSegmentSplitter splits a command line on shell separators so each rm in a
// chain (`a && rm -rf / ; b`) is inspected on its own.
var rmSegmentSplitter = regexp.MustCompile(`[;&|\n]+`)

// rmCatastrophicRoots are the top-level targets whose recursive force-removal is
// a hard Block (irreversible, whole-setup damage). A DEEPER path under one of
// these (e.g. /etc/foo) is not here — that falls through to a Confirm.
var rmCatastrophicRoots = map[string]bool{
	"/": true, "/etc": true, "/var": true, "/usr": true, "/boot": true,
	"/lib": true, "/lib64": true, "/sbin": true, "/bin": true, "/sys": true,
	"/proc": true, "/opt": true, "/srv": true, "/root": true, "/home": true,
	"/home/vibecraft": true, "~": true, "$HOME": true, "${HOME}": true,
}

// rmDecision returns a Block/Confirm decision (ok=true) when the command
// contains a recursive AND force rm (in any flag form: -rf, -r -f, --recursive
// --force, mixed), or any rm with --no-preserve-root. Block when a target is a
// catastrophic root; Confirm when a target is any other absolute path. ok=false
// means "no destructive rm here" and the caller continues its other checks.
func rmDecision(cmd string) (Decision, bool) {
	for _, seg := range rmSegmentSplitter.Split(cmd, -1) {
		fields := strings.Fields(seg)
		idx := -1
		for i, f := range fields {
			base := f
			if slash := strings.LastIndex(base, "/"); slash >= 0 {
				base = base[slash+1:]
			}
			if base == "rm" {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}

		recursive, force, noPreserve := false, false, false
		var operands []string
		for _, tok := range fields[idx+1:] {
			switch {
			case tok == "--":
				// end of options; everything after is an operand
			case tok == "--recursive":
				recursive = true
			case tok == "--force":
				force = true
			case tok == "--no-preserve-root":
				noPreserve = true
			case strings.HasPrefix(tok, "--"):
				// some other long option — ignore
			case strings.HasPrefix(tok, "-") && len(tok) > 1:
				for _, c := range tok[1:] {
					switch c {
					case 'r', 'R':
						recursive = true
					case 'f':
						force = true
					}
				}
			default:
				operands = append(operands, tok)
			}
		}

		if !((recursive && force) || noPreserve) {
			continue
		}

		for _, op := range operands {
			if rmTargetIsCatastrophic(op) {
				return Decision{
					Action: Block,
					Reason: "recursive force-remove of a system or home root",
					Rule:   "dangerous_command_block",
				}, true
			}
		}
		for _, op := range operands {
			if strings.HasPrefix(op, "/") || strings.HasPrefix(op, "~") ||
				strings.HasPrefix(op, "$HOME") || strings.HasPrefix(op, "${HOME}") {
				return Decision{
					Action: Confirm,
					Reason: "this could modify or shut down the machine",
					Rule:   "dangerous_command_confirm",
				}, true
			}
		}
	}
	return Decision{}, false
}

// rmRootGlob matches an rm operand that globs the whole filesystem root —
// `/*`, `/*.bak`, etc. `rm -rf /*` expands to every top-level entry and
// destroys the system just like `rm -rf /`, so it must hard-Block, not merely
// Confirm. A deeper glob like `/etc/*` is intentionally NOT caught here
// (legitimate cleanup under a subdir stays at Confirm).
var rmRootGlob = regexp.MustCompile(`^/\*`)

// rmTargetIsCatastrophic reports whether op (an rm operand) is a top-level
// system/home root whose recursive removal must be hard-blocked. A deeper path
// under such a root is intentionally NOT catastrophic (legitimate cleanup).
func rmTargetIsCatastrophic(op string) bool {
	// Strip surrounding quotes and a single trailing slash (but keep "/").
	op = strings.Trim(op, `"'`)
	if rmRootGlob.MatchString(op) {
		return true
	}
	if op != "/" {
		op = strings.TrimRight(op, "/")
	}
	return rmCatastrophicRoots[op]
}

func (p *DangerousCommandPolicy) Description() string {
	return "Block or require confirmation for destructive system commands (rm -rf /, mkfs, dd, shutdown, etc.)"
}

// FileSystemPolicy guards sensitive file system paths.
type FileSystemPolicy struct{}

func isShellOrEditorAction(actionType string) bool {
	switch actionType {
	case "bash", "text_editor", "str_replace_based_edit_tool", "str_replace_editor":
		return true
	default:
		return false
	}
}

var sensitivePathPatterns = []*regexp.Regexp{
	regexp.MustCompile(`/etc/passwd`),
	regexp.MustCompile(`/etc/sudoers`),
	regexp.MustCompile(`/etc/ssh/sshd_config`),
	regexp.MustCompile(`~?/\.ssh/authorized_keys`),
	regexp.MustCompile(`/etc/vibecraft/(daemon\.token|openai\.key|anthropic\.key|jwt\.secret|vault\.key|encryption\.key|health\.token)`),
	regexp.MustCompile(`/var/lib/vibecraft/vault\.enc`),
}

// credentialFilePatterns are sensitive files that hold raw credential
// material — system password hashes and SSH private keys. A *read* of one of
// these must hard-Block, never Confirm: a Confirm is eligible for the AI
// auto-review pass (RiskClassifier), whose prompt explicitly treats
// "read-only filesystem inspection (cat, ...)" as auto-approvable, so a
// `cat /etc/shadow` / `cat ~/.ssh/id_rsa` could be silently auto-approved and
// the file contents returned to the manager UNMASKED (the engine masks tool
// output only against vault secrets, not arbitrary credential files). Block
// short-circuits before the classifier and before execution — the strongest
// possible masking, since nothing is ever read. The non-secret config files
// above (passwd, sudoers, sshd_config, authorized_keys) stay at Confirm and
// surface a human approval card as designed.
var credentialFilePatterns = []*regexp.Regexp{
	regexp.MustCompile(`/etc/shadow`),
	regexp.MustCompile(`/etc/gshadow`),
	// SSH private keys: id_rsa, id_ed25519, id_ecdsa, id_dsa, … (the public
	// .pub siblings are matched too, but those are not secret and Blocking a
	// read of an id_*.pub is acceptably conservative).
	regexp.MustCompile(`~?/\.ssh/id_`),
}

var blockedPathPatterns = []*regexp.Regexp{
	// Named credential files. jwt_public.pem isn't secret (the platform
	// publishes the same key in JWKS) but we still block edits — the
	// daemon trusts whatever's there for token verification, and an
	// edit could swap in an attacker-controlled key.
	regexp.MustCompile(`/etc/vibecraft/(daemon\.token|openai\.key|anthropic\.key|vault\.key|encryption\.key|health\.token|jwt\.secret|jwt_public\.pem)`),
	// Any read-style access to anything under /etc/vibecraft/ — catches
	// ls, cat with wildcards (cat /etc/vibecraft/*), strings, base64,
	// xxd, hexdump, od, grep -r, find rooted there.
	regexp.MustCompile(`(?i)\b(cat|less|more|head|tail|strings|xxd|hexdump|od|base64|grep|egrep|fgrep|rg)\b[^|;&]*\s/etc/vibecraft(/|\s|$)`),
	regexp.MustCompile(`(?i)\bls\b[^|;&]*\s/etc/vibecraft(/|\s|$)`),
	regexp.MustCompile(`(?i)\bfind\s+[^|;&]*/etc/vibecraft(/|\s|$)`),
	// Daemon's encrypted SQLite + WAL/SHM siblings. SQLCipher-encrypted,
	// but blocking surfaces fast clear failure instead of a confusing
	// permission-denied + an opaque file. Matches the literal filenames
	// anywhere in the command (the agent has no legitimate reason to
	// reference them by name).
	regexp.MustCompile(`\bvibecraft\.db(-wal|-shm)?\b`),
	// Daemon data directory — blanket block on /var/lib/vibecraft.
	// Permissions already prevent the agent (running as vibecraft user
	// via bash) from reading the root of this tree, but the guardrail
	// surfaces a clear failure before the permission-denied confusion.
	regexp.MustCompile(`/var/lib/vibecraft(/|\s|$)`),
	regexp.MustCompile(`/var/lib/cloud/`),
	regexp.MustCompile(`/var/log/cloud-init`),
}

func (p *FileSystemPolicy) Evaluate(action Action) Decision {
	if !isShellOrEditorAction(action.Type) {
		return Decision{Action: Allow}
	}

	cmd := action.Command

	for _, pattern := range blockedPathPatterns {
		if pattern.MatchString(cmd) {
			return Decision{
				Action: Block,
				Reason: "access to system credentials and provisioning data is not allowed",
				Rule:   "filesystem_block",
			}
		}
	}

	// Credential-bearing files (password hashes, SSH private keys) hard-Block:
	// a Confirm here would be eligible for the auto-review pass and could be
	// auto-approved + returned unmasked. See credentialFilePatterns.
	for _, pattern := range credentialFilePatterns {
		if pattern.MatchString(cmd) {
			return Decision{
				Action: Block,
				Reason: "access to credential files (password hashes, private keys) is not allowed",
				Rule:   "filesystem_block",
			}
		}
	}

	for _, pattern := range sensitivePathPatterns {
		if pattern.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this reads a sensitive system file",
				Rule:   "filesystem_confirm",
			}
		}
	}

	return Decision{Action: Allow}
}

func (p *FileSystemPolicy) Description() string {
	return "Block access to daemon credentials; require confirmation for sensitive system files"
}

// NetworkPolicy guards network-related operations that reach out from
// the machine or expose a listening service.
type NetworkPolicy struct{}

// Commands that send write-style HTTP traffic, open a listener, or
// connect to another host on the user's behalf. Purely observational
// commands like `netstat` / `ss` are deliberately NOT in this list —
// they change nothing and shouldn't prompt.
var networkConfirmPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bcurl\b.*(-X\s+DELETE|-X\s+PUT)`),
	regexp.MustCompile(`\bwget\b.*-O\s*/`),
	regexp.MustCompile(`\bssh\b`),
	regexp.MustCompile(`\bscp\b`),
	regexp.MustCompile(`\brsync\b.*-e\s+ssh`),
	regexp.MustCompile(`\bnc\b.*-l`), // netcat listen
}

func (p *NetworkPolicy) Evaluate(action Action) Decision {
	if action.Type != "bash" {
		return Decision{Action: Allow}
	}

	for _, pattern := range networkConfirmPatterns {
		if pattern.MatchString(action.Command) {
			return Decision{
				Action: Confirm,
				Reason: "this connects to another machine or opens a listener",
				Rule:   "network_confirm",
			}
		}
	}

	return Decision{Action: Allow}
}

func (p *NetworkPolicy) Description() string {
	return "Confirm SSH/SCP, destructive HTTP writes, and network listeners"
}

// ProcessPolicy guards process management commands.
type ProcessPolicy struct{}

// cmdPrefix is one wrapper that can sit in front of the real command
// without changing what it does: `sudo`, a leading `VAR=value` environment
// assignment, or a launcher like `nice` / `timeout` / `env` / `nohup` that
// runs the command that follows. These prefixes are how a daemon-kill slips
// past a naive head anchor: `nice pkill vibecraft-daemon`,
// `timeout 5 systemctl stop vibecraft-daemon`, `env FOO=bar pkill vibecraft`,
// `FOO=bar pkill vibecraft` all invoke the same destructive command with a
// benign-looking token first. cmdHead consumes zero or more of these so the
// match lands on the actual command being invoked.
const cmdPrefix = `(?:` +
	`sudo\s+` +
	`|[A-Za-z_][A-Za-z0-9_]*=[^\s;&|]*\s+` +
	`|(?:nice|ionice|setsid|nohup|stdbuf|env|timeout|chrt|taskset)\b(?:\s+-{1,2}[^\s;&|]+|\s+\d+(?:\.\d+)?|\s+[A-Za-z_][A-Za-z0-9_]*=[^\s;&|]*)*\s+` +
	`)`

// cmdHead anchors a pattern to the start of a shell "simple command":
// either the beginning of the command line or after a separator (`;`,
// `&&`, `||`, `|`), then any leading whitespace and any number of wrapper
// prefixes (sudo, env assignments, nice/timeout/env/…). This avoids false
// positives like `grep kill foo` where "kill" is a grep argument rather
// than the command being invoked, while still catching the command when a
// wrapper or env assignment is stacked in front of it.
const cmdHead = `(?:^|[;&|]\s*)\s*(?:` + cmdPrefix + `)*`

// killDaemonPatterns covers the realistic ways a shell command could
// terminate or disrupt the vibecraft-daemon process.
var killDaemonPatterns = []*regexp.Regexp{
	// kill / pkill / killall with "vibecraft" anywhere in its arg list
	regexp.MustCompile(`(?i)` + cmdHead + `(kill|pkill|killall)\s+[^|;&]*vibecraft`),
	// systemctl actions targeting the vibecraft-* unit. `(?:--\S+\s+)*`
	// skips global options that legally precede the verb (`--now`,
	// `--no-block`, `--quiet`), which would otherwise let
	// `systemctl --now disable vibecraft-daemon` slip past. The verb list
	// also includes try-restart and reload-or-restart — both disrupt the
	// daemon just like stop/restart.
	regexp.MustCompile(`(?i)` + cmdHead + `systemctl\s+(?:--\S+\s+)*(stop|kill|restart|disable|mask|reload|try-restart|reload-or-restart)\s+[^|;&]*vibecraft`),
	// service command targeting the daemon
	regexp.MustCompile(`(?i)` + cmdHead + `service\s+vibecraft[\w-]*\s+(stop|restart|kill|reload|force-reload)`),
}

var killConfirmPatterns = []*regexp.Regexp{
	// Generic kill/pkill/killall — must be the command being invoked.
	regexp.MustCompile(`(?i)` + cmdHead + `(kill|pkill|killall)\s+`),
	// systemctl stop/restart of any service requires confirmation. Skip
	// global options before the verb and cover the alternate restart verbs
	// for the same reasons as the daemon-kill pattern above.
	regexp.MustCompile(`(?i)` + cmdHead + `systemctl\s+(?:--\S+\s+)*(stop|kill|restart|disable|mask|try-restart|reload-or-restart)\s+`),
}

// safeRecipeChainPattern matches a chain of "safe" shell operations on
// user-spawned GUI apps that the agent typically runs during layout
// recipes. Allowing these silently removes the main source of
// approval-card friction — every xterm recipe and every Chrome-close
// sequence would otherwise cost 2–5 round-trip approvals.
//
// The pattern admits a sequence of the following fragments, joined by
// `;`, newline, `&&`, or `||`:
//
//   - pkill/killall [-SIG]* <allowlisted-app> [>/dev/null|2>/dev/null|&>/dev/null]
//   - pgrep <allowlisted-app> [>/dev/null]
//   - sleep <non-negative-integer>
//   - echo <simple-word>
//
// Allowlisted apps: xterm / chrome (and google-chrome alias) / firefox /
// common image+PDF viewers (feh, eog, evince, xpdf). Signal flags are
// restricted to numeric (-9, -15) or real signal names (-TERM, -KILL,
// -SIGTERM, etc.) — deliberately excludes -f (full-command regex —
// unclear scope) and -F (pattern file).
//
// The pattern rejects anything with $, backticks, command substitution,
// pipes (|, distinct from ||), redirects to files other than /dev/null,
// unknown command names, quoted strings, or flags outside the signal
// whitelist. Anything non-trivial falls through to the normal Confirm
// path. The daemon-kill Block still runs first — `pkill vibecraft`
// anywhere in the chain remains Blocked.
//
// Concrete examples this now auto-Allows (previously Confirm):
//
//	pkill xterm
//	pkill xterm 2>/dev/null; sleep 1
//	pkill xterm
//	sleep 2
//	pkill -TERM chrome 2>/dev/null; sleep 2
//	pgrep chrome >/dev/null && pkill -KILL chrome 2>/dev/null
//	pgrep chrome >/dev/null && echo still-alive || echo closed
//	(full multi-line Chrome-close recipe as one bash call)
const signalFlag = `-(?:\d+|(?:SIG)?(?:HUP|INT|QUIT|KILL|TERM|USR1|USR2|STOP|CONT|ABRT|ALRM))`

// allowlistApp covers the user-space GUI apps the agent legitimately
// pkills as part of layout/cleanup recipes. zutty is here even though
// the agent should NEVER launch zutty (see prompt.md §"Real terminal
// windows") — it's a transitively-installed terminal emulator that
// historically rendered TUI apps badly, and the agent has been observed
// defensively running `pkill zutty` before launching xterm to guarantee
// no stray zutty processes interfere. That defensive cleanup is
// harmless (there are usually no zutty processes anyway) and should not
// fire a friction-y guardrail card. Adding zutty here keeps the
// "launch Claude Code" path seamless even when the model insists on
// pre-cleanup against the prompt's wishes.
const allowlistApp = `(?:xterm|zutty|chrome|google-chrome|google-chrome-stable|firefox|feh|eog|evince|xpdf)`
const pkillFragment = `(?:pkill|killall)\s+(?:` + signalFlag + `\s+)*` + allowlistApp +
	`(?:\s*(?:2>\s*/dev/null|&>\s*/dev/null|>\s*/dev/null))?`
const pgrepFragment = `pgrep\s+` + allowlistApp + `(?:\s*>\s*/dev/null)?`

// Sleep accepts fractional seconds — the multi-window recipe uses
// `sleep 0.5` as a stagger between xterm launches.
const sleepFragment = `sleep\s+\d+(?:\.\d+)?`

// echo accepts either a bare word or a double-quoted alphanumeric
// string. The quoted form is what the model emits as a "completion
// marker" inside cleanup recipes (e.g. `echo "cleared"`, `echo "done"`).
// The character class inside the quotes is deliberately narrow — no $,
// no backticks, no shell metacharacters — to keep the auto-allow surface
// scoped to harmless status messages.
const echoFragment = `echo\s+(?:[A-Za-z0-9_.:/+-]+|"[A-Za-z0-9_.:/+ -]*")`

// `true` / `false` are no-op shell builtins that the model frequently
// chains as defensive trailers (`... || true`). They cannot do harm on
// their own, and rejecting them broke the safe-chain match for an
// otherwise allowlisted recipe.
const noopFragment = `(?:true|false)`

// Per-cell sync cleanup from an earlier multi-window recipe variant in
// prompt.md. The exact path is fixed — `/tmp/.cell-*.done`. Anything
// else under /tmp still falls through to DangerousCommandPolicy's
// generic rm check. Kept allowlisted in case any prior daemon writes
// these files; the current recipe doesn't use them.
const cellCleanupFragment = `rm\s+-f\s+/tmp/\.cell-\*\.done`

const safeFragment = `(?:` + pkillFragment + `|` + pgrepFragment + `|` + sleepFragment + `|` + echoFragment + `|` + noopFragment + `|` + cellCleanupFragment + `)`
const chainSeparator = `\s*(?:;|&&|\|\|)\s*|\s+`

var safeRecipeChainPattern = regexp.MustCompile(
	`(?s)^\s*` + safeFragment + `(?:(?:` + chainSeparator + `)` + safeFragment + `)*\s*;?\s*$`,
)

func (p *ProcessPolicy) Evaluate(action Action) Decision {
	if action.Type != "bash" {
		return Decision{Action: Allow}
	}

	cmd := action.Command

	// Block any command that would plausibly stop the daemon.
	for _, pat := range killDaemonPatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Block,
				Reason: "cannot kill or stop the VibeCraft daemon process",
				Rule:   "process_block",
			}
		}
	}

	// Allow safe recipe chains — pkill/pgrep/sleep/echo on known
	// user-spawned GUI apps. Must run AFTER the daemon-kill block (so
	// `pkill vibecraft` still Blocks, even inside a chain) and BEFORE
	// the generic kill Confirm (so layout recipes don't spam approval
	// cards).
	if safeRecipeChainPattern.MatchString(cmd) {
		return Decision{
			Action: Allow,
			Reason: "safe recipe on user-space GUI apps",
			Rule:   "process_safe_recipe_allow",
		}
	}

	// Confirm other kill / systemctl stop commands.
	for _, pat := range killConfirmPatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this will stop a running process or service",
				Rule:   "process_confirm",
			}
		}
	}

	return Decision{Action: Allow}
}

func (p *ProcessPolicy) Description() string {
	return "Block killing the daemon; require confirmation for other process kills"
}

// PackageInstallPolicy guards the ways a command can change what software
// is present on the machine. The guiding principle: installing a package
// from a configured, trusted source is as benign as a human running
// `brew install jq` on their laptop — Allow it. The cases that warrant a
// user prompt are the ones that change the *risk model* itself: adding
// new software sources, installing local/unsigned binaries, running code
// piped off the network, escaping a sandbox, or removing software the
// user might still be relying on.
type PackageInstallPolicy struct{}

// Curl-to-shell and wget-to-shell: arbitrary code from the internet runs
// with current privileges. Covers both pipe (`curl ... | sh`) and
// process-substitution (`bash <(curl ...)`) forms.
var curlPipeShellPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(curl|wget)\b[^|;&]*\|\s*(sudo\s+)?(sh|bash|zsh|python3?)\b`),
	regexp.MustCompile(`(?i)\b(sh|bash|zsh|python3?)\b\s+<\(\s*(curl|wget)\b`),
}

// Adding a new apt source (PPA, third-party repo). Changes what future
// `apt install` commands are allowed to install from.
var addSourcePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\badd-apt-repository\b`),
	regexp.MustCompile(`(?i)/etc/apt/sources\.list(\.d)?`),
}

// Installing an unsigned or local package file (provenance unknown).
var localInstallPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bdpkg\s+(-i|--install)\b`),
	regexp.MustCompile(`(?i)\brpm\s+(-i|-U|--install|--upgrade)\b`),
}

// Snap `--classic` / `--devmode` escapes the snap sandbox and grants the
// package full access to the system.
var snapEscapePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsnap\s+install\b[^|;&]*--(classic|devmode)\b`),
}

// Removing software: user probably doesn't realize some workflow still
// depends on the thing being removed. Covers apt/apt-get/snap.
var packageRemovePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bapt(-get)?\s+(remove|purge|autoremove)\b`),
	regexp.MustCompile(`(?i)\bsnap\s+remove\b`),
	regexp.MustCompile(`(?i)\b(yum|dnf)\s+(remove|erase)\b`),
}

func (p *PackageInstallPolicy) Evaluate(action Action) Decision {
	if action.Type != "bash" {
		return Decision{Action: Allow}
	}

	cmd := action.Command

	for _, pat := range curlPipeShellPatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this runs code downloaded from the internet directly on the machine",
				Rule:   "curl_pipe_shell_confirm",
			}
		}
	}

	for _, pat := range addSourcePatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this adds a new source of software to the machine",
				Rule:   "source_add_confirm",
			}
		}
	}

	for _, pat := range snapEscapePatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this installs software that can access the whole system",
				Rule:   "snap_escape_confirm",
			}
		}
	}

	for _, pat := range localInstallPatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this installs a package from a local file",
				Rule:   "local_package_confirm",
			}
		}
	}

	for _, pat := range packageRemovePatterns {
		if pat.MatchString(cmd) {
			return Decision{
				Action: Confirm,
				Reason: "this removes software from the machine",
				Rule:   "package_remove_confirm",
			}
		}
	}

	return Decision{Action: Allow}
}

func (p *PackageInstallPolicy) Description() string {
	return "Allow standard package installs; confirm source changes, local-file installs, curl-to-shell, sandbox escapes, and removals"
}

// CredentialAccessPolicy guards credential and secret access patterns.
type CredentialAccessPolicy struct{}

func (p *CredentialAccessPolicy) Evaluate(action Action) Decision {
	if !isShellOrEditorAction(action.Type) {
		return Decision{Action: Allow}
	}

	cmd := strings.ToLower(action.Command)

	// Block attempts to read encryption keys or dump the vault.
	if strings.Contains(cmd, "vault.key") || strings.Contains(cmd, "vault.enc") || strings.Contains(cmd, "encryption.key") {
		return Decision{
			Action: Block,
			Reason: "direct vault/encryption key access is not permitted",
			Rule:   "credential_block",
		}
	}

	// Confirm if the command might expose environment variables with secrets.
	if matched, _ := regexp.MatchString(`\benv\b|\bprintenv\b|\bset\b.*\|`, cmd); matched {
		return Decision{
			Action: Confirm,
			Reason: "this may print environment variables that could contain secrets",
			Rule:   "credential_confirm",
		}
	}

	return Decision{Action: Allow}
}

func (p *CredentialAccessPolicy) Description() string {
	return "Block direct vault file access; confirm commands that may expose environment secrets"
}

// BrowserSafetyPolicy guards browser-related actions.
type BrowserSafetyPolicy struct{}

func (p *BrowserSafetyPolicy) Evaluate(action Action) Decision {
	if action.Type != "computer" {
		return Decision{Action: Allow}
	}

	// No specific blocks for browser actions by default.
	// Custom rules can restrict navigation to certain domains.

	return Decision{Action: Allow}
}

func (p *BrowserSafetyPolicy) Description() string {
	return "Browser actions are allowed by default; custom rules can restrict domains"
}

// CustomRulePolicy wraps a user-defined Rule as a Policy.
type CustomRulePolicy struct {
	rule Rule
}

func (p *CustomRulePolicy) Evaluate(action Action) Decision {
	if !p.rule.Enabled {
		return Decision{Action: Allow}
	}

	re, err := regexp.Compile(p.rule.Pattern)
	if err != nil {
		// Fail CLOSED. A custom rule whose pattern won't compile must not
		// silently allow the action it was authored to guard — the old
		// code swallowed the compile error and fell through to Allow,
		// which means a single malformed rule disabled itself without any
		// signal. Block instead and name the offending rule so it gets
		// noticed and fixed.
		return Decision{
			Action: Block,
			Reason: "guardrail rule has an invalid pattern and is failing closed",
			Rule:   p.rule.Name,
		}
	}

	// Match the command and the action type as SEPARATE fields. The old
	// code matched the pattern against the command and, on miss/error,
	// re-tested the same pattern against action.Type — so a command-shaped
	// pattern could fire on the bare type string (e.g. a rule meant for a
	// shell command leaking onto every "text_editor" action) and an error
	// on the command match was discarded. Evaluating each field on its own
	// keeps the intent explicit: a rule matches when its pattern hits the
	// command OR the action type, with no cross-contamination from a
	// swallowed error.
	if !re.MatchString(action.Command) && !re.MatchString(action.Type) {
		return Decision{Action: Allow}
	}

	// Validate the rule's action against the known enum and fail CLOSED on
	// anything unrecognized. The stored action string is free text (set via
	// the /rules API, which only checks it is non-empty), so a typo or a
	// miscased value — "Block", "deny", "confirmm" — would otherwise be
	// wrapped verbatim as ActionType("Block"). The engine only treats the
	// canonical "block"/"confirm" values as restrictive (see engine.go
	// Evaluate), so any other string silently degraded to Allow — the rule
	// that was authored to GUARD an action instead permitted it. Normalize
	// to the enum; reject unknown actions to the safest restrictive verdict
	// (Block) and surface the misconfiguration in the reason + rule name.
	resolved, ok := ResolveRuleAction(p.rule.Action)
	if !ok {
		return Decision{
			Action: Block,
			Reason: "guardrail rule has an unrecognized action and is failing closed",
			Rule:   p.rule.Name,
		}
	}

	return Decision{
		Action: resolved,
		Reason: p.rule.Description,
		Rule:   p.rule.Name,
	}
}

// ResolveRuleAction maps a custom-rule action string to a known ActionType.
// It is case-insensitive and trims surrounding whitespace so "Block", " block "
// and "BLOCK" all resolve to Block. ok is false for any value that is not one
// of the three canonical verdicts ("allow", "confirm", "block"); callers must
// fail closed (treat an unknown action as Block) rather than letting it fall
// through to Allow. Shared by CustomRulePolicy.Evaluate (load-time enforcement)
// and the /rules write handler (validate-before-persist).
func ResolveRuleAction(raw string) (ActionType, bool) {
	switch ActionType(strings.ToLower(strings.TrimSpace(raw))) {
	case Allow:
		return Allow, true
	case Confirm:
		return Confirm, true
	case Block:
		return Block, true
	default:
		return "", false
	}
}

func (p *CustomRulePolicy) Description() string {
	return p.rule.Description
}
