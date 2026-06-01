#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Diagnostic trap. Under 'set -e' a failing command exits the script
# immediately with no message — historically users just saw an SSH
# session close and had to dig in to figure out which step died. This
# trap prints the line number and command before we exit.
trap 'rc=$?; echo "" >&2; echo "============================================" >&2; echo "[install.sh] FAILED at line $LINENO (exit code $rc)" >&2; echo "[install.sh] last command: $BASH_COMMAND" >&2; echo "[install.sh] If this looks like a bug, file an issue with the output above." >&2; echo "============================================" >&2' ERR

# VibeCraft BYOM install script
# Usage: curl -sfL https://www.vibecraft.so/install.sh | sudo bash -s -- \
#   --token REG_TOKEN --machine MACHINE_ID --openai-key sk-...
#
# On retries where /etc/vibecraft/openai.key already exists, you may
# omit --openai-key and the existing operator-owned key file will be reused.
#
# The Codex worker is installed by default and authenticates through
# its own `codex login` (ChatGPT subscription) the first time the
# customer uses it. Claude Code uses its own subscription/session state too.

PLATFORM_URL="https://www.vibecraft.so"
TOKEN=""
MACHINE_ID=""
OPENAI_KEY=""

# Parse flags
while [[ $# -gt 0 ]]; do
  case $1 in
    --token) TOKEN="$2"; shift 2 ;;
    --machine) MACHINE_ID="$2"; shift 2 ;;
    --openai-key) OPENAI_KEY="$2"; shift 2 ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
done

if [ -z "$TOKEN" ] || [ -z "$MACHINE_ID" ]; then
  echo "Usage: install.sh --token TOKEN --machine MACHINE_ID [--openai-key KEY]"
  echo ""
  echo "  --openai-key is required unless /etc/vibecraft/openai.key already exists."
  exit 1
fi

if [ -z "$OPENAI_KEY" ] && [ ! -f /etc/vibecraft/openai.key ]; then
  echo "Error: --openai-key is required (no existing /etc/vibecraft/openai.key)"
  exit 1
fi

# ── Preflight checks ──────────────────────────────────────────

if [ "$(id -u)" -ne 0 ]; then
  echo "Error: This script must be run as root (use sudo)"
  exit 1
fi

# Check Ubuntu version
if command -v lsb_release &>/dev/null; then
  DISTRO=$(lsb_release -is 2>/dev/null || echo "unknown")
  VERSION=$(lsb_release -rs 2>/dev/null || echo "0")
  MAJOR_VERSION=$(echo "$VERSION" | cut -d. -f1)
  if [ "$DISTRO" != "Ubuntu" ] || [ "$MAJOR_VERSION" -lt 22 ]; then
    echo "Warning: This script is designed for Ubuntu 22.04+. You're running $DISTRO $VERSION."
    echo "Proceeding anyway, but some packages may not install correctly."
  fi
else
  echo "Warning: Cannot detect OS version. Proceeding anyway."
fi

# Detect public IPv4. We force -4 because the registration endpoint only
# accepts IPv4, and dual-stack hosts often resolve to IPv6 by default.
echo "Detecting public IPv4..."
PUBLIC_IP=$(curl -4sf --max-time 10 ifconfig.me || curl -4sf --max-time 10 icanhazip.com || echo "")
if [ -z "$PUBLIC_IP" ]; then
  echo "Error: Could not detect a public IPv4 address."
  echo "This host needs an IPv4 interface reachable from the internet."
  echo "(If you have only IPv6, the platform does not yet support IPv6-only daemons.)"
  exit 1
fi

