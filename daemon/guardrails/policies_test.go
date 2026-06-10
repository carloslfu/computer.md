// SPDX-License-Identifier: Apache-2.0

package guardrails

import "testing"

// policyCase is one row in a table-driven policy test: the bash command
// text the agent would run, and the decision we expect.
type policyCase struct {
	name    string
	command string
	toolTyp string // defaults to "bash" when empty
	want    ActionType
}

func runPolicy(t *testing.T, p Policy, cases []policyCase) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			typ := c.toolTyp
			if typ == "" {
				typ = "bash"
			}
			got := p.Evaluate(Action{Type: typ, Command: c.command}).Action
			if got != c.want {
				t.Fatalf("cmd=%q: got %s, want %s", c.command, got, c.want)
			}
		})
	}
}

// ProcessPolicy covers the killing-the-daemon bypass that lost us a
// full evening of confusion. Every single one of the "bypassed" rows
// used to slip past the old \bkill\b.*vibecraft pattern.
func TestProcessPolicy_DaemonKillCoverage(t *testing.T) {
	p := &ProcessPolicy{}
	cases := []policyCase{
		// Block: any command that would stop the daemon.
		{"kill with vibecraft in cmd", "kill vibecraft", "", Block},
		{"kill by process name subcommand", "kill $(pgrep vibecraft)", "", Block},
		{"pkill by name", "pkill vibecraft-daemon", "", Block},
		{"pkill with -f", "pkill -f vibecraft-daemon", "", Block},
		{"killall vibecraft", "killall vibecraft-daemon", "", Block},
		{"systemctl stop unit", "systemctl stop vibecraft-daemon", "", Block},
		{"sudo systemctl stop unit", "sudo systemctl stop vibecraft-daemon", "", Block},
		{"systemctl restart unit", "systemctl restart vibecraft-daemon", "", Block},
		{"systemctl disable unit", "sudo systemctl disable vibecraft-daemon", "", Block},
		{"systemctl kill unit", "systemctl kill vibecraft-daemon", "", Block},
		{"systemctl mask unit", "sudo systemctl mask vibecraft-daemon", "", Block},
		{"service stop", "service vibecraft-daemon stop", "", Block},
		{"service restart", "sudo service vibecraft-daemon restart", "", Block},

		// Confirm: other kills/stops that don't target the daemon.
		{"kill generic PID", "kill 1234", "", Confirm},
		{"kill -9 generic PID", "kill -9 4321", "", Confirm},
		{"pkill arbitrary", "pkill nginx", "", Confirm},
		{"systemctl stop arbitrary service", "systemctl stop nginx", "", Confirm},

		// Allow: non-matching commands.
		{"ls", "ls /tmp", "", Allow},
		{"echo", "echo hello", "", Allow},
		{"grep with kill word inside text", "grep kill /etc/hosts", "", Allow},
		{"non-bash tool type", "kill 1", "computer", Allow},
	}
	runPolicy(t, p, cases)
}

