# Developer Workstation Computer

An engineer's personal dev box. Coding, builds, deployment, local
infrastructure, dotfile hygiene, occasional personal projects.

## Owner

Riley Tanaka, staff software engineer at a healthcare startup. Based
in Seattle (Pacific time). Comfortable with the terminal; opinionated
about tools; prefers fast iteration over comprehensive setups.

## Business

Riley works at a Series B healthcare software company. The day job
runs in a separate corp-issued machine. This computer is personal —
side projects, open-source contributions, learning, occasional
freelance.

## Tools

- GitHub — code hosting (personal + open-source)
- Vercel — deploys for side projects
- Neon — Postgres
- Cloudflare — DNS, R2
- Fly.io — backend hosting when Vercel doesn't fit
- Linear — personal todo tracker
- Notion — engineering notes, learning log
- 1Password — secrets vault
- Docker Desktop — local containers
- pnpm / cargo / uv — package managers
- nvim / Cursor / VS Code — editors (depends on task)
- tmux / iTerm — terminal

## Standing rules

- Never commit secrets. If you see an `.env` or `.env.local` mentioned anywhere, double-check it's .gitignored before writing code.
- Open-source contributions: PRs go through me, never auto-merge or auto-submit.
- Personal projects: feel free to act autonomously on dev work, but `git push` to public repos always shows me the changeset first.
- Side gig invoices: drafts only — I send them.
- Don't auto-install npm/pnpm packages with native bindings without flagging; M-series Mac native-build flakiness eats hours.

## Routines

- ~/systems/morning-dev-pulse/ — 8am PT, summary of overnight PRs/notifications across personal repos, OSS contributions
- ~/systems/deploy-watch/ — every 15 min, monitors Vercel / Fly.io for failed deploys, drafts an alert if anything red
- ~/systems/learning-log/ — Sundays 8pm PT, summarizes the week's commits + notes into a weekly review
- ~/systems/freelance-tracker/ — Mondays 9am PT, time spent on freelance projects + invoice draft if invoiceable

## Out of scope

- Day-job code (separate machine, company NDA)
- Anything healthcare-PHI-related — strictly company computer
- Tax or business filings — handled with my accountant
- Anything related to job search or comp negotiation — handle myself

## Worker preference

Claude Code for everything substantive. Codex as fallback. Build
inline only for one-line questions ("what's the syntax for X").

## Preferences

- Code style: match the project's existing style (rustfmt, prettier, etc.). Never reformat unrelated lines in a PR.
- Commit messages: imperative voice, < 72 chars, body if non-trivial.
- PR descriptions: "what changed" + "why" + "how to test". Skip "what" if the diff is obvious.
- Always run the test suite before declaring a task done.
- When stuck, prefer searching the codebase to guessing.

## What the manager has learned about this machine

- I use pnpm for JS, uv for Python, cargo for Rust. Don't switch package managers per project.
- My GitHub workflow uses signed commits via SSH key (`~/.ssh/id_ed25519`); never use HTTPS for git operations.
- The "personal" GitHub Apps token in 1Password is read-only — don't try to push with it.
- Fly.io org "riley-personal" is for side projects; "riley-freelance" for paid client work — billing matters.
- Local dev DBs run in Docker; never connect production DBs to local dev tooling.
- I run nvim 0.10+ with LazyVim; the Cursor + VS Code combo is for occasional GUI work, not primary.
