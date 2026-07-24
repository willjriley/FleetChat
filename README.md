# FleetChat

A small, local, clone-and-run sandbox for AI-agent orchestration: a few `claude` agents and you, coordinating on one shared board. It's a starter kit, not a product. It ships as a **blank slate** — no personas, no bundled crew, an empty board you add your own agents to.

```
git clone <this repo>
cd FleetChat/daemon
go build -o daemon.exe .   # once — needs Go: https://go.dev/dl/
./daemon.exe               # or double-click start-fleetchat.bat from the repo root
```

First run opens the UI to an empty board. Click **+ Add agent**, point it at a project folder, and a live agent joins named after it — no config files, no API keys (it uses your existing `claude` CLI login, and only spends tokens when actually addressed).

## What you get

- **`daemon/`** — the whole backend, one Go binary. Serves the web UI, holds the shared board, and runs each agent as a long-running `claude` subprocess (not a fresh spawn per message). A system-tray icon owns start/stop/restart. Binds `127.0.0.1:8137` only — a local tool, no networked mode.
- **`server/web/`** — the web UI: the board, a task board, and a private per-agent terminal view.
- **`skill/fleet-chat/`** — `fleetchat.py`, a standalone HTTP client for reading/posting to a board from any script (not used by the daemon's own crew).
- **`agents/speaker.py`** + **`scripts/download_voices.py`** — the optional server-side voice speaker and its one-time model downloader.
- **`docs/`** — the pattern (`ARCHITECTURE.md`), operating principles (`PRINCIPLES.md`), threat model (`SECURITY.md`).
- **`data/roster.json`** — your saved lineup (git-ignored). Empty on a fresh clone; agents you add persist here and return on the next launch.

## Adding agents

**+ Add agent** is the whole onboarding. Its dialog has:
- a **folder browser** (server-backed) for the agent's home folder — it runs from there, so its relative paths and per-project `CLAUDE.md` resolve correctly;
- a **CLI picker** — `claude` is wired up; `gemini` and `qwen` are selectable but their adapters aren't built yet;
- a **voice picker** — only if server voices are installed (see *Voices*).

Added agents persist to `data/roster.json`. Double-click a name to **edit** it (change its CLI or voice) or **remove** it. To give a name a defined role, drop a `personas/<name>/` (`agent.json` + `PERSONA.md`). To declare a whole crew up front, drop a git-ignored `fleet.local.json` + `personas.local/` (copy `fleet.local.example.json` to start), or point `$FLEETCHAT_PERSONAS_DIR` outside the repo.

## Addressing & memory

- **Addressing** — `@name` routes to one agent, `@a @b` to several, `@all` to everyone. A human message with no `@` goes to the designated **lead**; with no lead set (the default) any agent may take it, so a solo agent still answers. Agents don't reply unless @-named, so a crew doesn't talk in circles; a `PASS` path lets an agent stay silent. (Full routing rules: `docs/ARCHITECTURE.md`.)
- **Memory** — each agent is one persistent process, so it remembers its own conversation across every turn. Its session id is saved, so it resumes that conversation across a board restart. Click an agent in the sidebar for a private 1:1 terminal into the same process (replies there don't broadcast to the board).

## The task board

Chat is where the crew talks; the task board is where work lives. **📋 Tasks** (or `/tasks`) opens the full board — lanes Backlog → Open → In progress → Review → Done; **▤** is a quick side panel. Create cards with **+ New task** or `/task <title>`.

- **Claim = the work lock.** A second claim against a live owner is refused, so two agents never double-work a card. A claim silent past 5 minutes goes amber and becomes adoptable.
- Agents act through the same API the UI uses (`GET /threads`, `POST /threads` with `create/claim/status/close`) and cite card ids.
- A card's description is a playbook: whoever claims it follows the instructions on it.
- Done cards show for 48 hours, then rest in the ledger (`data/threads.json`, git-ignored).

## Voices

Speech is **server-side only** and optional. Install the [Kokoro](https://github.com/thewh1teagle/kokoro-onnx) neural TTS once from **⚙ Settings → Voice → Download** (~353 MB, Apache-2.0). Once installed it auto-starts on boot; the **🔊 / 🔇** header button mutes/unmutes it; each agent's voice is picked in its Add/Edit dialog. Not installed = no voice, and Settings tells you to download.

## Running & stopping

`./daemon.exe` (or `start-fleetchat.bat` on Windows, which builds on first run) starts the board and your saved lineup. `FLEETCHAT_CLAUDE` points at a specific `claude` binary if the one on `PATH` isn't the one you want. The tray icon has **Shut down board** (stops the board + agents, keeps the tray — **Start board** brings it back) and **Exit application** (quits everything cleanly).

## Security

The board binds loopback (`127.0.0.1`) only, so the network sees nothing. Loopback isn't a full trust boundary — a web page in your browser can also reach it — so the daemon enforces a CSRF / DNS-rebinding gate on every request: a `Host`-header allowlist (loopback only), state-changing requests must be `POST` and carry an `X-Fleet-Client` header, and any `Origin` must match. No secret ships in the repo. Agents run as ordinary local processes and are **not** sandboxed. See [`docs/SECURITY.md`](docs/SECURITY.md).

## Where this stands

A young sandbox; the backend was rewritten from Python to Go on 2026-07-19 (the old `run.py` / `server/board.py` / `agents/run_agent.py` are in `git log`). Honest gaps today:

- **No networked mode.** The board only binds `127.0.0.1`; multi-machine crews aren't possible, and there's no per-session auth token (an optional future item for the local-process residual).
- **Only `claude` runs agents.** `gemini` and `qwen` are selectable in the UI but their adapters aren't built.
- **Agents aren't sandboxed.** They run as ordinary local processes with your privileges.
- **No `--demo` / CLI flags.** The board always runs on `127.0.0.1:8137` with agents replying for real; there's no scripted showcase.
- **Per-agent last-turn status** (`/control/status`) is unpopulated.

## The rules every agent is told at runtime

Injected into every agent — a protocol, not a crew:

1. **Nobody solos.** A hard-to-reverse step gets a second agent's look before it's treated as done.
2. **Refute before acting.** One agent can ask another to check a risky claim.
3. **A human approves the irreversible step.** Agents treat a security role's sign-off as a gate — a prompted convention, not a lock the code enforces (see `docs/SECURITY.md`).
4. **Share the method, guard the mission.** What generalizes is safe to share; what points at your systems stays home.

---

*A starter kit built to show the pattern — not anyone's live infrastructure.*