// TestProcessPolicy_SafeRecipeChain covers the silent-Allow path for
// chains of pkill/pgrep/sleep/echo on known user-spawned GUI apps.
// These are how the agent runs layout recipes (xterm grids, Chrome
// close); the generic Confirm used to create one approval card per
// step — see C.3 and D.4 in E2E_TESTS.md.
//
// The chain pattern allows multiple fragments joined by `;`, newline,
// `&&`, or `||`. Each fragment must be pkill/killall/pgrep of an
// allowlisted app (optionally with signal flag / redirect to
// /dev/null), `sleep <int>`, or `echo <simple-word>`. Anything with
// command substitution, pipes, unknown targets, or unquoted paths
// falls through to Confirm.
func TestProcessPolicy_SafeRecipeChain(t *testing.T) {
	p := &ProcessPolicy{}
	cases := []policyCase{
		// Allow: single pkill / killall of known GUI apps.
		{"pkill xterm bare", "pkill xterm", "", Allow},
		{"pkill xterm with stderr redirect", "pkill xterm 2>/dev/null", "", Allow},
		{"pkill chrome bare", "pkill chrome", "", Allow},
		{"pkill chrome with TERM signal", "pkill -TERM chrome", "", Allow},
		{"pkill chrome with -9", "pkill -9 chrome 2>/dev/null", "", Allow},
		{"pkill chrome with SIGTERM long form", "pkill -SIGTERM chrome", "", Allow},
		{"pkill chrome with -KILL", "pkill -KILL chrome 2>/dev/null", "", Allow},
		{"pkill google-chrome", "pkill google-chrome", "", Allow},
		{"pkill google-chrome-stable", "pkill google-chrome-stable", "", Allow},
		{"pkill firefox", "pkill firefox 2>/dev/null", "", Allow},
		{"killall xterm", "killall xterm", "", Allow},
		{"killall chrome with signal", "killall -9 chrome", "", Allow},
		{"pkill chrome with trailing semicolon", "pkill chrome;", "", Allow},
		{"pkill feh image viewer", "pkill feh", "", Allow},
		{"pkill eog", "pkill eog", "", Allow},
		// zutty is allowlisted as defense-in-depth for the "launch one
		// Claude Code" path. The agent may defensively pkill zutty (even
		// though no zutty processes exist on this machine) before
		// launching xterm, and the v0.19.4 fix keeps that benign
		// preamble friction-free.
		{"pkill zutty (defensive cleanup)", "pkill zutty", "", Allow},
		{"pkill zutty 2>/dev/null", "pkill zutty 2>/dev/null", "", Allow},
		{"killall zutty", "killall zutty", "", Allow},

		// Allow: xterm recipe forms from daemon/prompt.md.
		{"recipe: pkill xterm 2>/dev/null; sleep 1", "pkill xterm 2>/dev/null; sleep 1", "", Allow},
		{"recipe: pkill chrome; sleep 1", "pkill chrome; sleep 1", "", Allow},
		{"recipe: pkill -TERM chrome 2>/dev/null; sleep 2", "pkill -TERM chrome 2>/dev/null; sleep 2", "", Allow},
		{"recipe: pkill xterm then sleep on newline", "pkill xterm\nsleep 2", "", Allow},
		{"recipe with trailing semicolon after sleep", "pkill xterm 2>/dev/null; sleep 1;", "", Allow},

		// Allow: Chrome-close recipe — all three phases individually.
		{"close step 1: TERM + sleep", "pkill -TERM chrome 2>/dev/null; sleep 2", "", Allow},
		{"close step 2: guarded KILL", "pgrep chrome >/dev/null && pkill -KILL chrome 2>/dev/null", "", Allow},
		{"close step 3: verify", "pgrep chrome >/dev/null && echo still-alive || echo closed", "", Allow},

		// Allow: Chrome-close recipe as one concatenated bash call (the
		// way the agent runs multi-line code blocks from the prompt).
		{"close recipe: all three phases in one call", "pkill -TERM chrome 2>/dev/null\nsleep 2\npgrep chrome >/dev/null && pkill -KILL chrome 2>/dev/null\nsleep 1\npgrep chrome >/dev/null && echo still-alive || echo closed", "", Allow},

		// Allow: other reasonable chains on allowlist apps.
		{"chain: pkill two allowlist apps", "pkill chrome && pkill firefox", "", Allow},
		{"chain: pkill + echo marker", "pkill chrome; echo done", "", Allow},
		{"chain: pkill + sleep + echo", "pkill chrome; sleep 1; echo done", "", Allow},
		// The exact failure-mode chain from the v0.19.3 → v0.19.4 fix:
		// agent ran this before launching Claude Code; v0.19.3 tripped a
		// soft-deny card; v0.19.4 auto-allows so the path stays seamless
		// even when the model insists on defensive pre-cleanup.
		{"v0.19.4 defensive launch preamble (xterm+zutty+echo quoted)",
			`pkill xterm 2>/dev/null; pkill zutty 2>/dev/null; sleep 1; echo "cleared"`, "", Allow},
		// echo with double-quoted alphanumeric "marker" string is now
		// allowed (previously only bare-word echo was). Quoted form is
		// what the model emits inside cleanup recipes as a completion
		// signal.
		{"echo with quoted marker (cleared)", `echo "cleared"`, "", Allow},
		{"echo with quoted marker (done)", `echo "done"`, "", Allow},
		{"echo with quoted marker (closed)", `echo "closed"`, "", Allow},
		{"chain: pkill + sleep + echo quoted", `pkill chrome; sleep 1; echo "ok"`, "", Allow},

		// Allow: fractional sleep — the multi-window recipe staggers
		// xterm launches with `sleep 0.5`.
		{"fractional sleep alone", "sleep 0.5", "", Allow},
		{"chain with fractional sleep", "pkill xterm; sleep 0.5", "", Allow},
		{"chain: pkill + fractional sleep stderr-redirect", "pkill xterm 2>/dev/null; sleep 0.5", "", Allow},
		{"chain: pkill + sleep 1.5 (fractional)", "pkill chrome; sleep 1.5", "", Allow},

		// Allow: per-cell sync cleanup from the multi-window recipe.
		{"bare rm -f /tmp/.cell-*.done", "rm -f /tmp/.cell-*.done", "", Allow},
		{"chain: pkill + sleep + rm-cells", "pkill xterm 2>/dev/null; sleep 1; rm -f /tmp/.cell-*.done", "", Allow},
		{"chain: sleep + rm-cells (seen in tests)", "sleep 0.5\nrm -f /tmp/.cell-*.done", "", Allow},

		// Allow: defensive shell idioms the model emits inside recipes.
		// `... || true` (no-op trailer) shouldn't break the safe-chain
		// match — `true`/`false` are harmless builtins.
		{"chain: pkill || true", "pkill xterm 2>/dev/null || true", "", Allow},
		{"chain: pkill && sleep || true", "pkill xterm 2>/dev/null && sleep 1 || true", "", Allow},
		{"chain: pgrep || false marker", "pgrep chrome || false", "", Allow},

		// Confirm: should keep the user in the loop.
		{"pkill with -f flag (unclear regex scope)", "pkill -f chrome", "", Confirm},
		{"pkill unknown app name", "pkill node", "", Confirm},
		{"pkill sshd", "pkill sshd", "", Confirm},
		{"chain ending in pkill sshd", "pkill chrome; pkill sshd", "", Confirm},
		{"pkill chrome piped to grep (single pipe, not ||)", "pkill chrome | grep foo", "", Confirm},
		// (Fractional sleeps and true/false are now Allow — see Allow
		// section above. They were promoted out of Confirm because the
		// chain still only admits pkill/pgrep/sleep/echo/true/false on
		// allowlisted apps; widening the inert atoms doesn't widen what
		// the chain can DO.)
		{"kill PID (no app name)", "kill 1234", "", Confirm},
		{"sudo pkill xterm (sudo changes risk model)", "sudo pkill xterm", "", Confirm},
		{"pkill with process-substitution", "pkill $(pgrep -n chrome)", "", Confirm},
		{"pkill with backtick substitution", "pkill `pgrep chrome`", "", Confirm},
		{"pkill chrome with extra path arg", "pkill chrome /tmp", "", Confirm},
		{"pkill chrome with env var", "pkill $FOO", "", Confirm},
		{"echo with pipe", "pkill chrome; echo done | tee /tmp/x", "", Confirm},
		{"redirect to arbitrary file", "pkill chrome > /tmp/log", "", Confirm},
		// Note: ProcessPolicy doesn't opine on rm — it only flags
		// kill-like commands. The generic `rm .../` rule lives in
		// DangerousCommandPolicy; see that test suite for rm coverage
		// including paths outside /tmp/.cell-*.done.

		// Block: daemon-kill block runs first; verify ordering with a
		// chain that has an allowed-looking prefix but targets vibecraft.
		{"chain ending in pkill vibecraft", "pkill chrome; pkill vibecraft", "", Block},
		{"chain with && pkill vibecraft", "pgrep chrome && pkill vibecraft", "", Block},
	}
	runPolicy(t, p, cases)
}

