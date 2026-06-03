# computer.md

**The open agentic computer.**

A computer you direct in plain language. A capable model runs the real machine for you: terminal, browser, filesystem. You say what you want; it does the work.

`computer.md` is two things in one repo:

1. **A spec** — [`SPEC.md`](spec/SPEC.md) defines the customer-authored `COMPUTER.md`
   file: who the computer is for, what its standing rules are, what
   tools it has access to.
2. **A reference runtime** — the `daemon/`, `cli/`, and `daemon/web/`
   that ship the agent loop, the command surface, and the per-machine
   UI. Apache-2.0. Runs on Linux. The same binary VibeCraft ships in
   its hosted product.

A `COMPUTER.md` file is the first **open, customer-owned,
machine-level, portable state primitive** for the agentic-computer
era. The format is plain markdown — anyone can read it, edit it, or
build a runtime that understands it.

## The bet

computer.md is a bold bet, made in the open.

The bet is that frontier models keep getting better, fast. And that the right thing to build is not another app on top of an operating system, but a thin substrate a model can operate directly. Give a capable model a real computer and a few good primitives, and it outperforms any framework you could wrap around it. Every model release makes the substrate stronger, with no new code.

Taken to its conclusion, the agentic computer is what replaces the operating system as the surface you talk to. The OS becomes plumbing. The model becomes the interface. You stop operating the computer. The computer operates itself, for you.

This is what computers should be. computer.md is the open standard for getting there.

## Quick start

```bash
# Install the CLI (works on macOS, Linux, Windows)
curl -fsSL https://www.vibecraft.so/install/cli.sh | sh

# Generate a COMPUTER.md (replace 'developer' with your role)
computer-md init --role developer

# Validate a COMPUTER.md
computer-md validate
```

The spec, the parser, and the example role files live in [`spec/`](spec/SPEC.md).
The current spec is **v0.1** (tagged [`v0.1`](https://github.com/carloslfu/computer.md/releases/tag/v0.1); additive changes only — see [SPEC.md § Versioning](spec/SPEC.md)).
The Go daemon lives in [`daemon/`](daemon/). The CLI lives in [`cli/`](cli/).

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
├── install/cli.sh       CLI installer (the script vibecraft.so/install/cli.sh serves)
└── HomebrewFormula/     Homebrew tap template for `brew install carloslfu/tap/vibecraft`
```

## License

[Apache-2.0](LICENSE). Patent grant, trademark clause, explicit
modification disclosure. CLA on every PR via CLA Assistant — see
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
[SECURITY.md](SECURITY.md) for the threat model. Two things worth
knowing if you run this:

**Releases are verifiable.** Every binary is built in CI,
SHA256-checksummed, and signed with computer.md's Ed25519 release key,
which `install.sh` and the CLI self-updater check against a pinned
public key (plus cosign keyless signatures and build-provenance
attestations). See [SIGNING.md](SIGNING.md).

**Dependencies are continuously audited.** Every pull request runs
`govulncheck` over the Go modules and fails on a reachable
vulnerability; the Go and web (npm) dependency trees are also watched
by GitHub Dependabot and Socket supply-chain scanning (malware,
typosquats, suspicious install scripts).

## Related

- **db.md** — the open database in plain files, the natural storage
  layer for a computer.md computer. An independent standard, usable
  on its own. See its
  [SPEC.md](https://github.com/carloslfu/db.md/blob/main/SPEC.md) and
  [repo](https://github.com/carloslfu/db.md).
- **AGENTS.md** — the agent-instruction convention computer.md
  composes with. See [agentsmd/agents.md](https://github.com/agentsmd/agents.md).
