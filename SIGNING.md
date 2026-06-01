<!-- SPDX-License-Identifier: Apache-2.0 -->

# Signing — computer.md release integrity

How computer.md signs its release binaries, and how to operate the signing key.

## What this is

Every binary published by [`.github/workflows/release.yml`](.github/workflows/release.yml)
is signed with an **Ed25519 release key**. Clients (the `vibecraft` CLI's
self-update, the daemon installer) verify the detached `.ed25519.sig` against a
**public key pinned in the source** before trusting a downloaded binary. This
stops a compromised download origin from pushing a forged or unsigned binary —
something the same-origin SHA-256 checksum alone cannot prevent.

The verification is **fail-closed**: [`cli/cmd/verify.go`](cli/cmd/verify.go)
refuses to swap in a binary whose signature does not verify against the pinned
key.

## This key is computer.md's own

computer.md signs and verifies entirely on **its own** Ed25519 key, generated
for and owned by this repository. It is **independent** of any other project's
signing key — there is no shared trust root.

(The sibling standard, db.md, does not use this mechanism at all: it publishes
to crates.io via Trusted Publishing / OIDC with no stored key, plus SLSA
build-provenance attestations on its GitHub release tarballs.)

- **Public key** — pinned in three places, all kept in sync:
  - [`cli/cmd/verify.go`](cli/cmd/verify.go) (`releasePublicKeyPEM`)
  - [`daemon/bootstrap/install.sh`](daemon/bootstrap/install.sh)
  - the two verify blocks in [`.github/workflows/release.yml`](.github/workflows/release.yml)

  ```
  -----BEGIN PUBLIC KEY-----
  MCowBQYDK2VwAyEA+Lcb8IpwuZjZHh6FddfgliKbupMfUSXv4PKCSjBn5mw=
  -----END PUBLIC KEY-----
  ```

- **Private key** — stored as the `RELEASE_SIGNING_KEY` GitHub Actions secret on
  `carloslfu/computer.md` (write-only — GitHub never reveals it again) **and**
  backed up in the maintainer's password manager. Keep the backup (see below).

## Back up the private key — do not skip this

GitHub Actions secrets are **write-only**: once set, the value cannot be read
back by anyone, through any tool (API, CLI, or web UI). If the only copy is the
GitHub secret and you later need the value, it is gone.

So the private key MUST also live in a password manager. If it is lost:

- You can still publish (generate a fresh key — see [Rotate](#rotate-the-key)),
  **but**
- every already-installed client pins the old public key and will **hard-fail**
  on a new-key signature — there is no way to sign a migration release they will
  accept, so they must **reinstall**.

(That exact situation — a release key that exists only in a write-only secret
with no recoverable backup — is what motivated giving computer.md its own,
properly-backed-up key in the first place.)

## Fail-safe at release time

Both signing inputs are fail-safe in `release.yml`:

- If `RELEASE_SIGNING_KEY` is unset, the signing step is a no-op (no `.sig`
  asset) and clients fall back to SHA-256-only — non-bricking.
- If `HOMEBREW_TAP_TOKEN` is unset, the Homebrew formula push is skipped.

So a release still succeeds without these; you only lose the Ed25519 signature
and/or the brew bump for that release.

## Rotate the key

1. Generate a new keypair (use OpenSSL 3.x — macOS's default LibreSSL has weak
   Ed25519 support):
   `openssl genpkey -algorithm ed25519 -out new-release.pem`
2. Derive the public key: `openssl pkey -in new-release.pem -pubout`.
3. Replace the pinned public key in all three locations listed above. Ship this
   as a release **still signed with the OLD key**, so existing clients verify it
   and pick up the new pinned key.
4. THEN set the new private key as the `RELEASE_SIGNING_KEY` secret and start
   signing with it: `gh secret set RELEASE_SIGNING_KEY -R carloslfu/computer.md < new-release.pem`
5. Save the new private key in the password manager.

Skipping step 3 (or having lost the old key) strands already-installed clients
on the old key — they must reinstall.

## Blast radius of the initial key

As of its creation (2026-06-01), **zero** production clients pin this key: the
platform install route defaults to `carloslfu/vibecraft-so` releases
(`GITHUB_RELEASE_REPO || "carloslfu/vibecraft-so"`), and computer.md has
published no releases yet. So this key starts clean — the first computer.md
release establishes it in the field.

## Relationship to VibeCraft

VibeCraft's own release-signing key (the `RELEASE_SIGNING_KEY` secret on
`carloslfu/vibecraft-so`) is a **separate** key for a separate distribution and
is **not touched** by this setup. computer.md does not share, reuse, or depend
on it. The two sign independently.