func TestDangerousCommandPolicy(t *testing.T) {
	p := &DangerousCommandPolicy{}
	cases := []policyCase{
		// Block: catastrophic + irreversible, OR platform integrity.
		// The product principle: hard-stop for actions that are both
		// not-reversible AND likely to damage the whole setup, plus
		// anything that tampers with VibeCraft itself.
		{"fork bomb", ":(){ :|:& };:", "", Block},
		{"kill init", "kill -9 1", "", Block},
		{"killall -9", "killall -9 -u root", "", Block},

		// Real-disk destruction: irreversible + destroys the boot
		// disk / data disks.
		{"dd to sda", "dd if=/dev/zero of=/dev/sda bs=1M", "", Block},
		{"dd to nvme", "dd if=image of=/dev/nvme0n1 bs=1M", "", Block},
		{"dd to vd (qemu)", "dd if=/dev/zero of=/dev/vda", "", Block},
		{"mkfs on sda partition", "mkfs /dev/sda1", "", Block},
		{"mkfs.ext4 on real disk", "mkfs.ext4 /dev/sdb", "", Block},
		{"fdisk on sda", "fdisk /dev/sda", "", Block},
		{"parted on nvme", "parted /dev/nvme0n1 mklabel gpt", "", Block},
		{"redirect to /dev/sda", "echo x > /dev/sda", "", Block},

		// Wholesale system-tree destruction (path ends at the root).
		{"rm -rf /etc", "rm -rf /etc", "", Block},
		{"rm -rf /usr", "rm -rf /usr", "", Block},
		{"rm -rf / (root)", "rm -rf /", "", Block},
		{"rm -rf /etc with semicolon", "rm -rf /etc; echo done", "", Block},

		// Flag-order-agnostic rm: GNU long-form, interleaved, separate, and
		// --no-preserve-root flags must NOT bypass the block (the old
		// -[rfRF]+ regex anchor missed all of these).
		{"rm long-form root", "rm --recursive --force /", "", Block},
		{"rm -rf --no-preserve-root root", "rm -rf --no-preserve-root /", "", Block},
		{"rm long-form --no-preserve-root", "rm --recursive --force --no-preserve-root /", "", Block},
		{"rm separate flags root", "rm -r -f /", "", Block},
		{"rm reversed short flags root", "rm -fr /", "", Block},
		{"rm long-form /etc", "rm --recursive --force /etc", "", Block},
		{"rm long-form company brain", "rm --recursive --force /home/vibecraft", "", Block},
		{"rm -rf company brain (short)", "rm -rf /home/vibecraft", "", Block},
		{"rm -rf $HOME", "rm -rf $HOME", "", Block},
		{"rm -rf chained after cd", "cd /tmp && rm --recursive --force /", "", Block},

		// Platform integrity — VibeCraft's own infrastructure.
		// Path-anchored: any read/write/delete on these paths is Block.
		{"rm daemon binary", "rm /usr/local/bin/vibecraft-daemon", "", Block},
		{"cat daemon binary", "cat /usr/local/bin/vibecraft-daemon", "", Block},
		{"chmod daemon binary", "chmod 000 /usr/local/bin/vibecraft-daemon", "", Block},
		{"cp over daemon binary", "cp evil /usr/local/bin/vibecraft-daemon", "", Block},
		{"mv onto daemon binary", "mv tmp /usr/local/bin/vibecraft-daemon", "", Block},
		{"truncate daemon binary", "echo > /usr/local/bin/vibecraft-daemon", "", Block},
		{"rm updater script", "rm /usr/local/bin/vibecraft-update.sh", "", Block},
		{"rm daemon service unit", "rm /etc/systemd/system/vibecraft-daemon.service", "", Block},
		{"rm updater service unit", "rm /etc/systemd/system/vibecraft-updater.service", "", Block},
		{"rm updater timer unit", "rm /etc/systemd/system/vibecraft-updater.timer", "", Block},
		{"redirect to Caddyfile", "echo x > /etc/caddy/Caddyfile", "", Block},
		{"cat Caddyfile", "cat /etc/caddy/Caddyfile", "", Block},
		{"vim Caddyfile", "vim /etc/caddy/Caddyfile", "", Block},
		// systemctl/service control on the reverse proxy or the
		// updater. Daemon process kills are caught in ProcessPolicy.
		{"systemctl stop caddy", "systemctl stop caddy", "", Block},
		{"systemctl restart caddy", "systemctl restart caddy", "", Block},
		{"systemctl reload caddy", "systemctl reload caddy", "", Block},
		{"systemctl disable caddy", "systemctl disable caddy", "", Block},
		{"systemctl mask caddy", "systemctl mask caddy", "", Block},
		{"systemctl stop updater", "systemctl stop vibecraft-updater", "", Block},
		{"systemctl disable updater", "systemctl disable vibecraft-updater", "", Block},
		{"service caddy stop (sysvinit style)", "service caddy stop", "", Block},
		{"sudo service caddy stop", "sudo service caddy stop", "", Block},
		{"sudo systemctl stop caddy", "sudo systemctl stop caddy", "", Block},

		// Soft-Deny territory: destructive but bounded, or reversible.
		// These hit Confirm; the classifier may recommend deny and the
		// customer can override with friction.
		{"rm -rf sub-path under /etc", "rm -rf /etc/old-app-config", "", Confirm},
		{"rm -rf sub-path under /var", "rm -rf /var/log/old-stuff", "", Confirm},
		{"rm long-form sub-path of company brain", "rm --recursive --force /home/vibecraft/systems/old", "", Confirm},
		{"dd to loopback", "dd if=/dev/zero of=/dev/loop0", "", Confirm},
		{"mkfs on loopback", "mkfs.ext4 /dev/loop0", "", Confirm},
		{"chmod 777 /", "chmod 777 /", "", Confirm},
		{"shutdown", "sudo shutdown -h now", "", Confirm},
		{"reboot", "sudo reboot", "", Confirm},
		{"systemctl stop sshd", "systemctl stop sshd", "", Confirm},
		{"ufw disable", "sudo ufw disable", "", Confirm},

		// Allow: benign.
		{"ls -la /tmp", "ls -la /tmp", "", Allow},
		{"rm -f local file", "rm -f ./tmp.log", "", Allow},
		{"rm -rf relative build dir", "rm -rf ./build", "", Allow},
		{"rm -rf bare relative dir", "rm -rf node_modules", "", Allow},
		{"dd imaging file", "dd if=/dev/zero of=./zeros bs=1M count=1", "", Allow},

		// Allow: safe recipe chain short-circuits the generic `rm .../`
		// dangerous-pattern check. Without this exemption, the per-cell
		// sync cleanup (`rm -f /tmp/.cell-*.done`) the agent uses in
		// every multi-window recipe would Confirm on every run.
		{"safe recipe: pkill + sleep + rm-cells", "pkill xterm 2>/dev/null; sleep 1; rm -f /tmp/.cell-*.done", "", Allow},
		{"safe recipe: bare rm-cells", "rm -f /tmp/.cell-*.done", "", Allow},
		{"safe recipe: Chrome-close multi-line chain", "pkill -TERM chrome 2>/dev/null\nsleep 2\npgrep chrome >/dev/null && pkill -KILL chrome 2>/dev/null\nsleep 1\npgrep chrome >/dev/null && echo still-alive || echo closed", "", Allow},

		// Non-chain rm paths still Confirm (generic rm rule stays in
		// force for anything outside the safe-recipe allowlist).
		{"rm -f arbitrary /tmp path still confirms", "rm -f /tmp/arbitrary.txt", "", Confirm},
		{"rm /var/log still confirms", "rm /var/log/syslog", "", Confirm},
	}
	runPolicy(t, p, cases)
}

