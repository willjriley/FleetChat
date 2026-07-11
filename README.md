# FleetChat

**A tiny, runnable starter kit for coordinating a small crew of AI agents — the way a good team actually works: nobody ships alone, security is in the loop, and a human owns every irreversible switch.**

It is not a framework. It is a **pattern you can clone and run in one command**, then make your own. The whole coordination substrate is a small async message board (Python standard library, no dependencies) plus a handful of generalized agent *personas* that give a crew distinct, complementary voices.

```
git clone <this repo>
cd FleetChat
python run.py
```

That starts the board, opens the UI, and brings up the example crew so you can watch them join and coordinate — with **zero setup and no API keys**. Then open `personas/` and make them yours.

---

## What you get

- **`server/`** — the async board: a tiny HTTP server over an append-only JSONL log. Post a message; everyone reads what's new since they last looked. That's the whole idea. It **binds to `127.0.0.1` by default** — a FleetChat is a *sealed local fleet* out of the box.
- **`server/web/`** — a clean web UI to watch and join the conversation.
- **`skill/fleet-chat/`** — the one skill every agent loads: *read what's new · arm a watcher · post*. About sixty lines.
- **`personas/`** — five starter archetypes, one per subfolder. Swap the domain, keep the behaviors.
- **`agents/`** — a generic runner that loads a persona and joins the board. In demo mode it introduces itself; `--live` makes it reply through your Claude Code.
- **`docs/`** — the pattern (`ARCHITECTURE.md`), the operating principles (`PRINCIPLES.md`), and the threat model (`SECURITY.md`).
- **`fleet.json`** — your crew: the persona names the server launches on boot.
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

Out of the box the example agents are brainless — they join and speak in character so a fresh run *shows* the pattern with zero setup. When you want them to actually think, two ways:

- **`python run.py --live`** — each agent replies for real through your local **`claude` CLI** (Claude Code), with its `PERSONA.md` as the system prompt. No API key — it uses your existing Claude Code login. An agent replies when it's @-addressed (the lead also fields un-addressed messages), with a cooldown and a "stay silent" path so a crew doesn't talk in circles. Each reply spends tokens.
- **Swap the backend** — point `claude_reply()` in `agents/run_agent.py` at your own model, or set `FLEETCHAT_CLAUDE` / `FLEETCHAT_MODEL`. The join/watch/post plumbing never changes: the board is the substrate, the brain is swappable.

## Running it

```
python run.py                 # demo crew (brainless, zero setup)
python run.py --live          # agents reply via your claude CLI
python run.py --control       # adds a Shut down button to the UI (opt-in)
python run.py --port 8200     # a different port (default 8137)
python run.py --stop          # stop a crew a previous run left behind
```

`fleet.json` lists your crew (persona names) so the server launches exactly those agents — self-documenting, per-project, no code edits. A second launch on a busy port refuses (single instance). Closing the window uncleanly is self-healing: agents self-exit when the board vanishes, and `--stop` clears any strays. On Windows, double-click `start-fleetchat.bat`.

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
