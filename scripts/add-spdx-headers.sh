#!/usr/bin/env bash
# Add `SPDX-License-Identifier: Apache-2.0` to source files that don't
# already have an SPDX header. Idempotent — safe to run repeatedly.
#
# Skipped:
#   - generated files (*_gen.go, embedded assets)
#   - vendored dependencies (node_modules, vendor/)
#   - the daemon/web/dist build output
#   - files that already contain an SPDX-License-Identifier line
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

add_header() {
  local file="$1"
  local comment="$2"   # "//" for Go/TS/JS, "#" for shell, etc.
  local header="${comment} SPDX-License-Identifier: Apache-2.0"

  if grep -q 'SPDX-License-Identifier' "$file" 2>/dev/null; then
    return 0
  fi

  # Preserve shebang on shell scripts.
  if head -1 "$file" | grep -q '^#!'; then
    local first_line rest
    first_line="$(head -1 "$file")"
    rest="$(tail -n +2 "$file")"
    {
      printf '%s\n%s\n\n%s\n' "$first_line" "$header" "$rest"
    } > "$file.tmp"
  else
    {
      printf '%s\n\n' "$header"
      cat "$file"
    } > "$file.tmp"
  fi
  mv "$file.tmp" "$file"
  echo "  + $file"
}

# Source files where a `//` comment is correct.
while IFS= read -r -d '' f; do
  add_header "$f" "//"
done < <(
  find daemon cli spec -type f \( -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.js' -o -name '*.jsx' \) \
    ! -path '*/node_modules/*' \
    ! -path '*/dist/*' \
    ! -path '*/vendor/*' \
    ! -name '*_gen.go' \
    -print0
)

# Shell scripts and Bash/POSIX files.
while IFS= read -r -d '' f; do
  add_header "$f" "#"
done < <(
  find daemon cli spec install scripts -type f \( -name '*.sh' \) \
    ! -path '*/node_modules/*' \
    -print0
)

# Homebrew formula template.
for f in HomebrewFormula/*.rb HomebrewFormula/*.rb.template; do
  [ -f "$f" ] && add_header "$f" "#"
done

echo "done."
