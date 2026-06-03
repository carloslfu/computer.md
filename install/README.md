# Install paths

`computer.md` ships three installers for three deployment shapes.

## 1. CLI installer — `install/cli.sh`

Installs the `vibecraft` CLI on a developer machine (macOS, Linux,
Windows). The script served at `https://www.vibecraft.so/install/cli.sh`
is this file.

```bash
curl -fsSL https://www.vibecraft.so/install/cli.sh | sh
```

Optional flags:

```bash
# System install (requires sudo, goes to /usr/local/bin)
curl -fsSL https://www.vibecraft.so/install/cli.sh | sh -s -- --system

# Pin a specific version
curl -fsSL https://www.vibecraft.so/install/cli.sh | sh -s -- --version=cli-v0.50.0
```

Self-update path: `vibecraft self-update`.

## 2. BYOM daemon installer — `daemon/bootstrap/install.sh`

Installs the `vibecraft-daemon` on a customer-owned machine that
registers with the VibeCraft platform (shape 2: Connected). Uses a
one-time registration token + machine id minted by the platform.
Current support is Linux x86_64/amd64 only; the installer exits early
on other architectures until the daemon/browser stack is released there.

```bash
# The platform's BYOM "Add machine" flow generates this command for you:
curl -fsSL https://www.vibecraft.so/install.sh | sudo bash -s -- \
  --token REG_TOKEN \
  --machine MACHINE_ID \
  --openai-key sk-...
```

## 3. Pure self-host installer — `daemon/bootstrap/install-standalone.sh`

Installs the `vibecraft-daemon` for shape 1 (pure self-host, no
platform integration). No registration token, no machine id, no
auto-update timer, no platform calls.
Current support is Linux x86_64/amd64 only, matching the released
`vibecraft-daemon-linux-amd64` binary.

```bash
curl -fsSL https://raw.githubusercontent.com/carloslfu/computer.md/main/daemon/bootstrap/install-standalone.sh \
  | sudo bash -s -- --openai-key sk-...
```

Updates: manual download of a new binary, then `systemctl restart vibecraft-daemon`.

## Docker Compose

For an even lighter self-host (no system users, no systemd), use the
[docker-compose.yml](../docker-compose.yml) at the repo root:

```bash
git clone https://github.com/carloslfu/computer.md.git
cd computer.md
cp .env.example .env  # set OPENAI_API_KEY
docker compose up -d
```

Caveats: no X11/Chrome inside the container (browser tooling won't
work), no Codex/Claude Code workers.
