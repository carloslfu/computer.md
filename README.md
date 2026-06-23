# computer.md

**The open agentic computer.**

A `COMPUTER.md` file at the root of a machine says what that computer
is for, who it works for, its standing rules, and its tools. A capable
model reads it at the start of every turn and runs the real machine for
you: terminal, browser, filesystem. You direct the machine in plain
language; `COMPUTER.md` is how it knows itself.

That file is the whole idea. `computer.md` (this repo) is the spec for
it plus a reference runtime that reads it:

1. **The spec.** [`SPEC.md`](spec/SPEC.md) defines the `COMPUTER.md`
   file: its recognized sections, its read/write contract, its format.
2. **The reference runtime.** The `daemon/`, `cli/`, and `daemon/web/`
   that read `COMPUTER.md` and run the agent loop, the command surface,
   and the per-machine UI. Apache-2.0. Runs on Linux. The same binary
   VibeCraft ships in its hosted product.

`COMPUTER.md` is the first **open, customer-owned, machine-level,
portable** config for the agentic-computer era: plain markdown,
human-readable, model-read every turn. Anyone can read it, edit it, or
build a runtime that understands it.

## The bet

computer.md is a bold bet, made in the open.

The bet is that frontier models keep getting better, fast. And that the right thing to build is not another app on top of an operating system, but a thin substrate a model can operate directly. Give a capable model a real computer and a few good primitives, and it outperforms any framework you could wrap around it. Every model release makes the substrate stronger, with no new code.

Taken to its conclusion, the agentic computer is what replaces the operating system as the surface you talk to. The OS becomes plumbing. The model becomes the interface. You stop operating the computer. The computer operates itself, for you.

This is what computers should be. computer.md is the open standard for getting there.

## Quick start

The computer is operated by agents, and the installer is text. The quick
start is a prompt you hand to an agent, and it covers both cases: adopting a
machine you already have, with its existing tools, files, and workflows, or
starting fresh. You do not have to decide which. The agent looks at what you
have and proposes the path. It is safe to paste: every binary is built in CI
from a tagged commit and signed, and the install stays fast. Paste this into
Claude Code, Codex, or any agent with a shell:

```text
Read https://raw.githubusercontent.com/carloslfu/computer.md/main/llms.txt
and set up the vibecraft CLI on this machine: install the CLI, load the
reference with `vibecraft docs`, place the vibecraft skill so future sessions
find it, and run `vibecraft auth login` so I can approve access in my browser.
Then meet me where I am: if I already have a machine or a server with tools,
files, and workflows on it, connect it and take all of that into account:
inventory what is there, reflect it in the machine's COMPUTER.md, and bring
any existing notes or docs into its ~/db store, preserving provenance and
verifying nothing was lost; if tools, skills, or workflows here already connect
to that knowledge base, update them to read from ~/db too. Don't leave migration
artifacts unless I ask; git is the audit trail. Show me the plan before you
change anything. If I am starting fresh, walk me through getting a machine and
setting it up. Ask me what I already have if it is not obvious.
```

The agent reads [`llms.txt`](llms.txt), installs the binary, loads the command
reference, places the skill, and waits on the one human step: you approving
CLI access in your browser. From there it either adopts a machine you already
have (inventorying its tools and files, reflecting them in `COMPUTER.md`, and
bringing any existing knowledge base into its `~/db` store) or walks you
through a fresh one. Then it drives your machines: submit tasks, stream
results, read the screen.

