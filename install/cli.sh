#!/usr/bin/env sh
# SPDX-License-Identifier: Apache-2.0

# VibeCraft CLI installer.
#
# Usage:
#   curl -fsSL https://www.vibecraft.so/install/cli.sh | sh
#   curl -fsSL https://www.vibecraft.so/install/cli.sh | sh -s -- --system   # /usr/local/bin
#   curl -fsSL https://www.vibecraft.so/install/cli.sh | sh -s -- --version=cli-v0.1.0
#
# This script:
#   1. Detects OS + arch (darwin-arm64, darwin-amd64, linux-arm64, linux-amd64).
#   2. Fetches the manifest at https://www.vibecraft.so/install/manifest.json.
#   3. Downloads the matching binary.
#   4. Verifies SHA-256 against the manifest.
#   5. Installs to ~/.local/bin/vibecraft (default) or /usr/local/bin (--system).
#   6. Prints next steps.

set -eu

INSTALL_BASE_URL="${VIBECRAFT_INSTALL_URL:-https://www.vibecraft.so/install}"
DASHBOARD_URL="https://www.vibecraft.so/dashboard"

# Cosign keyless-signing pins. The release pipeline signs every CLI
# binary with the GitHub Actions OIDC identity of the release workflow;
# verification only proves provenance if it pins that exact signer +
# issuer. A wildcard would accept a binary signed by anyone who can get
# a Fulcio certificate. These MUST stay in sync with the cosign sign-blob
# step in .github/workflows/release.yml of the repo the platform serves
# Cosign identity is dual-trust during the OSS-extraction rotation
# window: existing installed CLIs were signed under
# carloslfu/vibecraft-so (the private platform repo where the release
# pipeline ran originally); new releases ship from
# carloslfu/computer.md (the OSS repo where the pipeline lives now).
# Trusting both identities lets in-field CLIs self-update across the
# rotation. After 1-2 weeks of dual-trust uptake (tracked via the
# manifest-fetch install-telemetry signal), the next release tightens
# this regex to the new identity only.
COSIGN_IDENTITY_REGEXP='^https://github\.com/carloslfu/(vibecraft-so|computer\.md)/\.github/workflows/release\.yml@refs/tags/v'
COSIGN_OIDC_ISSUER='https://token.actions.githubusercontent.com'

INSTALL_PATH=""
USE_SYSTEM=0
PINNED_VERSION=""

# ---- helpers ----

die() {
  printf 'error: %s\n' "$1" >&2
  exit 1
}

note() {
  printf '%s\n' "$1"
}

usage() {
  cat <<EOF
VibeCraft CLI installer.

  --system           Install to /usr/local/bin (requires sudo)
  --user             Install to ~/.local/bin (default)
  --version=VERSION  Pin a specific version (default: latest from manifest)
  --help             Show this help

Environment:
  VIBECRAFT_INSTALL_URL  Override the manifest base URL.
EOF
}

# ---- parse args ----

while [ $# -gt 0 ]; do
  case "$1" in
    --system) USE_SYSTEM=1 ;;
    --user)   USE_SYSTEM=0 ;;
    --version=*) PINNED_VERSION="${1#--version=}" ;;
    --help|-h) usage; exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
  shift
done

# ---- detect platform ----

uname_s="$(uname -s)"
uname_m="$(uname -m)"

case "$uname_s" in
  Linux)  os=linux  ;;
  Darwin) os=darwin ;;
  *)      die "unsupported OS: $uname_s (try the manual install — see https://www.vibecraft.so/dashboard)" ;;
esac

case "$uname_m" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *)             die "unsupported arch: $uname_m" ;;
esac

target="${os}-${arch}"
note "Platform: ${target}"

# ---- pick destination ----

if [ "$USE_SYSTEM" -eq 1 ]; then
  INSTALL_PATH="/usr/local/bin/vibecraft"
else
  INSTALL_PATH="${HOME}/.local/bin/vibecraft"
fi

dest_dir="$(dirname "$INSTALL_PATH")"
mkdir -p "$dest_dir" 2>/dev/null || die "cannot create ${dest_dir} (try --system with sudo, or set HOME)"

# ---- fetch manifest ----

manifest_url="${INSTALL_BASE_URL}/manifest.json"
note "Fetching manifest from ${manifest_url}"

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO- "$1"; }
else
  die "need curl or wget to install"
fi

manifest="$(fetch "$manifest_url")" || die "could not fetch manifest"

# ---- parse manifest (POSIX shell + python or jq fallback) ----

binary_url=""
binary_sha=""
version="$PINNED_VERSION"

extract_field() {
  field="$1"
  # First try jq if available (more robust).
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$manifest" | jq -r "${field} // empty"
    return
  fi
  # Fallback: grep + cut on the flat per-target object.
  # Works because the manifest is single-line JSON for each target's url/sha.
  # If your environment has neither jq nor a permissive grep, install jq.
  printf '%s' "$manifest" | grep -oE "\"${field##*.}\":\s*\"[^\"]+\"" | head -1 | sed 's/.*: *"\([^"]*\)"/\1/'
}

