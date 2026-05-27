# Contributing to computer.md

Thanks for thinking about contributing. Three things to know up
front:

1. **License is Apache-2.0.** Everything you contribute lands under
   Apache-2.0 (with patent grant). The trademark on "VibeCraft"
   stays with VibeCraft Inc.
2. **CLA is required.** First-time PRs trigger the CLA Assistant
   bot. Sign the Apache ICLA via the comment workflow it prompts.
   This preserves the project's option to relicense forward if the
   ecosystem ever requires it; without the CLA we'd be locked into
   the current license forever.
3. **The boundary between this repo and the VibeCraft platform is
   HTTPS.** The OSS daemon talks to the platform via env-overridable
   URLs (`VIBECRAFT_PLATFORM_URL`, etc.). No Go imports cross the
   boundary in either direction. Keep it that way.

## Development setup

```bash
# Clone
git clone https://github.com/carloslfu/computer.md.git
cd computer.md

# Daemon (Go, requires Go 1.22+, CGO for SQLCipher)
cd daemon
make build                          # builds web/dist first, then go binary
make test

# CLI
cd ../cli
make build
make test

# Spec parser + computer-md CLI
cd ../spec
make build
make test
```

## What to work on

- **Bugs**: file an issue first, then PR with a test.
- **Spec extensions**: SPEC.md additions are additive only — propose
  a new optional `##` section, not a change to existing semantics.
- **Reference runtime improvements**: focus on the daemon, the CLI,
  or the per-machine UI under `daemon/web/`.
- **Documentation**: README, SPEC.md, examples, code comments. Be
  concrete — show, don't claim.

## What we won't merge

- **Productized features that the manager AI can build itself with
  the primitives it already has.** The repo is intentionally a thin
  harness; if the addition can be authored on a running computer
  in 30 minutes of conversation, it doesn't need to live in the
  binary.
- **DOM-access framework integrations.** No Playwright, no Puppeteer,
  no raw CDP, no Chrome extension, no AT-SPI. The browser is driven
  by pixel ops on real Chrome. This is load-bearing for the
  "looks-human" property. See PRODUCT.md in the upstream VibeCraft
  repo for the full reasoning.
- **Per-tool sandbox features.** Tools run as the `vibecraft` user
  with the operator's permissions, like any program on Linux. The
  shipped bwrap mechanism is a low-level Linux capability the AI
  invokes ad-hoc when a specific tool genuinely needs isolation;
  it is not a product concept.

## Style

- **Code comments**: only when the WHY is non-obvious. Don't explain
  WHAT the code does; the code already does that.
- **Commit messages**: imperative voice, under 72 chars on the
  subject line. Body if non-trivial.
- **PR titles**: short and concrete. *"Fix race in task queue
  shutdown"* beats *"Improvements to the queue"*.
- **Tests**: every behavior change needs a test. Use the existing
  test-helper patterns in each module.

## Pre-PR checklist

- [ ] Tests pass (`make test` in each module you touched)
- [ ] No `// TODO` comments added without an issue number
- [ ] No secrets in code, tests, or fixtures
- [ ] `SPDX-License-Identifier: Apache-2.0` header on any new source
      file (run `scripts/add-spdx-headers.sh` if you forget)
- [ ] CLA signed via the bot on your first PR

## Reporting security issues

**Do not file public issues for security problems.** Email
security@vibecraft.so. See [SECURITY.md](SECURITY.md).

## Code of conduct

Be kind. Disagreement is fine; disrespect is not. We follow the
[Contributor Covenant 2.1](https://www.contributor-covenant.org/version/2/1/code_of_conduct/).