func TestFileSystemPolicy_VibecraftDirectoryBypasses(t *testing.T) {
	p := &FileSystemPolicy{}
	cases := []policyCase{
		// Block: the old regex only matched exact file names; wildcard and
		// read-tool invocations used to slip through.
		{"cat exact token", "cat /etc/vibecraft/daemon.token", "", Block},
		{"cat any file in dir", "cat /etc/vibecraft/anthropic.key", "", Block},
		{"cat OpenAI manager key", "cat /etc/vibecraft/openai.key", "", Block},
		{"strings on enc dir", "strings /etc/vibecraft/vault.enc", "", Block},
		{"ls the dir", "ls /etc/vibecraft/", "", Block},
		{"ls -la dir", "ls -la /etc/vibecraft/", "", Block},
		{"grep recursively", "grep -r secret /etc/vibecraft/", "", Block},
		{"find rooted there", "find /etc/vibecraft/ -type f", "", Block},
		{"xxd binary", "xxd /etc/vibecraft/daemon.token", "", Block},
		{"base64 encode", "base64 /etc/vibecraft/daemon.token", "", Block},
		{"head cred file", "head /etc/vibecraft/anthropic.key", "", Block},
		{"head OpenAI manager key", "head /etc/vibecraft/openai.key", "", Block},
		{"var lib cloud", "cat /var/lib/cloud/instance/user-data.txt", "", Block},

		// Block: daemon-internal SQLite + WAL/SHM siblings.
		{"cat daemon db", "cat /var/lib/vibecraft/vibecraft.db", "", Block},
		{"cat wal", "cat vibecraft.db-wal", "", Block},
		{"cat shm", "cat vibecraft.db-shm", "", Block},
		{"cp daemon db", "cp /var/lib/vibecraft/vibecraft.db /tmp/x", "", Block},

		// Block: anything rooted in /var/lib/vibecraft.
		{"ls daemon dir", "ls /var/lib/vibecraft/", "", Block},
		{"find rooted there", "find /var/lib/vibecraft/ -name '*.db'", "", Block},

		// Block: jwt_public.pem — not secret, but daemon trusts it.
		{"cat jwt_public.pem", "cat /etc/vibecraft/jwt_public.pem", "", Block},
		{"edit jwt_public.pem", "vim /etc/vibecraft/jwt_public.pem", "", Block},

		// Confirm: legitimately-sensitive system files.
		{"cat /etc/shadow", "cat /etc/shadow", "", Confirm},
		{"edit sshd_config", "vim /etc/ssh/sshd_config", "", Confirm},

		// Allow: benign paths.
		{"ls /tmp", "ls /tmp", "", Allow},
		{"cat ~/notes", "cat /home/vibecraft/notes.txt", "", Allow},
	}
	runPolicy(t, p, cases)
}