# Validate the result looks like a dot-quad before continuing.
if ! [[ "$PUBLIC_IP" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
  echo "Error: Detected IP '$PUBLIC_IP' is not a valid IPv4 address."
  exit 1
fi
echo "Public IPv4: $PUBLIC_IP"

# Check UFW status
if command -v ufw &>/dev/null; then
  UFW_STATUS=$(ufw status 2>/dev/null | head -1 || echo "")
  if echo "$UFW_STATUS" | grep -q "active"; then
    UFW_80=$(ufw status 2>/dev/null | grep -E "80/tcp.*ALLOW" || echo "")
    UFW_443=$(ufw status 2>/dev/null | grep -E "443/tcp.*ALLOW" || echo "")
    if [ -z "$UFW_80" ] || [ -z "$UFW_443" ]; then
      echo ""
      echo "WARNING: UFW is active and ports 80/443 may not be open."
      echo "Run: sudo ufw allow 80/tcp && sudo ufw allow 443/tcp"
      echo ""
    fi
  fi
fi

echo ""
echo "Installing VibeCraft daemon on this machine..."
echo "Machine ID: $MACHINE_ID"
echo ""

# ── Install packages ──────────────────────────────────────────

echo "Installing system packages..."
export DEBIAN_FRONTEND=noninteractive

# Wait up to 60s for any in-flight apt (unattended-upgrades, a manual
# apt the user happens to have running) to release the lock before
# we hammer it. A deadlock here turns into "the installer hangs
# forever" with no diagnosable signal. Mirrors the cloud-init.ts
# cold-AMI apt-lock preamble.
for _i in $(seq 1 30); do
  if [ -z "$(fuser /var/lib/dpkg/lock-frontend /var/lib/apt/lists/lock /var/lib/dpkg/lock 2>/dev/null)" ]; then
    break
  fi
  sleep 2
done

timeout 120 apt-get update -qq

# Keep this list aligned with lib/cloud-init.ts. Both paths drop a
# customer onto the same machine shape — the agent's prompt assumes a
# specific set of tools (xterm, lynx, pandoc, pdftotext, claude, etc.)
# and a BYOM that's missing them turns into a debugging session.
#
# Critical entries to never drop:
#   - xterm        the ONLY terminal the agent and Claude Code render
#                  reliably in. Without it, Ubuntu sometimes pulls
#                  zutty in as a transitive dep, the agent picks it,
#                  and Claude Code's TUI paints as garbled color bars.
#                  Fixed in cloud-init.ts in v0.15.2 (commit 45048da);
#                  this is the matching BYOM fix.
#   - nodejs/npm   for the interactive Claude Code worker.
#   - lynx/pandoc/python3-bs4/poppler-utils  text extraction tools the
#                  prompt tells the agent to reach for instead of
#                  scrolling Chrome.
# Hard-bound the big package install so a stalled apt mirror fails
# loudly rather than hanging the installer indefinitely.
timeout 600 apt-get install -y -qq \
  curl wget git jq tree htop ncdu tmux vim nano less file man-db rsync zip unzip \
  net-tools dnsutils ca-certificates gnupg build-essential \
  python3 python3-pip python3-venv python3-bs4 \
  nodejs npm \
  lynx pandoc poppler-utils \
  xvfb fluxbox x11-xserver-utils xdotool wmctrl scrot xterm \
  unattended-upgrades \
  bubblewrap tini nftables uidmap iproute2 \
  >/dev/null

# Phase-3 per-sandbox scheduler: supercronic (pinned + SHA256-verified).
# NOT Debian cron — bwrap's --unshare-user sets setgroups=deny and Vixie
# cron's per-job setgroups() EPERMs, killing jobs before exec (proven on
# a real kernel). supercronic runs jobs as the sandbox identity with no
# privilege-drop, so it works under full user-ns isolation.
if ! [ -x /usr/local/bin/supercronic ]; then
  curl -sfL https://github.com/aptible/supercronic/releases/download/v0.2.45/supercronic-linux-amd64 -o /tmp/supercronic
  echo "bb6da5af8d5547c9a5cbb4cf58d9f5541f0433df2188bfe4f1a54b04ad253db6  /tmp/supercronic" | sha256sum -c -
  install -m 0755 /tmp/supercronic /usr/local/bin/supercronic
  rm -f /tmp/supercronic
fi

# Chrome. The wrapper lives in /usr/bin, but the actual binary and
# resources live under /opt/google; verify both so a partial install
# cannot pass.
if ! command -v google-chrome-stable &>/dev/null || ! [ -x /opt/google/chrome/google-chrome ]; then
  echo "Installing Google Chrome..."
  wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg
  echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
  timeout 120 apt-get update -qq && timeout 300 apt-get install -y -qq google-chrome-stable >/dev/null
fi

# Caddy
if ! command -v caddy &>/dev/null; then
  echo "Installing Caddy..."
  apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https >/dev/null
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi

# ── Create vibecraft user ─────────────────────────────────────

if ! id vibecraft &>/dev/null; then
  useradd -m -s /bin/bash vibecraft
fi

# ── Worker-agent CLIs (Claude Code + Codex) ─────────────────
# Same shape as the Managed path in lib/cloud-init.ts: install under
# the vibecraft user so the npm prefix + ~/.claude / ~/.codex sessions
# survive daemon restarts and machine reboots.
#
# Auth fallback (BYOM specifically): once the binaries are installed,
# the customer can run `claude` or `codex login` interactively; each
# CLI's session directory persists across daemon restarts.
echo "Installing worker agents (Claude Code + Codex)..."
su - vibecraft -c "mkdir -p ~/.npm-global ~/.claude ~/.codex"
su - vibecraft -c "npm config set prefix ~/.npm-global"
if ! grep -q '.npm-global/bin' /home/vibecraft/.bashrc 2>/dev/null; then
  echo 'export PATH=$HOME/.npm-global/bin:$PATH' >> /home/vibecraft/.bashrc
fi
# Try user-prefix first; fall back to the system npm so we don't fail
# the whole install over a transient registry hiccup. Hard-bound so a
# cold npm registry / proxy stall can't wedge the installer — the
# daemon's bootstrap.go ensureWorkerAgents retries on first start.
timeout 180 su - vibecraft -c "~/.npm-global/bin/npm i -g @anthropic-ai/claude-code 2>/dev/null || npm i -g @anthropic-ai/claude-code 2>/dev/null" || true
timeout 180 su - vibecraft -c "~/.npm-global/bin/npm i -g @openai/codex 2>/dev/null || npm i -g @openai/codex 2>/dev/null" || true
# Provider API keys are never written to ~/.bashrc or worker env. Strip
# any line an older install left behind. Idempotent.
sed -i '/^export ANTHROPIC_API_KEY=/d' /home/vibecraft/.bashrc 2>/dev/null || true
sed -i '/^export OPENAI_API_KEY=/d' /home/vibecraft/.bashrc 2>/dev/null || true
chown vibecraft:vibecraft /home/vibecraft/.bashrc 2>/dev/null || true

# ── Write config files ────────────────────────────────────────

echo "Writing configuration..."
mkdir -p /etc/vibecraft
chmod 700 /etc/vibecraft

# Pinned release-signing public key (Phase 6). Public — used by the
# updater to verify the daemon's Ed25519 signature before any swap.
cat > /etc/vibecraft/release_pub.pem << 'PUBEOF'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA+Lcb8IpwuZjZHh6FddfgliKbupMfUSXv4PKCSjBn5mw=
-----END PUBLIC KEY-----
PUBEOF
chmod 644 /etc/vibecraft/release_pub.pem

# Remote audit sink (Phase 6). BYOM is opt-in: ship "off"; the customer
# flips it to "on" to stream audit off-machine (their machine, their
# choice).
echo -n "off" > /etc/vibecraft/audit_sink
chmod 644 /etc/vibecraft/audit_sink

echo -n "$MACHINE_ID" > /etc/vibecraft/machine.id
if [ -n "$OPENAI_KEY" ]; then
  echo -n "$OPENAI_KEY" > /etc/vibecraft/openai.key
  chmod 600 /etc/vibecraft/openai.key
else
  echo "Reusing existing /etc/vibecraft/openai.key"
fi
echo -n "operator" > /etc/vibecraft/manager_key_mode
chmod 600 /etc/vibecraft/manager_key_mode
echo -n "$PUBLIC_IP" > /etc/vibecraft/public-ip

# Data directory
mkdir -p /var/lib/vibecraft
chmod 700 /var/lib/vibecraft

# ── Download daemon binary ────────────────────────────────────

echo "Downloading VibeCraft daemon..."
MANIFEST=$(curl -sf "$PLATFORM_URL/api/releases/manifest")
DAEMON_URL=$(echo "$MANIFEST" | jq -r '.daemon.url')
EXPECTED_CHECKSUM=$(echo "$MANIFEST" | jq -r '.daemon.checksum')

curl -sfL "$DAEMON_URL" -o /tmp/vibecraft-daemon
ACTUAL_CHECKSUM=$(sha256sum /tmp/vibecraft-daemon | cut -d' ' -f1)

if [ "$EXPECTED_CHECKSUM" != "$ACTUAL_CHECKSUM" ]; then
  echo "Error: Daemon binary checksum mismatch"
  rm -f /tmp/vibecraft-daemon
  exit 1
fi

# Phase 6 signed releases: verify Ed25519 signature before install.
# Present-but-invalid is fatal; absent (older manifest) → SHA256-only.
SIG_URL=$(echo "$MANIFEST" | jq -r '.daemon.sig // empty')
if [ -n "$SIG_URL" ]; then
  curl -sfL "$SIG_URL" -o /tmp/vibecraft-daemon.sig
  if ! openssl pkeyutl -verify -pubin -inkey /etc/vibecraft/release_pub.pem \
       -rawin -in /tmp/vibecraft-daemon -sigfile /tmp/vibecraft-daemon.sig >/dev/null; then
    echo "Error: Daemon release signature invalid"
    rm -f /tmp/vibecraft-daemon /tmp/vibecraft-daemon.sig
    exit 1
  fi
  rm -f /tmp/vibecraft-daemon.sig
fi

mv /tmp/vibecraft-daemon /usr/local/bin/vibecraft-daemon
chmod +x /usr/local/bin/vibecraft-daemon

# ── Complete registration ─────────────────────────────────────

echo "Registering with VibeCraft platform..."
# Capture body and status separately so we can show a real error message
# under 'set -e' (a failing 'curl -sf' inside command substitution would
# exit the script silently before any 'if [ $? ]' check could run).
REG_BODY=$(mktemp)
REG_STATUS=$(curl -sS --max-time 30 -X POST \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"machineId\":\"${MACHINE_ID}\",\"ip\":\"${PUBLIC_IP}\"}" \
  -o "$REG_BODY" -w "%{http_code}" \
  "$PLATFORM_URL/api/machines/register/complete" || echo "000")

if [ "$REG_STATUS" != "200" ]; then
  echo "Error: Registration failed (HTTP $REG_STATUS)"
  echo "Response body:"
  cat "$REG_BODY"
  echo ""
  rm -f "$REG_BODY"
  exit 1
fi

RESPONSE=$(cat "$REG_BODY")
rm -f "$REG_BODY"

MACHINE_HOST=$(echo "$RESPONSE" | jq -r '.machineHost')
MACHINE_NAME=$(echo "$RESPONSE" | jq -r '.machineName // empty')
JWT_PUBLIC_KEY=$(echo "$RESPONSE" | jq -r '.jwtPublicKey')
HEALTH_TOKEN=$(echo "$RESPONSE" | jq -r '.healthToken')

if [ -z "$MACHINE_HOST" ] || [ "$MACHINE_HOST" = "null" ]; then
  echo "Error: Registration response missing machineHost"
  echo "Full response: $RESPONSE"
  exit 1
fi

echo -n "$MACHINE_HOST" > /etc/vibecraft/machine.host
if [ -n "$MACHINE_NAME" ]; then
  echo -n "$MACHINE_NAME" > /etc/vibecraft/machine.name
  chmod 644 /etc/vibecraft/machine.name
fi
echo -n "$JWT_PUBLIC_KEY" > /etc/vibecraft/jwt_public.pem
chmod 644 /etc/vibecraft/jwt_public.pem
echo -n "$HEALTH_TOKEN" > /etc/vibecraft/health.token
chmod 600 /etc/vibecraft/health.token

# Phase 0b: per-machine local token gating the daemon's localhost-only
# endpoints. BYOM generates this locally — the platform never sees it
# (Decision D2 / install.sh path). Generate-if-absent so install retries
# and existing BYOM machines self-heal without rotating a working token.
if [ ! -s /etc/vibecraft/local.token ]; then
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/vibecraft/local.token
fi
chmod 600 /etc/vibecraft/local.token

# ── vc-spawn-worker (Phase 1, D3 explicit helper) ──
# Inert until VIBECRAFT_SANDBOXED_WORKERS is flipped (daemon runs the
# worker legacy-style otherwise). Safe to ship now.
cat > /usr/local/bin/vc-spawn-worker <<'VCSW'
#!/bin/bash
set -euo pipefail
NAME=""; SYS=""; ALLOW=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="$2"; shift 2;;
    --system) SYS="$2"; shift 2;;
    --allow) ALLOW="$2"; shift 2;;
    --) shift; break;;
    *) echo "vc-spawn-worker: unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$NAME" ] || { echo "vc-spawn-worker: --name required" >&2; exit 2; }