if [ -z "$version" ]; then
  if command -v jq >/dev/null 2>&1; then
    version="$(printf '%s' "$manifest" | jq -r '.version // empty')"
  else
    version="$(printf '%s' "$manifest" | grep -oE '"version":[^,]+' | head -1 | sed 's/.*"version":[[:space:]]*"\?\([^",]*\)"\?/\1/')"
  fi
fi

if command -v jq >/dev/null 2>&1; then
  binary_url="$(printf '%s' "$manifest" | jq -r ".binaries.\"${target}\".url // empty")"
  binary_sha="$(printf '%s' "$manifest" | jq -r ".binaries.\"${target}\".sha256 // empty")"
fi

if [ -z "$binary_url" ] || [ -z "$binary_sha" ]; then
  die "manifest missing entry for ${target}; install jq (brew install jq, apt-get install jq) and re-run"
fi

note "Version:  ${version}"
note "Target:   ${target}"

# ---- download to a temp file ----

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT INT TERM HUP
tmp_bin="${tmpdir}/vibecraft"

note "Downloading ${binary_url}"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL --proto '=https' --tlsv1.2 "$binary_url" -o "$tmp_bin" || die "download failed"
else
  wget -q "$binary_url" -O "$tmp_bin" || die "download failed"
fi

# ---- verify SHA-256 ----

note "Verifying SHA-256"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp_bin" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp_bin" | awk '{print $1}')"
else
  die "need sha256sum or shasum to verify download"
fi

if [ "$actual" != "$binary_sha" ]; then
  die "SHA-256 mismatch (expected ${binary_sha}, got ${actual}). Refusing to install."
fi

# ---- verify Cosign signature (best-effort) ----
#
# D2 — keyless signing via GitHub OIDC. The release pipeline uploads a
# .sig + .pem next to every binary, signed by the release workflow's
# GitHub Actions identity. If `cosign` is on PATH we verify the binary
# against the pinned signer identity + issuer above — that pin is what
# proves provenance; a binary signed by any other identity is rejected.
# If `cosign` is not installed the SHA-256 above is the trust boundary:
# it catches transport corruption, but the manifest, binary, and hash
# are same-origin, so it cannot catch a compromised origin. Users
# without cosign therefore lose the signed-provenance proof.
if command -v cosign >/dev/null 2>&1; then
  note "Verifying Cosign signature"
  base="${binary_url%vibecraft-*}"
  asset_name="vibecraft-${target}"
  sig_url="${base}${asset_name}.sig"
  pem_url="${base}${asset_name}.pem"
  sig_tmp="${tmpdir}/cosign.sig"
  pem_tmp="${tmpdir}/cosign.pem"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --tlsv1.2 "$sig_url" -o "$sig_tmp" 2>/dev/null || sig_tmp=""
    curl -fsSL --proto '=https' --tlsv1.2 "$pem_url" -o "$pem_tmp" 2>/dev/null || pem_tmp=""
  fi
  if [ -n "$sig_tmp" ] && [ -n "$pem_tmp" ] && [ -s "$sig_tmp" ] && [ -s "$pem_tmp" ]; then
    if ! COSIGN_EXPERIMENTAL=1 cosign verify-blob \
        --certificate "$pem_tmp" \
        --signature "$sig_tmp" \
        --certificate-identity-regexp "$COSIGN_IDENTITY_REGEXP" \
        --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER" \
        "$tmp_bin" >/dev/null 2>&1; then
      die "Cosign signature verification failed (binary not signed by the VibeCraft release workflow). Refusing to install."
    fi
    note "Cosign verified"
  else
    note "Cosign signature artifacts not present in this release; SHA-256 is the trust boundary."
  fi
else
  note "cosign not installed; skipping signature verification (SHA-256 still verified)."
fi

# ---- install ----

chmod 0755 "$tmp_bin"

if [ -e "$INSTALL_PATH" ]; then
  note "Replacing existing ${INSTALL_PATH}"
fi

# mv is atomic on POSIX (same filesystem). Fall back to cp+rm.
if mv "$tmp_bin" "$INSTALL_PATH" 2>/dev/null; then
  :
else
  cp "$tmp_bin" "$INSTALL_PATH" || die "cannot write ${INSTALL_PATH} (try with sudo, or use --user)"
fi

# ---- final hints ----

if ! command -v vibecraft >/dev/null 2>&1; then
  # If $INSTALL_PATH/.. isn't on PATH, give the user the right command.
  case ":$PATH:" in
    *":$dest_dir:"*) ;;
    *) note ""
       note "Add ${dest_dir} to PATH (e.g. echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc)"
       ;;
  esac
fi

note ""
note "Installed vibecraft ${version} to ${INSTALL_PATH}"
note ""
note "Next:"
note "  1. Connect to a machine:"
note "       ${INSTALL_PATH} auth login"
note "     (or paste an API key from ${DASHBOARD_URL} → Settings → CLI)"
note "  2. Learn the surface:"
note "       ${INSTALL_PATH} docs"
note "  3. Teach your coding agent:"
note "       ${INSTALL_PATH} install-skill"
note ""
note "To remove vibecraft later: ${INSTALL_PATH} uninstall"
