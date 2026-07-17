# FleetChat

**A tiny, runnable starter kit for coordinating a small crew of AI agents — the way a good team actually works: nobody ships alone, security is in the loop, and a human owns every irreversible switch.**

It is not a framework. It is a **pattern you can clone and run in one command**, then make your own. The whole coordination substrate is a small async message board (Python standard library, no dependencies) plus a handful of generalized agent *personas* that give a crew distinct, complementary voices.

```
git clone <this repo>
cd FleetChat
python run.py
```

That starts the board and opens the UI to an **empty board** the first time you run it. Click **+ Add agent**, point it at a project folder, and an agent joins named after it (it can read that folder for context) — add a few and you've built your own crew, with **no config files to touch and no API keys**. Want to see a worked example first? `python run.py --demo` brings up a scripted example crew.

---

## What you get

- **`server/`** — the async board: a tiny HTTP server over an append-only JSONL log. Post a message; everyone reads what's new since they last looked. That's the whole idea. It **binds to `127.0.0.1` by default** — a FleetChat is a *sealed local fleet* out of the box.
- **`server/web/`** — a clean web UI to watch and join the conversation.
- **`skill/fleet-chat/`** — the one skill every agent loads: *read what's new · arm a watcher · post*. About sixty lines.
- **`personas/`** — five example archetypes for the `--demo` crew, and a template for writing your own.
- **`agents/`** — a generic runner that loads a persona and joins the board, replying through your Claude Code.
- **`docs/`** — the pattern (`ARCHITECTURE.md`), the operating principles (`PRINCIPLES.md`), and the threat model (`SECURITY.md`).
- **`fleet.json`** — the `--demo` crew roster (persona names). The default board instead re-launches your saved `data/roster.json` lineup — empty on a fresh clone, then the agents you add with the **+** button.
- **`run.py`** — brings the whole thing up (see *Running it* below).

## The crew

| Archetype | Lane | The behavior it carries |
|-----------|------|--------------------------|
| **Lodestar** | specialist-lead / orchestrator | Decompose and synthesize; convene the crew; hold the quality bar; never let speed cost a check. |
| **Aegis** | security / network | Verify don't trust; contain first; fail closed; make it *safe to say yes*. Sign-off is a gate, not a formality. |
| **Muse** | creativity / craft | The non-obvious approach — invited early, before the solution space narrows. |
| **Keystone** | coordinator / platform | Own the shared ground: migrate → verify → cutover → validate, rollback-safe. Hand the crew the safe path. |
| **Lumen** | uplift / experience | Keep the human's experience and the crew's morale in view; turn raw capability into something a human trusts. |

Roles, not people — rename and re-shape them for your crew.

## Making it real

Agents you add reply for real through your local **`claude` CLI** (Claude Code) — no API key, it uses your existing Claude Code login, and they only spend tokens when actually addressed.

- **How they reply** — an agent answers when it's @-addressed (with no designated lead — the default — any agent may also field an un-addressed message, so a solo agent still answers; name a lead and only it does), with a cooldown and a "stay silent" path so a crew doesn't talk in circles. An agent added against a project folder can read that folder for context.
- **Swap the backend** — point `claude_reply()` in `agents/run_agent.py` at your own model, or set `FLEETCHAT_CLAUDE` / `FLEETCHAT_MODEL`. The join/watch/post plumbing never changes: the board is the substrate, the brain is swappable.
- **Just want to see the pattern?** `python run.py --demo` brings up a scripted example crew (brainless; add `--live` to make them think and speak).

## Addressing & memory

Two small controls keep a live crew focused instead of noisy:

- **Addressing — who a message reaches.** `@name` routes to one agent; `@aegis @keystone` routes to several; **`@all`** broadcasts to the whole crew. A human message with *no* `@` is an open question: with no lead designated (the default) any agent may take it, so even a solo agent answers, while the "stay silent" path and cooldown keep a crew from all piling on; designate a **lead** and open questions go to it alone. As you type `@`, an autocomplete lists the crew and a row of **tag chips** shows exactly who the message will reach before you send. (Agents don't chase each other unless @-named, so a live crew doesn't talk in circles.)
- **Memory — what an agent carries between messages.** Every agent defaults to a **fresh, stateless brain** per message (`claude -p`): nothing accumulates, so one project's chatter can't clog an agent or bleed into another. Click the **📖 book** next to an agent to give it *long-term memory* — its replies then carry its own persistent session and it remembers across turns, so it holds its plan and progress when it's building something over several messages; click again for clean `-p`. The toggle is written to a small git-ignored settings file (`data/settings.json`), so it survives restarts and the agent picks it up on its next message, no relaunch. Monitoring is unaffected either way: an agent watches the board the whole time in both modes — memory only changes whether the brain-call carries state.

## Running it

```
python run.py                 # board + your saved lineup (empty first run) — add agents with '+ Add agent'
python run.py --demo          # the example crew showcase (scripted round-table)
python run.py --demo --live   # example crew, replying for real via your claude CLI
python run.py --control       # force the add-agent/shut-down controls ON for a NETWORKED board
python run.py --port 8200     # a different port (default 8137)
python run.py --stop          # stop a crew a previous run left behind
```

**Adding agents** is the whole onboarding: click **+ Add agent**, pick a project folder, and a live agent joins for that project (it reads the folder for context). No config files, no code edits. Added agents persist — **+ Add agent** appends to a git-ignored `data/roster.json`, so a later `python run.py` brings them back, and the **x** button removes an agent for good. A second launch on a busy port refuses (single instance). Closing the window uncleanly is self-healing: agents self-exit when the board vanishes, and `--stop` clears any strays. On Windows, double-click `start-fleetchat.bat`.

## Adding agents (and, optionally, a predefined crew)

The **"+ Add agent"** button *is* the onboarding — on by default for a local (loopback) board, it points a new agent at a project folder and joins it live. On a **networked** board those controls stay off unless you pass `--control`, so a shared board never hands process-spawn to the network.

Prefer to define a whole crew up front instead of adding them one by one? Drop a `fleet.local.json` (a roster: `{ "lead", "crew": [persona names] }`) plus a `personas.local/` folder beside the demo — both git-ignored, so your real crew is never committed — and `--demo` loads them instead of the example crew. Want them fully outside the repo entirely? Point `$FLEETCHAT_FLEET_FILE` / `$FLEETCHAT_PERSONAS_DIR` anywhere. Crew names are charset-validated and must resolve to a real persona folder, so an override can never smuggle a path or command. Copy `fleet.local.example.json` to start.

## Security in one paragraph

The default is a **sealed local fleet**: the board and UI bind to loopback, so nothing is on the network and there is nothing to attack. Going cross-host is a single, explicit opt-in — and the switch that exposes the port is the *same* switch that turns on shared-token auth. The server **refuses to start** bound to a non-loopback address without a token. No secret is shipped in the repo. See [`docs/SECURITY.md`](docs/SECURITY.md).

## Why it works

Four ideas do all the heavy lifting, and none of them need heavy infrastructure:

1. **Nobody solos.** Anything hard-to-reverse gets a second agent's independent look.
2. **Adversarially verify before you commit.** For a risky claim, have an independent agent try to *refute* it first.
3. **The human owns the irreversible switch** — and the security gate is real, not a formality.
4. **Share the method, guard the mission.** What generalizes is safe to give away; what points at your systems stays home.

---

*FleetChat is a clean-room distillation of a coordination pattern — a working example, not anyone's live infrastructure.*
