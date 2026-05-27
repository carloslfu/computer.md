#!/usr/bin/env bash
# Renders HomebrewFormula/vibecraft.rb.template into a concrete formula
# using the release tag + the manifest's per-target SHAs. Output is
# printed to stdout.
#
# Usage:
#   ./HomebrewFormula/render.sh <version> <manifest-json-path>
#
# Reads:
#   $1                       — version tag, e.g. v0.50.0
#   $2                       — path to the release manifest.json
#
# Writes:
#   stdout                   — the rendered Ruby formula

set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: render.sh <version> <manifest.json>" >&2
  exit 2
fi

VERSION="$1"
MANIFEST="$2"
TEMPLATE="$(dirname "$0")/vibecraft.rb.template"

if [ ! -f "$TEMPLATE" ]; then
  echo "template not found: $TEMPLATE" >&2
  exit 2
fi
if [ ! -f "$MANIFEST" ]; then
  echo "manifest not found: $MANIFEST" >&2
  exit 2
fi
if ! command -v jq >/dev/null; then
  echo "jq is required" >&2
  exit 2
fi

# The release.yml manifest format keys CLI entries as cli-<target>.
get_url()  { jq -r ".binaries[\"cli-$1\"].url"    < "$MANIFEST"; }
get_sha()  { jq -r ".binaries[\"cli-$1\"].sha256" < "$MANIFEST"; }

# Route brew downloads through the platform install path. Bare
# github.com/.../releases/download/... URLs require auth for private-
# source repos and return 404 to brew, which downloads with no token.
# The platform's /install/<asset> route 302-redirects to a signed
# release-assets URL — brew follows it without needing any auth, and it
# is the same path the `curl | sh` installer uses.
PLATFORM_BASE="${HOMEBREW_FORMULA_BASE:-https://www.vibecraft.so/install}"
to_platform() { printf '%s/%s' "$PLATFORM_BASE" "$(basename "$1")"; }

DARWIN_ARM64_URL=$(to_platform "$(get_url darwin-arm64)")
DARWIN_ARM64_SHA=$(get_sha darwin-arm64)
DARWIN_AMD64_URL=$(to_platform "$(get_url darwin-amd64)")
DARWIN_AMD64_SHA=$(get_sha darwin-amd64)
LINUX_ARM64_URL=$(to_platform "$(get_url linux-arm64)")
LINUX_ARM64_SHA=$(get_sha linux-arm64)
LINUX_AMD64_URL=$(to_platform "$(get_url linux-amd64)")
LINUX_AMD64_SHA=$(get_sha linux-amd64)

sed \
  -e "s|{{VERSION}}|${VERSION}|g" \
  -e "s|{{DARWIN_ARM64_URL}}|${DARWIN_ARM64_URL}|g" \
  -e "s|{{DARWIN_ARM64_SHA}}|${DARWIN_ARM64_SHA}|g" \
  -e "s|{{DARWIN_AMD64_URL}}|${DARWIN_AMD64_URL}|g" \
  -e "s|{{DARWIN_AMD64_SHA}}|${DARWIN_AMD64_SHA}|g" \
  -e "s|{{LINUX_ARM64_URL}}|${LINUX_ARM64_URL}|g" \
  -e "s|{{LINUX_ARM64_SHA}}|${LINUX_ARM64_SHA}|g" \
  -e "s|{{LINUX_AMD64_URL}}|${LINUX_AMD64_URL}|g" \
  -e "s|{{LINUX_AMD64_SHA}}|${LINUX_AMD64_SHA}|g" \
  "$TEMPLATE"
