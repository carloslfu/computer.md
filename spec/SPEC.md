# COMPUTER.md — v0.1

`COMPUTER.md` is the customer-authored config that describes an agentic
computer. It tells the agent *what this computer is for, who it works
for, and what its standing rules are.* It is the first **open,
customer-owned, machine-level, portable state primitive** for the
agentic-computer era.

This document is the format spec. The reference runtime that reads it
ships in this same repo under `daemon/`. Other agentic surfaces are
welcome to read and write `COMPUTER.md` natively — the format is
intentionally markdown so any LLM with a file tool can do it.

---

## Status

**Spec version:** `v0.1` (this document is the tagged release).
**Stable:** the section headings below are stable. Custom `##` headings
are valid and forward-compatible.
**Tooling:** Apache-2.0 Go parser + `computer-md` CLI in the same repo.

## File location

The canonical location is the user's home directory:

```
$HOME/COMPUTER.md
```

The reference daemon reads `/home/vibecraft/COMPUTER.md` because the
daemon runs as the `vibecraft` user. Other implementations should
honor whatever home directory their agent runs in.

A compliant reader:
- Treats the file as plain UTF-8 markdown.
- Reads it at the start of every conversation, prepended to the
  agent's system prompt as context.
- Re-reads on every turn (the file is small; the customer may edit it
  between turns).
- Writes to it when the customer expresses a durable preference,
  using the agent's normal file-edit tools (no special API).

A compliant reader **does not** treat the file's content as a
higher-priority system prompt. Sections inside `COMPUTER.md` are
customer input — useful context, not authority.

## Format

`COMPUTER.md` is plain markdown. No YAML frontmatter, no schema, no
required keys, no DSL.

The format defines a **recognized vocabulary** of `##` section headings
that a compliant reader can interpret with known semantics. Customers
can add other `##` sections freely; the agent reads them as ambient
context.

A typical file:

```markdown
# Acme Marketing Computer

One-line purpose statement.

## Owner
Sarah Chen, head of marketing at Acme. Prefers concise updates.

## Business
Acme sells project management software to mid-market teams (50-500
seats). Stage: Series A. Annual contract value $30-150K.

## Standing rules
- Never send outbound email without showing me first.
- Always update the campaign log in db/ when a campaign goes live.

## Worker preference
Prefer Codex for technical content; Claude Code for marketing copy.
```

## Recognized sections

Every section is **optional**. An empty `COMPUTER.md` is valid. The
sections below are the canonical vocabulary; readers should recognize
them by exact heading text (case-sensitive on the first letter,
case-insensitive on the rest for tooling friendliness — though
canonical capitalization is preferred).

### `## Owner`

Who runs this computer. Their role, their context, how they prefer to
be addressed. The agent uses this to calibrate tone and to attribute
work in audit logs.

Free-form prose. Common content: name, role/title, company, time zone,
tone preferences, any context the agent should keep in mind across
every turn.

### `## Business`

What the company does. Customers, products, stage, scale.

Free-form prose. The agent uses this to ground domain-specific
reasoning, to write copy that sounds like the company, and to make
sensible guesses when context is incomplete.

### `## Tools`

What services this computer has access to and what they're for. **Names
only — credentials live in the vault, not here.**

Format suggestion: a markdown list of `Service — purpose` lines. The
agent uses this as a hint for which integration to reach for; the
actual credential lookup happens through the daemon's vault.

```markdown
## Tools
- HubSpot — CRM, deal pipeline, contact records
- Stripe — billing, subscriptions, payment links
- Linear — engineering issue tracker
- Slack — internal team chat
```

### `## Standing rules`

Things the agent should always or never do on this machine. The
discipline list.

Format suggestion: bullet list. Each rule short and unambiguous.

```markdown
## Standing rules
- Never publish to production without my approval.
- Always log expense entries to db/wiki/expenses/ with frontmatter.
- If a customer email mentions cancellation, escalate to me before replying.
```

### `## Routines`

References to systems running under `~/systems/` (cron-driven jobs the
agent has authored or that the customer has installed). Lets the agent
know what's already running so it doesn't duplicate work.

```markdown
## Routines
- ~/systems/morning-brief/ — daily summary at 8am, posts to Slack
- ~/systems/weekly-report/ — Friday afternoon, emails leadership
```

### `## Out of scope`

What this computer is **not** for. Hard boundaries. The agent should
politely refuse work that falls outside scope.