func TestFileSystemPolicy_CoversCanonicalEditorTool(t *testing.T) {
	p := &FileSystemPolicy{}
	cases := []policyCase{
		{
			name:    "canonical editor blocks manager key",
			command: `{"command":"view","path":"/etc/vibecraft/openai.key"}`,
			toolTyp: "str_replace_based_edit_tool",
			want:    Block,
		},
		{
			name:    "canonical editor blocks daemon db",
			command: `{"command":"view","path":"/var/lib/vibecraft/vibecraft.db"}`,
			toolTyp: "str_replace_based_edit_tool",
			want:    Block,
		},
		{
			name:    "canonical editor confirms sensitive system path",
			command: `{"command":"view","path":"/etc/shadow"}`,
			toolTyp: "str_replace_based_edit_tool",
			want:    Confirm,
		},
	}
	runPolicy(t, p, cases)
}

func TestCredentialAccessPolicy_CoversCanonicalEditorTool(t *testing.T) {
	p := &CredentialAccessPolicy{}
	cases := []policyCase{
		{
			name:    "canonical editor blocks vault",
			command: `{"command":"view","path":"/var/lib/vibecraft/vault.enc"}`,
			toolTyp: "str_replace_based_edit_tool",
			want:    Block,
		},
		{
			name:    "canonical editor blocks encryption key",
			command: `{"command":"view","path":"/etc/vibecraft/encryption.key"}`,
			toolTyp: "str_replace_based_edit_tool",
			want:    Block,
		},
	}
	runPolicy(t, p, cases)
}

