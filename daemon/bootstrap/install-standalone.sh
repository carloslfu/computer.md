#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
#
# Pure self-host installer (shape 1 in the four-deployment-shapes
# model). No platform integration, no registration token, no machine
# id, no auto-update timer. The daemon runs as a local agent the
# operator owns end-to-end.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/carloslfu/computer.md/main/daemon/bootstrap/install-standalone.sh \
#     | sudo bash -s -- --openai-key sk-...
#
# Requirements:
#   - Linux (Ubuntu/Debian preferred)
#   - sudo
#   - An OpenAI API key for the manager loop
#
# What this installs:
#   - vibecraft-daemon binary in /usr/local/bin/
#   - systemd unit at /etc/systemd/system/vibecraft-daemon.service
#   - /etc/vibecraft/ (root-owned credentials, mode 0600)
#   - /var/lib/vibecraft/ (SQLite db, vault, screenshots)
#   - vibecraft + vibecraft-daemon system users
#
# What this does NOT install:
#   - Chrome / X11 / xdotool (install separately for browser tooling)
#   - Codex CLI / Claude Code CLI workers
#   - Auto-update timer (manual update path: download new binary,
#     systemctl restart vibecraft-daemon)
#
# What this does NOT do:
#   - Register with any platform
#   - Send any telemetry (off by default; opt-in via VIBECRAFT_TELEMETRY=on)
#   - Open inbound network ports (default bind 127.0.0.1)

set -euo pipefail

trap 'rc=$?; echo "" >&2; echo "============================================" >&2; echo "[install-standalone.sh] FAILED at line $LINENO (exit code $rc)" >&2; echo "[install-standalone.sh] last command: $BASH_COMMAND" >&2; echo "============================================" >&2' ERR

OPENAI_KEY=""
DAEMON_PORT="8420"
DAEMON_VERSION="${DAEMON_VERSION:-latest}"
BIND_ADDR="${BIND_ADDR:-127.0.0.1}"
MANAGER_MODEL="${VIBECRAFT_MANAGER_MODEL:-gpt-5.4-mini}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --openai-key)     OPENAI_KEY="$2"; shift 2 ;;
    --port)           DAEMON_PORT="$2"; shift 2 ;;
    --bind)           BIND_ADDR="$2"; shift 2 ;;
    --model)          MANAGER_MODEL="$2"; shift 2 ;;
    --version)        DAEMON_VERSION="$2"; shift 2 ;;
    -h|--help)
      grep '^# ' "$0" | sed 's/^# //'; exit 0 ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
done

if [ "$(id -u)" != 0 ]; then
  echo "install-standalone.sh: run as root (sudo)" >&2
  exit 1
fi

if [ -z "$OPENAI_KEY" ] && [ ! -f /etc/vibecraft/openai.key ]; then
  echo "install-standalone.sh: --openai-key is required on first install"
  echo "  (subsequent runs reuse /etc/vibecraft/openai.key)"
  exit 1
fi

# ─── users ───────────────────────────────────────────────────────────

if ! id vibecraft >/dev/null 2>&1; then
  useradd --system --shell /bin/bash --create-home --home-dir /home/vibecraft vibecraft
fi
if ! id vibecraft-daemon >/dev/null 2>&1; then
  useradd --system --shell /usr/sbin/nologin vibecraft-daemon
fi

# ─── directories ─────────────────────────────────────────────────────

install -d -m 0750 -o vibecraft-daemon -g vibecraft-daemon /etc/vibecraft
install -d -m 0750 -o vibecraft-daemon -g vibecraft-daemon /var/lib/vibecraft
install -d -m 0775 -o vibecraft       -g vibecraft       /home/vibecraft

# ─── credentials ─────────────────────────────────────────────────────

if [ -n "$OPENAI_KEY" ]; then
  printf '%s' "$OPENAI_KEY" > /etc/vibecraft/openai.key
fi
chmod 0600 /etc/vibecraft/openai.key
chown vibecraft-daemon:vibecraft-daemon /etc/vibecraft/openai.key

# Machine ID + manager mode for shape 1 are local-only.
if [ ! -f /etc/vibecraft/machine.id ]; then
  printf 'self-host-%s\n' "$(head -c 6 /dev/urandom | xxd -p)" > /etc/vibecraft/machine.id
  chmod 0644 /etc/vibecraft/machine.id
fi
printf 'operator\n' > /etc/vibecraft/manager_key_mode
chmod 0644 /etc/vibecraft/manager_key_mode

printf '%s\n' "$MANAGER_MODEL" > /etc/vibecraft/manager_model
chmod 0644 /etc/vibecraft/manager_model

# ─── binary ──────────────────────────────────────────────────────────

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *)       echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if [ "$DAEMON_VERSION" = "latest" ]; then
  echo "Pinning to a specific tag — set --version cli-vX.Y.Z to override."
  DAEMON_VERSION="$(curl -fsSL https://api.github.com/repos/carloslfu/computer.md/releases/latest | grep '"tag_name"' | head -1 | cut -d'"' -f4)"
fi

DAEMON_URL="https://github.com/carloslfu/computer.md/releases/download/${DAEMON_VERSION}/vibecraft-daemon-linux-${GOARCH}"

echo "Downloading ${DAEMON_URL}..."
curl -fsSL -o /usr/local/bin/vibecraft-daemon "$DAEMON_URL"
chmod 0755 /usr/local/bin/vibecraft-daemon

# ─── systemd unit ────────────────────────────────────────────────────

cat > /etc/systemd/system/vibecraft-daemon.service <<UNIT
[Unit]
Description=computer.md daemon (pure self-host, shape 1)
Documentation=https://github.com/carloslfu/computer.md
After=network.target

[Service]
Type=simple
User=vibecraft-daemon
Group=vibecraft-daemon
Environment=VIBECRAFT_PORT=${DAEMON_PORT}
Environment=VIBECRAFT_BIND=${BIND_ADDR}
Environment=VIBECRAFT_PLATFORM_URL=
Environment=VIBECRAFT_TELEMETRY=off
ExecStart=/usr/local/bin/vibecraft-daemon
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now vibecraft-daemon

echo ""
echo "Done. Daemon running on ${BIND_ADDR}:${DAEMON_PORT}."
echo ""
echo "Next steps:"
echo "  - Visit http://${BIND_ADDR}:${DAEMON_PORT} to authenticate (first-run token)"
echo "  - Install xterm + chromium + xdotool + xvfb if you want browser tooling"
echo "  - Update: sudo $0 --version cli-vX.Y.Z"
echo "  - Logs:   journalctl -u vibecraft-daemon -f"