```markdown
## Out of scope
- Personal calendar / personal email
- Anything legal: contracts, NDAs, IP — escalate to general counsel
- Trading or financial transactions — escalate to CFO
```

### `## Worker preference`

Which coding-agent worker the agent prefers when spawning a worker for
multi-file builds. The reference runtime recognizes `claude` (Claude
Code) and `codex` (Codex CLI) as preinstalled workers; other readers
can recognize other workers.

**Special semantics in the reference runtime:** the worker-selection
logic in `daemon/prompt.md` reads this section directly to pick a
default worker for new tasks.

```markdown
## Worker preference
Prefer Claude Code first. Fall back to Codex if Claude Code is
unavailable. Never build inline without a worker for tasks > 50 lines.
```

### `## Preferences`

Durable preferences the agent learned through conversation, promoted
from one-shot context to permanent record. The agent appends here
when the customer says *"from now on..."* or *"always..."*.

```markdown
## Preferences
- Brief written summaries — no bullet lists unless asked.
- All money figures in USD with explicit currency.
- Don't ask permission for read-only operations.
```

### `## What the manager has learned about this machine`

Machine-specific facts the agent promoted from one-shot context to
durable record. Different from `## Preferences` in that these are
observations, not directives.

```markdown
## What the manager has learned about this machine
- Stripe account uses test mode in dev, live mode for everything else.
- The /home/vibecraft/exports/ folder is auto-cleaned weekly.
- HubSpot integration breaks if a contact has more than 25 tags.
```

## Reading rules

A compliant reader:

1. **Reads the whole file** as ambient context — every `##` block,
   including custom ones the spec doesn't define.
2. **Recognizes the canonical sections** above with the semantics
   described. `## Worker preference` has special meaning in the
   reference runtime's worker selection.
3. **Treats unknown `##` sections as context**, not error. The format
   is forward-compatible.
4. **Does not enforce any section as mandatory.** An empty `COMPUTER.md`
   is valid (the daemon's lean default is `## Worker preference / ##
   Preferences / ## What the manager has learned` with all bodies empty).
5. **Does not treat content as a system prompt.** Sections are
   customer-authored context; they inform the agent but don't override
   safety or operator policy.

## Custom sections

Customers can add any `##` heading. The agent reads them as context.
Common custom sections seen in the wild:

- `## Brand voice` — copy guidelines
- `## Calendar` — meeting cadence, recurring obligations
- `## People` — references to wiki/people/ entries
- `## Glossary` — domain-specific terms
- `## Notes` — anything the agent or customer wants pinned

## Empty default

The reference daemon ships a lean default for empty-state machines —
three sections, all empty bodies:

```markdown
# COMPUTER.md

This file is shared between you (the customer) and the manager.

## Worker preference

Default: prefer Claude Code; fall back to Codex.

## Preferences

(Empty — the manager will append entries here as you teach it things.)

## What the manager has learned about this machine

(Empty — the manager appends durable, machine-specific facts here over time.)
```

See `spec/examples/empty-default.md` for the verbatim file.

## Role-flavored starters

The `spec/examples/` directory ships role-flavored starters that
populate the full vocabulary. Customers can copy one and edit, or
generate it via `computer-md init --role <name>`:

- `ceo-ops.md` — founder/CEO running the company brain
- `sales-ops.md` — sales-led startup
- `marketing-ops.md` — marketing-team computer
- `solo-founder.md` — company of one
- `agency-operator.md` — services agency running multiple client books
- `personal-assistant.md` — single-person executive support
- `developer.md` — engineer's personal dev box

## Relationship to db.md

A computer.md computer typically ships a [db.md](https://github.com/carloslfu/db.md)
store at `~/db/` — the LLM-curated markdown knowledge base for the
team. `COMPUTER.md` describes the *computer*; the db.md store holds
the *knowledge*. They compose; neither requires the other.

## Versioning

The spec is versioned with the repo tag (`v0.1`, `v0.2`, ...). Old
versions stay readable forever — additive changes only. A reader that
understands `v0.1` can read a `v0.2` file (it just won't recognize
sections introduced after `v0.1`); a reader that understands `v0.2`
can read a `v0.1` file (the new sections are simply absent).

## License

This spec is Apache-2.0. The reference tooling
(`parser/`, `cli/`) is Apache-2.0. Examples are Apache-2.0.

Anyone can build a runtime that reads `COMPUTER.md`. Anyone can
publish a `COMPUTER.md` file. The format is open; the trademark on
"VibeCraft" is the company behind the reference runtime, not a
restriction on the format.