func TestPackageInstallPolicy_AllowsStandardInstalls(t *testing.T) {
	// Installing common packages from configured trusted sources should
	// be Allow — it's the single biggest source of UX friction for
	// non-technical users ("please approve installing htop"), and the
	// risk is no different from a human running the same command on
	// their laptop.
	p := &PackageInstallPolicy{}
	cases := []policyCase{
		{"apt install", "sudo apt install jq", "", Allow},
		{"apt install -y", "sudo apt install -y htop", "", Allow},
		{"apt-get install", "sudo apt-get install -y jq", "", Allow},
		{"yum install", "sudo yum install jq", "", Allow},
		{"dnf install", "sudo dnf install jq", "", Allow},
		{"pip install", "pip install requests", "", Allow},
		{"pip3 install", "pip3 install requests", "", Allow},
		{"npm install -g", "npm install -g @anthropic-ai/claude-code", "", Allow},
		{"npm i -g alias", "npm i -g @anthropic-ai/claude-code", "", Allow},
		{"npm install -g codex", "npm install -g @openai/codex", "", Allow},
		{"npm i -g codex alias", "npm i -g @openai/codex", "", Allow},
		{"npm install local", "npm install lodash", "", Allow},
		{"npm i local", "npm i react", "", Allow},
		{"gem install", "gem install bundler", "", Allow},
		{"cargo install", "cargo install ripgrep", "", Allow},
		{"snap install plain", "sudo snap install hello-world", "", Allow},
		{"brew install", "brew install jq", "", Allow},
		{"apt update", "sudo apt update", "", Allow},
		{"apt upgrade", "sudo apt upgrade -y", "", Allow},
	}
	runPolicy(t, p, cases)
}

