# Security

The `computer.md` daemon runs an autonomous AI agent on a computer
that holds customer credentials and operates customer systems. We
take isolation and disclosure seriously.

## Reporting a vulnerability

Email **security@vibecraft.so** with details and reproduction steps.
Do not open a public issue for security problems. We aim to
acknowledge within 2 business days.

## Threat model (summary)

**The trust boundary is the daemon process.** The daemon runs as
its own user (`vibecraft-daemon`) with credentials at
`/etc/vibecraft/` mode 0600. Tools (the agent's own shell, workers,
systems, customer-installed services) run as the `vibecraft` user.
Standard Unix permissions keep tools from reading the daemon's
credentials or killing the daemon process. That is the load-bearing
security boundary: `chmod` + `useradd` hygiene, the same thing
every Linux service does.

**No per-tool sandbox as a product feature.** Tools run on the
operator's machine as the `vibecraft` user with the operator's
permissions, like any other program on Linux. The substrate does
not isolate tools from each other by default; the operator's data
is the operator's data, and tools sharing it is the natural state.

**Defense-in-depth capabilities the daemon ships.** The
`daemon/sandbox/` package ships a bubblewrap-based isolation
mechanism (user/mount/pid/net/ipc/uts namespaces + per-sandbox
egress allowlist + per-sandbox DNS proxy + per-sandbox Xvfb). This
stays in the codebase as a low-level Linux capability the manager
AI can invoke ad-hoc for a specific tool that genuinely needs
isolation (e.g. a hosted-public surface taking external input).
It is **not** the default behavior, not a product concept, and not
something tools opt into via metadata fields. When invoked, what
the mechanism closes is real:

- Secrets at rest unreachable from inside the invoked sandbox
  (`/etc/vibecraft` masked, `/var/lib/vibecraft` and host
  `~/.bashrc` API key absent).
- Outbound exfiltration: default-deny per-sandbox egress allowlist
  (IP + FQDN via a filtering DNS proxy).
- Cross-workload reach: PID/mount/net namespaces; per-sandbox
  Unix socket as the only daemon channel.
- Cross-sandbox X snooping: per-sandbox private Xvfb.

**Multi-tenant deployments** (managed cloud, embedded forks) handle
isolation at the **VM boundary**: each customer gets their own
EC2/Hetzner instance, or the embedding product runs one daemon per
tenant. Per-tool sandboxing inside the VM adds nothing.

**Explicitly out of scope of the daemon-protection boundary**
(governance + separate hardening tracks):

- Prompt-injected within-scope attacker-directed actions
- User-trust misuse via Chrome logins
- A runtime exploit of the daemon itself (the daemon is the trust
  root; defense in depth lives in hardening work tracked separately)

## Supply chain

Release binaries (daemon + CLI) ship signed via:

- **Cosign keyless OIDC** (Sigstore), identity pinned to the
  release-workflow URL.
- **Ed25519 detached signatures** (release-signing key documented
  in the release notes).
- **SLSA build-provenance attestations**, binding each binary to the
  exact commit and workflow run that built it. Verify out-of-band:
  `gh attestation verify <binary> --repo carloslfu/computer.md`.

The CLI verifies the two signatures before swapping in a new binary on
auto-update; the attestation is an independent provenance check anyone
can run by hand. The cosign identity pin is a published commitment:
see Hard Rule #7 in
[plans/open-source-and-computer-md.md](https://github.com/carloslfu/vibecraft-so/blob/main/plans/open-source-and-computer-md.md)
in the upstream private repo for the rotation discipline. Rotating
the identity without a dual-trust release breaks self-update for
the entire install base.

## Daemon-specific attack surface

For the per-component attack inventory (auth flows, JWT validation,
session lifecycle, vault encryption, audit log integrity,
cloud-init handling), see `daemon/SECURITY.md` in this repo.
