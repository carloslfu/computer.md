---
name: vibecraft
description: Drive a VibeCraft agentic computer via its CLI. Run 'vibecraft docs' for the full reference, or read https://www.vibecraft.so/llms.txt.
---

# VibeCraft CLI

You have the `vibecraft` binary on PATH. It is the agent-native interface to a
customer's VibeCraft computer. Every command returns a JSON envelope on
stdout (or JSON Lines for streams); errors go to stderr with stable `code`
strings; exit codes signal task outcome (0/1/2/3/4).

**Before doing anything else: read the reference once per session.**

```
vibecraft docs
```

Same content is served at `https://www.vibecraft.so/llms.txt`.

## Cheat sheet (most common moves)

```
# Auth — ONCE. One browser approval authorizes every machine on the
# account. Tell the human to approve the window that opens:
vibecraft auth login

# See every machine you can drive:
vibecraft machine list

# State check (a single machine, or all of them):
vibecraft status
vibecraft --machine all status

# Send a task and wait for the answer (add --machine <id> to pick one):
vibecraft task submit "do the thing"

# Background submit + later wait:
id=$(vibecraft task submit "long thing" --no-wait | jq -r .data.id)
vibecraft task wait "$id"

# Multi-turn (task ends in waiting_for_input → exit 3):
out=$(vibecraft task submit "ship the deploy")
if [ $? -eq 3 ]; then
  id=$(echo "$out" | jq -r .data.id)
  vibecraft task respond "$id" "yes, ship it"
fi

# Stream events:
vibecraft task stream "$id"   # JSON Lines

# Read the transcript:
vibecraft task messages "$id" | jq .data.messages

# Headless (CI / no browser): set VIBECRAFT_ACCOUNT_KEY, or a single
# machine's VIBECRAFT_MACHINE_URL + VIBECRAFT_API_KEY.
```

## Exit code dispatch (memorize this)

```
0  ok / task completed
1  CLI error (auth, network, validation, daemon 5xx) — see stderr envelope
2  task FAILED — TaskData on stdout
3  task NEEDS INPUT — call 'vibecraft task respond <id>'
4  task CANCELLED
```

## Asking for help

If you hit `auth_required` / `auth_invalid` / `auth_forbidden` / repeated `server_error`,
surface to the human:

> "I tried `vibecraft <cmd>` on `<machine>` and got `<error_code>`. Suggested next step:
> `<hint>`. Should I `<hint>` or do you want to handle it?"