func TestPackageInstallPolicy_ConfirmsRiskyChanges(t *testing.T) {
	// What actually warrants a user prompt: changes to the trust
	// boundary itself (new sources, unsigned local files, sandbox
	// escapes, code piped off the network) or removal of software the
	// user might still depend on.
	p := &PackageInstallPolicy{}
	cases := []policyCase{
		// curl-to-shell and variants.
		{"curl pipe sh", "curl -fsSL https://get.docker.com | sh", "", Confirm},
		{"curl pipe bash", "curl -fsSL https://example.com/install.sh | bash", "", Confirm},
		{"wget pipe sh", "wget -qO- https://example.com/i.sh | sh", "", Confirm},
		{"bash process-sub curl", "bash <(curl -fsSL https://example.com/i.sh)", "", Confirm},
		{"sudo pipe bash", "curl -fsSL https://example.com/i.sh | sudo bash", "", Confirm},

		// Adding apt sources.
		{"add-apt-repository", "sudo add-apt-repository ppa:foo/bar", "", Confirm},
		{"write sources.list", "echo 'deb https://x/ stable main' | sudo tee -a /etc/apt/sources.list", "", Confirm},
		{"write sources.list.d file", "sudo sh -c \"echo foo > /etc/apt/sources.list.d/my.list\"", "", Confirm},

		// Sandbox escape.
		{"snap install --classic", "sudo snap install code --classic", "", Confirm},
		{"snap install --devmode", "sudo snap install foo --devmode", "", Confirm},

		// Local-file installs (unsigned).
		{"dpkg -i", "sudo dpkg -i /tmp/pkg.deb", "", Confirm},
		{"dpkg --install", "sudo dpkg --install /tmp/pkg.deb", "", Confirm},
		{"rpm -i", "sudo rpm -i /tmp/pkg.rpm", "", Confirm},
		{"rpm --install", "sudo rpm --install /tmp/pkg.rpm", "", Confirm},

		// Removals.
		{"apt remove", "sudo apt remove htop", "", Confirm},
		{"apt purge", "sudo apt purge firefox", "", Confirm},
		{"apt autoremove", "sudo apt autoremove", "", Confirm},
		{"apt-get remove", "sudo apt-get remove nginx", "", Confirm},
		{"snap remove", "sudo snap remove code", "", Confirm},
		{"yum remove", "sudo yum remove nginx", "", Confirm},
		{"dnf erase", "sudo dnf erase nginx", "", Confirm},
	}
	runPolicy(t, p, cases)
}