Want to confirm it is safe before trusting it? You do not have to verify
anything to install, but you can: [Safe to paste](#safe-to-paste) below has
the receipts and a one-line verify command, and you can ask your agent to run
the audit for you.

Installing by hand is one command on macOS and Linux (on Windows, download
the binary from [Releases](https://github.com/carloslfu/computer.md/releases);
self-update works natively after that):

```bash
curl -fsSL https://www.vibecraft.so/install/cli.sh | sh
# or: brew install carloslfu/tap/vibecraft
# or: download straight from GitHub Releases (no platform route, no install telemetry)
```

To make the CLI stick across agent sessions, place a skill where your
harness reads skills, in the open
[Agent Skills](https://www.anthropic.com/news/skills) format. The canonical
file ships at [`skills/vibecraft/SKILL.md`](skills/vibecraft/SKILL.md): a
thin pointer at `vibecraft docs`, never a copy, so it cannot drift. Copy it
into your harness's skills dir (Claude Code `~/.claude/skills/`, Codex
`~/.codex/skills/`, any other harness's equivalent), use the harness's own
skill installer, or tell the agent to set itself up.

Working with the spec instead? [`spec/SPEC.md`](spec/SPEC.md) defines the
`COMPUTER.md` format. Current spec is **v0.1** (tagged
[`v0.1`](https://github.com/carloslfu/computer.md/releases/tag/v0.1);
additive changes only, see [SPEC.md § Versioning](spec/SPEC.md)). The spec
tooling runs from a clone:

```bash
git clone https://github.com/carloslfu/computer.md
cd computer.md/spec
go run ./cmd/computer-md init --role developer   # generate a COMPUTER.md
go run ./cmd/computer-md validate                # validate one
```

The Go daemon lives in [`daemon/`](daemon/). The CLI lives in [`cli/`](cli/).

### Safe to paste

You do not need to verify anything to install. The install is the fast path
above. But a prompt that ends in an installed binary deserves the option, so
the chain is built to be checked, by you or by the agent you hand it to:

- **The installer is readable.** [`install/cli.sh`](install/cli.sh) is about
  280 lines of POSIX sh: detect the platform, fetch the release manifest,
  download the binary, verify its SHA-256 against the manifest and refuse on
  mismatch, install to `~/.local/bin` with no sudo (`--system` opts into
  `/usr/local/bin`, `--version` pins a release). When `cosign` is on your
  PATH it also verifies the keyless signature and refuses a binary signed by
  anyone but this repo's release workflow.
- **Every binary traces back to source, three ways.** Releases are built in
  CI from version tags, never on a developer's laptop. Each binary ships a
  SHA-256 checksum, an Ed25519 signature against a public key pinned in the
  source (and dry-run verified in CI before the release publishes, see
  [SIGNING.md](SIGNING.md)), a Cosign keyless signature whose identity is
  pinned to this repo's release workflow (Fulcio/Rekor transparency log),
  and a signed build-provenance attestation:

  ```bash
  gh attestation verify vibecraft-<target> --repo carloslfu/computer.md
  ```

- **Self-update is fail-closed.** `vibecraft update` verifies SHA-256 plus
  the Ed25519 signature against the key pinned inside the binary before
  swapping; a download that does not verify is never installed.
- **Telemetry is off by default.** The daemon sends nothing unless you opt
  in, see [Telemetry](#telemetry). The platform install route logs one
  anonymous download event; installing straight from GitHub Releases skips
  even that.
- **Dependencies are continuously audited.** Every pull request runs
  `govulncheck` over the Go modules and fails on a reachable vulnerability;
  Dependabot and Socket watch the Go and npm trees for malware, typosquats,
  and suspicious install scripts.

Do not take the list's word for it. The audit is one more prompt:

```text
Read install/cli.sh and .github/workflows/release.yml in
carloslfu/computer.md and tell me whether this is safe to install.
```

[SIGNING.md](SIGNING.md) documents key custody and rotation;
[SECURITY.md](SECURITY.md) holds the threat model.

## The four deployment shapes

The same daemon binary serves four deployment shapes:

1. **Pure self-host (shape 1).** Clone, build, run on your own box.
   No platform, no telemetry, no auto-update. Auth is first-run
   token + cookie session.
2. **Connected (shape 2, BYOM).** You own the hardware; the daemon
   registers with the platform (`VIBECRAFT_PLATFORM_URL`,
   default `https://www.vibecraft.so`) for notifications, usage
   reporting, and auto-updates. Operator-owned OpenAI key.
3. **Managed (shape 3).** VibeCraft provisions the box (AWS or
   Hetzner). Same daemon binary; VibeCraft's platform handles
   billing and the manager key.
4. **Embedded / commercial fork (shape 4).** Third party ships the
   daemon inside their own product. Overrides `daemon/brand.md`,
   points `VIBECRAFT_PLATFORM_URL` at their own backend, retains
   attribution per the NOTICE file. The license invites this; the
   trademark on "VibeCraft" prevents using the name.

All four run from the same code. See [SPEC.md](spec/SPEC.md) and
[`daemon/SECURITY.md`](daemon/SECURITY.md) for details.

## Repository layout

```
computer.md/
├── spec/                Format spec + parser + role-flavored examples + computer-md CLI
│   ├── SPEC.md
│   ├── parser/          Go reference parser (section extractor)
│   ├── examples/        Role-flavored COMPUTER.md starters
│   └── cmd/computer-md/ The `computer-md init|validate|format|sections` CLI
├── daemon/              The Go agent runtime
│   ├── web/             Vite + React per-machine UI (embedded via go:embed)
│   ├── core/            COMPUTER.md, task queue, system state
│   ├── manager/         OpenAI Responses API client
│   ├── computer/        Mouse/keyboard/screenshot/shell tools
│   ├── vault/           AES-256-GCM secrets vault
│   ├── audit/           Append-only audit log
│   ├── sandbox/         Low-level bwrap capability (invoked ad-hoc, not by default)
│   └── bootstrap/       Daemon installers (install.sh)
├── cli/                 The `vibecraft` CLI (task, chat, screenshot, auth)
├── llms.txt             Agent-readable entry text: the CLI reference an agent reads to install + integrate (served at vibecraft.so/llms.txt; printed by `vibecraft docs`)
├── install/cli.sh       CLI installer (the script vibecraft.so/install/cli.sh serves)
└── HomebrewFormula/     Homebrew tap template for `brew install carloslfu/tap/vibecraft`
```

## License

[Apache-2.0](LICENSE). Patent grant, trademark clause, explicit
modification disclosure. CLA on every PR via CLA Assistant, see
[CONTRIBUTING.md](CONTRIBUTING.md). The CLA preserves the option
to relicense forward (Grafana 2014→2021 pivot pattern) if a
hyperscaler-fork threat ever materializes; the active codebase is
permissive today.

## Telemetry

**Off by default.** The daemon sends nothing to anyone unless you
opt in via `VIBECRAFT_TELEMETRY=on`. When on, the only payload is
a weekly `{version, os, arch}` POST to
`${VIBECRAFT_TELEMETRY_URL:-${VIBECRAFT_PLATFORM_URL}/api/telemetry/v1/ping}`.
The code is OSS; the payload is auditable line by line.

**Install telemetry** is separate: when you download the CLI
via `https://www.vibecraft.so/install/cli.sh`, the platform logs an
anonymous event into its DB (User-Agent + asset name + timestamp;
no IPs, no fingerprints). Bypass it by downloading directly from
GitHub Releases. See [SECURITY.md](SECURITY.md) for the threat model.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow.
Sign the Apache ICLA via the CLA Assistant bot on your first PR.

## Security

Report vulnerabilities to security@vibecraft.so; see
[SECURITY.md](SECURITY.md) for the threat model.

The supply chain is covered in [Safe to paste](#safe-to-paste) above:
CI-built from version tags, SHA-256-checksummed, Ed25519-signed against a
pinned key, Cosign-keyless-signed against the release workflow identity,
provenance-attested, fail-closed self-update, telemetry off by default,
dependencies continuously audited. [SIGNING.md](SIGNING.md) documents key
custody and rotation.

## Related

- **db.md** is the open standard for databases in plain files, the
  natural storage layer for sources and records (atomic data plus
  curator synthesis, tagged by a `meta-type` field) in a
  computer.md computer. An independent standard, usable on its own. See
  its [SPEC.md](https://github.com/carloslfu/db.md/blob/main/SPEC.md) and
  [repo](https://github.com/carloslfu/db.md).
- **AGENTS.md** is the agent-instruction convention computer.md
  composes with. See [agentsmd/agents.md](https://github.com/agentsmd/agents.md).

## Star history

<a href="https://www.star-history.com/#carloslfu/computer.md&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=carloslfu/computer.md&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=carloslfu/computer.md&type=Date" />
   <img alt="Star history chart for carloslfu/computer.md" src="https://api.star-history.com/svg?repos=carloslfu/computer.md&type=Date" />
 </picture>
</a>
