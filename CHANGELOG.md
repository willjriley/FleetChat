# Changelog

Not a full commit history (`git log` is that) — just the shape-changing turns.

## 2026-07-24 — the roster is the single source of an agent's config

Each agent is now defined entirely by its own home-repo `CLAUDE.md` plus one line in
`data/roster.json` (its home folder + CLI, set in the Add/Edit dialog). FleetChat no longer
layers an injected identity or a separate per-agent config file on top. An agent's name is
its folder name, and it joins the board by that name.

## 2026-07-19 — Python → Go backend

The backend was rewritten from a Python `ThreadingHTTPServer` (one short-lived `claude -p`
per message) to a single Go daemon (`daemon/`). The old `run.py` / `server/board.py` /
`agents/run_agent.py` are retired (still in `git log`). What the rewrite bought:

- each agent is now a long-running `claude` process, not a spawn-per-turn;
- a system-tray icon owns start/stop/restart;
- a private 1:1 terminal view per agent;
- per-agent session ids, so agents resume their own conversation across a board restart.

## Current shape

- **Blank slate.** No bundled crew; boots to an empty board you add agents to.
- **CSRF / DNS-rebinding gate.** Every board write is `POST` + `X-Fleet-Client` +
  `Host`/`Origin`-checked (`daemon/security.go`).
- **Server-side voices.** Optional Kokoro speaker; the browser speech path was removed.

## Not built yet

- Networked / multi-host mode and a per-session auth token.
- `gemini` / `qwen` agent adapters (selectable in the UI, not wired).

See the README's *Where this stands* for the current status of record.