func TestEngine_Evaluate_MostRestrictiveWins(t *testing.T) {
	// ProcessPolicy Block should dominate over e.g. PackageInstallPolicy
	// Confirm even though both might theoretically match.
	e := &Engine{policies: DefaultPolicies()}
	d := e.Evaluate(Action{Type: "bash", Command: "sudo apt remove nginx && systemctl stop vibecraft-daemon"})
	if d.Action != Block {
		t.Fatalf("expected Block from ProcessPolicy, got %s (rule=%s)", d.Action, d.Rule)
	}
}

func TestEngine_Evaluate_AllowWhenNoPolicyMatches(t *testing.T) {
	e := &Engine{policies: DefaultPolicies()}
	d := e.Evaluate(Action{Type: "bash", Command: "ls -la /tmp"})
	if d.Action != Allow {
		t.Fatalf("expected Allow, got %s (reason=%s)", d.Action, d.Reason)
	}
}

// TestCustomRulePolicy_MatchScopingAndFailClosed covers the two defects
// in CustomRulePolicy.Evaluate: (1) the pattern used to leak across the
// command/type boundary so a command-shaped rule could fire on the bare
// type string, and (2) an uncompilable pattern silently failed OPEN. The
// fix matches command and type as separate fields and Blocks on a compile
// error.
func TestCustomRulePolicy_MatchScopingAndFailClosed(t *testing.T) {
	t.Run("matches on command", func(t *testing.T) {
		p := &CustomRulePolicy{rule: Rule{
			Name: "block-secret-cat", Pattern: `cat\s+/secret`,
			Action: string(Block), Enabled: true,
		}}
		d := p.Evaluate(Action{Type: "bash", Command: "cat /secret/key"})
		if d.Action != Block {
			t.Fatalf("command match: want Block, got %s", d.Action)
		}
	})

	t.Run("matches on type as a separate field", func(t *testing.T) {
		p := &CustomRulePolicy{rule: Rule{
			Name: "confirm-all-computer", Pattern: `^computer$`,
			Action: string(Confirm), Enabled: true,
		}}
		d := p.Evaluate(Action{Type: "computer", Command: "screenshot"})
		if d.Action != Confirm {
			t.Fatalf("type match: want Confirm, got %s", d.Action)
		}
	})

	t.Run("command-shaped pattern does not fire when neither field matches", func(t *testing.T) {
		// "rm" appears in neither the command nor the type, so the rule
		// must NOT fire. Under the old code a non-matching command fell
		// through to a second test against the type string, widening the
		// blast radius; here both fields are checked cleanly.
		p := &CustomRulePolicy{rule: Rule{
			Name: "block-rm", Pattern: `\brm\b`,
			Action: string(Block), Enabled: true,
		}}
		d := p.Evaluate(Action{Type: "bash", Command: "ls -la"})
		if d.Action != Allow {
			t.Fatalf("no-match: want Allow, got %s (rule=%s)", d.Action, d.Rule)
		}
	})

	t.Run("disabled rule always allows", func(t *testing.T) {
		p := &CustomRulePolicy{rule: Rule{
			Name: "disabled", Pattern: `.*`,
			Action: string(Block), Enabled: false,
		}}
		d := p.Evaluate(Action{Type: "bash", Command: "anything"})
		if d.Action != Allow {
			t.Fatalf("disabled rule: want Allow, got %s", d.Action)
		}
	})

	t.Run("invalid regex fails CLOSED (Block)", func(t *testing.T) {
		// An unparseable pattern must not silently disable the rule.
		p := &CustomRulePolicy{rule: Rule{
			Name: "broken", Pattern: `a(b`, // unbalanced group → compile error
			Action: string(Confirm), Enabled: true,
		}}
		d := p.Evaluate(Action{Type: "bash", Command: "whatever"})
		if d.Action != Block {
			t.Fatalf("invalid regex must fail closed with Block, got %s", d.Action)
		}
		if d.Rule != "broken" {
			t.Errorf("fail-closed decision should name the offending rule, got %q", d.Rule)
		}
	})
}