[ $# -gt 0 ] || { echo "vc-spawn-worker: command required after --" >&2; exit 2; }
CMD_JSON=$(printf '%s\n' "$@" | jq -R . | jq -s .)
ALLOW_JSON=$(printf '%s' "$ALLOW" | jq -R 'split(",")|map(select(length>0))')
BODY=$(jq -nc --arg n "$NAME" --arg s "$SYS" --argjson c "$CMD_JSON" --argjson a "$ALLOW_JSON" \
  '{name:$n,system:$s,cmd:$c,allow_fqdns:$a}')
curl -fsS -X POST --unix-socket /run/vibecraft.sock http://daemon/api/daemon/spawn-worker \
  -H 'Content-Type: application/json' -d "$BODY"
VCSW
chmod 755 /usr/local/bin/vc-spawn-worker

# ── Configure Caddy ───────────────────────────────────────────

echo "Configuring Caddy..."
mkdir -p /etc/caddy
cat > /etc/caddy/Caddyfile << CADDYEOF
{
  on_demand_tls {
    ask http://localhost:8420/routes/verify
  }
}

${MACHINE_HOST} {
  reverse_proxy localhost:8420
}
CADDYEOF

# ── Set up systemd services ──────────────────────────────────

echo "Setting up services..."

cat > /etc/systemd/system/vibecraft-daemon.service << 'SERVICEEOF'
[Unit]
Description=VibeCraft Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/vibecraft-daemon
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SERVICEEOF

# Updater
# Runs every 5 minutes via vibecraft-updater.timer. Two responsibilities:
#   1. Roll the daemon binary forward to the latest release.
#   2. Self-heal the baseline package set — idempotent checks for the
#      tools the agent prompt assumes (xterm, claude). If any are
#      missing (older BYOM install that predates them, or someone
#      apt-removed a tool), reinstall them. This is the BYOM mirror of
#      the Claude Code rollout block in lib/cloud-init.ts's updater.
cat > /usr/local/bin/vibecraft-update.sh << 'UPDATEEOF'
#!/bin/bash
set -euo pipefail

# ── 1. Daemon binary rollout ─────────────────────────────────
MANIFEST=$(curl -sf https://www.vibecraft.so/api/releases/manifest) || exit 0
LATEST=$(echo "$MANIFEST" | jq -r '.daemon.version')
CURRENT=$(vibecraft-daemon --version 2>/dev/null || echo "unknown")
if [ "$CURRENT" != "$LATEST" ]; then
    URL=$(echo "$MANIFEST" | jq -r '.daemon.url')
    EXPECTED=$(echo "$MANIFEST" | jq -r '.daemon.checksum')
    curl -sfL "$URL" -o /tmp/vibecraft-daemon
    ACTUAL=$(sha256sum /tmp/vibecraft-daemon | cut -d' ' -f1)
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        rm -f /tmp/vibecraft-daemon
    else
        # Phase 6 signed releases: verify before swap. SHA256 stays the
        # hard gate; signature engages only when the manifest carries
        # `.daemon.sig` (older manifests → SHA256-only, non-bricking).
        SIG_URL=$(echo "$MANIFEST" | jq -r '.daemon.sig // empty')
        SIG_OK=1
        if [ -n "$SIG_URL" ]; then
            if curl -sfL "$SIG_URL" -o /tmp/vibecraft-daemon.sig \
               && openssl pkeyutl -verify -pubin \
                    -inkey /etc/vibecraft/release_pub.pem -rawin \
                    -in /tmp/vibecraft-daemon \
                    -sigfile /tmp/vibecraft-daemon.sig >/dev/null; then
                SIG_OK=1
            else
                SIG_OK=0
            fi
            rm -f /tmp/vibecraft-daemon.sig
        fi
        if [ "$SIG_OK" != "1" ]; then
            echo "vibecraft-update: release signature INVALID — refusing swap" >&2
            rm -f /tmp/vibecraft-daemon
        else
            mv /tmp/vibecraft-daemon /usr/local/bin/vibecraft-daemon
            chmod +x /usr/local/bin/vibecraft-daemon
            systemctl restart vibecraft-daemon
        fi
    fi
fi

# ── 2. xterm self-heal (existing-machine path) ───────────────
# Older BYOM installs (before this fix) shipped without xterm. Without
# it the agent falls back to whatever terminal apt happened to pull
# in transitively — often zutty, which renders Claude Code's TUI as
# garbled color bars. Idempotent: a no-op once xterm is on disk.
if ! command -v xterm >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xterm >/dev/null || true
fi

# ── 2a. Chrome self-heal (existing-machine path) ─────────────
# Chrome's wrapper lives in /usr/bin, but the real binary lives under
# /opt/google. Check both so a partial install cannot pass.
if ! command -v google-chrome-stable &>/dev/null || ! [ -x /opt/google/chrome/google-chrome ]; then
    wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg 2>/dev/null || true
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
    timeout 120 apt-get update -qq >/dev/null || true
    timeout 300 env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq google-chrome-stable >/dev/null || true
fi

# ── 2b. Sandboxing stack self-heal (Phase 1, existing-machine path) ──
# Retroactively installs per-workload-sandboxing deps. Idempotent — a
# no-op once bwrap is present. Proven on a real kernel (D5).
if ! command -v bwrap >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq bubblewrap tini nftables uidmap iproute2 >/dev/null || true
fi
# Phase 3 per-sandbox scheduler: supercronic (existing-machine
# self-heal, pinned + SHA256-verified). Idempotent; stderr stays
# visible (the ERR trap + a failed sha256sum -c must not be silent).
if ! [ -x /usr/local/bin/supercronic ]; then
    if curl -sfL https://github.com/aptible/supercronic/releases/download/v0.2.45/supercronic-linux-amd64 -o /tmp/supercronic \
      && echo "bb6da5af8d5547c9a5cbb4cf58d9f5541f0433df2188bfe4f1a54b04ad253db6  /tmp/supercronic" | sha256sum -c -; then
        install -m 0755 /tmp/supercronic /usr/local/bin/supercronic
    fi
    rm -f /tmp/supercronic
fi

# ── 3. Worker-agent self-heal (existing-machine path) ───────
# Idempotent check-and-install — mirrors lib/cloud-init.ts's updater
# block. Once every machine has both workers, these are no-ops.
# Failures are tolerated; the next 5-min tick retries.
#
# Check the canonical install path directly. A non-interactive `su -`
# login shell resolves PATH from ~/.bashrc, and Ubuntu's stock copy
# returns early for non-interactive shells — before the appended
# ~/.npm-global/bin line — so a PATH-based lookup false-negatives even
# when the binary is installed and the self-heal reinstalls every tick
# forever. Matches the daemon's binaryInstalled() (bootstrap.go).
if [ ! -x /home/vibecraft/.npm-global/bin/claude ]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs npm >/dev/null || true
    su - vibecraft -c "mkdir -p ~/.npm-global ~/.claude" || true
    su - vibecraft -c "npm config set prefix ~/.npm-global" || true
    if ! grep -q '.npm-global/bin' /home/vibecraft/.bashrc 2>/dev/null; then
        echo 'export PATH=$HOME/.npm-global/bin:$PATH' >> /home/vibecraft/.bashrc
    fi
    su - vibecraft -c "~/.npm-global/bin/npm i -g @anthropic-ai/claude-code 2>/dev/null || npm i -g @anthropic-ai/claude-code 2>/dev/null" || true
    chown vibecraft:vibecraft /home/vibecraft/.bashrc 2>/dev/null || true
fi
# Codex peer worker. Same shape as the Claude Code block above; the
# only differences are the canonical-path bin name, the session dir
# (~/.codex), and the npm package (@openai/codex). Older BYOM
# machines whose frozen vibecraft-update.sh has no codex block at all
# get codex via the daemon binary's ensureWorkerAgents on the next
# daemon update.
if [ ! -x /home/vibecraft/.npm-global/bin/codex ]; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs npm >/dev/null || true
    su - vibecraft -c "mkdir -p ~/.npm-global ~/.codex" || true
    su - vibecraft -c "npm config set prefix ~/.npm-global" || true
    if ! grep -q '.npm-global/bin' /home/vibecraft/.bashrc 2>/dev/null; then
        echo 'export PATH=$HOME/.npm-global/bin:$PATH' >> /home/vibecraft/.bashrc
    fi
    su - vibecraft -c "~/.npm-global/bin/npm i -g @openai/codex 2>/dev/null || npm i -g @openai/codex 2>/dev/null" || true
    chown vibecraft:vibecraft /home/vibecraft/.bashrc 2>/dev/null || true
fi

# Strip any provider API key exports left in ~/.bashrc. Always runs
# (not gated on the missing-binary blocks). Idempotent.
if grep -q '^export ANTHROPIC_API_KEY=' /home/vibecraft/.bashrc 2>/dev/null; then
    sed -i '/^export ANTHROPIC_API_KEY=/d' /home/vibecraft/.bashrc || true
fi
if grep -q '^export OPENAI_API_KEY=' /home/vibecraft/.bashrc 2>/dev/null; then
    sed -i '/^export OPENAI_API_KEY=/d' /home/vibecraft/.bashrc || true
fi

# ── 4. Local token self-heal (Phase 0b, existing-machine path) ──
# Pre-Phase-0b BYOM machines have no local.token. Generate one locally
# (the platform never sees BYOM tokens — Decision D2). Idempotent: a
# no-op once the file is non-empty. The daemon picks it up on its next
# restart (the binary rollout above restarts it on every version bump;
# a one-time restart also happens here if we just created the token).
if [ ! -s /etc/vibecraft/local.token ]; then
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/vibecraft/local.token
    chmod 600 /etc/vibecraft/local.token
    systemctl restart vibecraft-daemon || true
fi
UPDATEEOF
chmod +x /usr/local/bin/vibecraft-update.sh

cat > /etc/systemd/system/vibecraft-updater.service << 'SERVICEEOF'
[Unit]
Description=VibeCraft binary updater

[Service]
Type=oneshot
ExecStart=/usr/local/bin/vibecraft-update.sh
SERVICEEOF

cat > /etc/systemd/system/vibecraft-updater.timer << 'TIMEREOF'
[Unit]
Description=Check for VibeCraft binary updates every 5 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
TIMEREOF

# IP monitor
cat > /usr/local/bin/vibecraft-ip-check.sh << 'IPEOF'
#!/bin/bash
set -euo pipefail
# Force IPv4 — registration / update-ip and DNS A records are IPv4-only.
CURRENT_IP=$(curl -4sf --max-time 10 ifconfig.me || curl -4sf --max-time 10 icanhazip.com || exit 0)
STORED_IP=$(cat /etc/vibecraft/public-ip 2>/dev/null || echo "")
if [ "$CURRENT_IP" != "$STORED_IP" ] && [ -n "$CURRENT_IP" ]; then
  HEALTH_TOKEN=$(cat /etc/vibecraft/health.token 2>/dev/null || exit 0)
  MACHINE_ID=$(cat /etc/vibecraft/machine.id)
  curl -sf -X POST \
    -H "Authorization: Bearer ${HEALTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"ip\":\"${CURRENT_IP}\"}" \
    "https://www.vibecraft.so/api/machines/${MACHINE_ID}/update-ip" >/dev/null
  echo -n "$CURRENT_IP" > /etc/vibecraft/public-ip
fi
IPEOF
chmod +x /usr/local/bin/vibecraft-ip-check.sh

cat > /etc/systemd/system/vibecraft-ip-monitor.service << 'SERVICEEOF'
[Unit]
Description=VibeCraft IP change monitor

[Service]
Type=oneshot
ExecStart=/usr/local/bin/vibecraft-ip-check.sh
SERVICEEOF

cat > /etc/systemd/system/vibecraft-ip-monitor.timer << 'TIMEREOF'
[Unit]
Description=Check for public IP changes every 5 minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
TIMEREOF

# User services for desktop stack
mkdir -p /home/vibecraft/.config/systemd/user

cat > /home/vibecraft/.config/systemd/user/xvfb.service << 'SERVICEEOF'
[Unit]
Description=Xvfb virtual display

[Service]
Type=simple
ExecStart=/usr/bin/Xvfb :1 -screen 0 1024x768x24
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
SERVICEEOF

cat > /home/vibecraft/.config/systemd/user/fluxbox.service << 'SERVICEEOF'
[Unit]
Description=Fluxbox window manager
After=xvfb.service

[Service]
Type=simple
Environment=DISPLAY=:1
ExecStart=/usr/bin/fluxbox
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
SERVICEEOF

cat > /home/vibecraft/.config/systemd/user/chrome.service << 'SERVICEEOF'
[Unit]
Description=Google Chrome browser
After=fluxbox.service

[Service]
Type=simple
Environment=DISPLAY=:1
ExecStart=/usr/bin/google-chrome-stable --no-first-run --disable-default-apps --start-maximized --disable-infobars --no-default-browser-check
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
SERVICEEOF

chown -R vibecraft:vibecraft /home/vibecraft/.config

# ── Enable and start services ─────────────────────────────────

echo "Starting services..."

# Validate Caddyfile before restart — a typo or bad reverse_proxy
# directive would otherwise leave Caddy running on the previous config
# with no signal that the new one is broken. Capture combined output so
# we don't silently swallow stderr.
if ! caddy_validate_output=$(caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1); then
  echo "Error: /etc/caddy/Caddyfile failed validation. Output:"
  echo "$caddy_validate_output"
  exit 1
fi

systemctl daemon-reload
systemctl enable caddy vibecraft-daemon vibecraft-updater.timer vibecraft-ip-monitor.timer
# Use restart so caddy picks up the new Caddyfile (apt-install left a default
# config running; 'enable --now' would not reload it).
systemctl restart caddy
systemctl restart vibecraft-daemon
systemctl start vibecraft-updater.timer vibecraft-ip-monitor.timer

# Wait for each critical service to actually reach active. systemctl
# restart returns when the unit has been told to start, not when the
# process is ready — and a crash-on-startup would otherwise be invisible.
wait_for_active() {
  local unit="$1"
  for i in $(seq 1 20); do
    [ "$(systemctl is-active "$unit" 2>/dev/null)" = "active" ] && return 0
    sleep 0.5
  done
  echo "Error: $unit failed to become active within 10s" >&2
  echo "--- last 30 lines of journalctl -u $unit ---" >&2
  journalctl -u "$unit" --no-pager -n 30 >&2 || true
  return 1
}
wait_for_active caddy
wait_for_active vibecraft-daemon

# Enable linger and wait for the user manager + D-Bus to come up.
# Without this wait, the next 'systemctl --user' fails with
# 'Failed to connect to bus: No medium found' — linger triggers the
# user manager asynchronously and we race it.
loginctl enable-linger vibecraft
VIBECRAFT_UID=$(id -u vibecraft)
for i in $(seq 1 30); do
  [ -S "/run/user/$VIBECRAFT_UID/bus" ] && break
  sleep 0.5
done
if [ ! -S "/run/user/$VIBECRAFT_UID/bus" ]; then
  echo "Error: vibecraft user D-Bus socket did not appear after 15s."
  echo "  Try: loginctl enable-linger vibecraft && systemctl restart user@$VIBECRAFT_UID"
  exit 1
fi
sudo -u vibecraft XDG_RUNTIME_DIR=/run/user/$VIBECRAFT_UID systemctl --user daemon-reload
sudo -u vibecraft XDG_RUNTIME_DIR=/run/user/$VIBECRAFT_UID systemctl --user enable --now xvfb fluxbox chrome

# Verify each user service actually came up — 'enable --now' returns
# success even when the unit subsequently fails to start, so check the
# state explicitly. Same defense as wait_for_active above, against
# silent failures of a "successful" install.
for svc in xvfb fluxbox chrome; do
  state=$(sudo -u vibecraft XDG_RUNTIME_DIR=/run/user/$VIBECRAFT_UID \
    systemctl --user is-active "$svc" 2>/dev/null || echo "")
  if [ "$state" != "active" ]; then
    echo "Error: user service '$svc' is not active (state=$state)" >&2
    echo "--- journal for $svc ---" >&2
    sudo -u vibecraft XDG_RUNTIME_DIR=/run/user/$VIBECRAFT_UID \
      journalctl --user -u "$svc" --no-pager -n 30 >&2 || true
    exit 1
  fi
done

# ── Self-test ────────────────────────────────────────────────
# Verify the daemon is listening locally. Curl returns 0 on any HTTP
# response, non-zero on connection refused / timeout — so we just need
# a response (401 from a protected endpoint counts as 'alive').
echo "Verifying daemon is reachable on localhost:8420..."
DAEMON_READY=false
for i in $(seq 1 20); do
  if curl -s -o /dev/null --max-time 2 http://127.0.0.1:8420/; then
    DAEMON_READY=true
    break
  fi
  sleep 0.5
done
if [ "$DAEMON_READY" != "true" ]; then
  echo "Error: vibecraft-daemon is not responding on localhost:8420 after 10s" >&2
  echo "--- last 30 lines of journalctl -u vibecraft-daemon ---" >&2
  journalctl -u vibecraft-daemon --no-pager -n 30 >&2 || true
  exit 1
fi

# ── Done ──────────────────────────────────────────────────────

echo ""
echo "================================================"
echo "  VibeCraft daemon installed and registered."
echo ""
echo "  Machine: $MACHINE_HOST"
echo "  Status:  active"
echo ""
echo "  To uninstall:"
echo "  curl -sfL https://www.vibecraft.so/uninstall.sh | sudo bash"
echo "================================================"
echo ""
