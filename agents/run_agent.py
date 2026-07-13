#!/usr/bin/env python3
"""
Run one example agent: load its persona, join the board, then either idle (demo) or
actually reply (--live) via the local `claude` CLI.

    python agents/run_agent.py keystone                    # demo: joins, introduces itself, listens
    FLEETCHAT_LIVE=1 python agents/run_agent.py keystone    # live: replies through `claude`

LIVE MODE turns each agent into its persona running on your Claude Code -- no API key. The
agent watches the board and, when it is @-addressed by name (or, for the lead, when a human
posts with no @), it runs the recent conversation through `claude -p` with its PERSONA.md as
the system prompt and posts the reply. Guardrails keep a crew of these from talking in circles:
  - it never replies to itself,
  - an 8-second per-agent cooldown,
  - it only engages when @-named (the lead also fields un-addressed human messages),
  - the model is told to answer with a bare "PASS" when it has nothing worth adding.

To use a different backend, point FLEETCHAT_CLAUDE at another command, or replace claude_reply().
"""
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "skill" / "fleet-chat"))
from fleetchat import Board  # noqa: E402

LIVE = os.environ.get("FLEETCHAT_LIVE") == "1"
CLAUDE = os.environ.get("FLEETCHAT_CLAUDE", "claude")
MODEL = os.environ.get("FLEETCHAT_MODEL", "")
COOLDOWN = 3.0


FOLDER = os.environ.get("FLEETCHAT_AGENT_DIR") or None  # a dynamically-added agent's project folder


def persona_base_dirs():
    """Persona lookup order (mirrors run.py): external $FLEETCHAT_PERSONAS_DIR, then personas.local/
    (git-ignored), then the committed personas/. Lets a personal fleet live outside the repo, or
    mix its own personas with the shipped demo ones."""
    dirs = []
    env = os.environ.get("FLEETCHAT_PERSONAS_DIR")
    if env:
        dirs.append(Path(env).expanduser())
    dirs.append(REPO / "personas.local")
    dirs.append(REPO / "personas")
    return dirs


def load_agent(name):
    d = None
    for base in persona_base_dirs():
        if (base / name / "agent.json").is_file():
            d = base / name
            break
    if d is not None:
        cfg = json.loads((d / "agent.json").read_text(encoding="utf-8"))
        pf = d / "PERSONA.md"
        return cfg, (pf.read_text(encoding="utf-8") if pf.exists() else "")
    # a dynamically-added agent (no persona folder) -- synthesize a generic one
    disp = name.capitalize()
    cfg = {"name": disp, "id": name, "role": "crew member",
           "intro": disp + " here, joining the board."}
    persona = ("You are " + disp + ", a member of a small agent crew on a team chat board. "
               "Be helpful, concise, and collaborative; reply in character.")
    if FOLDER:
        persona += " You are responsible for the project in your working folder; you can read its files for context."
    return cfg, persona


def agent_ids():
    seen, ids = set(), []
    for base in persona_base_dirs():
        if base.is_dir():
            for p in base.iterdir():
                if (p / "agent.json").is_file() and p.name not in seen:
                    seen.add(p.name); ids.append(p.name)
    return ids


def addressed(name, text):
    return re.search(r"(^|[\s(@])@?" + re.escape(name) + r"\b", text or "", re.I) is not None


def should_engage(cfg, msg, ids, is_lead):
    """Decide whether this agent should reply.
    - @-named -> always answer (talk to one agent directly).
    - an OPEN message from a human (no @name) -> every agent evaluates it; the model PASSes
      if it has nothing to add, so 1-2 naturally chime in and the crew feels alive.
    - another agent's message -> only if @-named, so the crew doesn't talk in circles."""
    if msg["sender"] == cfg["id"]:
        return False
    text = msg.get("text", "")
    if text.startswith("/vote") or text.startswith("/poll"):
        return False
    if addressed(cfg["id"], text):
        return True
    # an un-@-addressed message from a human (non-agent sender): everyone may weigh in
    if msg["sender"] not in ids and not any(addressed(a, text) for a in ids):
        return True
    return False


def claude_reply(cfg, persona, context):
    """One headless `claude` call, persona as system prompt. Returns the reply, or None to stay silent."""
    prompt = ("You are in a live team chat. Recent messages:\n\n" + context +
              "\n\n---\nYou are " + cfg["name"] + " (" + cfg.get("role", "") + "). Reply IN CHARACTER, "
              "warmly and briefly (1-3 sentences), IF: you're addressed by name, OR your lane is "
              "relevant, OR it's a greeting/opener a friendly crew member would naturally answer. "
              "markdown / `code` / links are fine. But if another member is clearly better placed and "
              "you'd just be echoing, reply with exactly: PASS and nothing else. Don't all pile on -- "
              "one or two good replies beat five.")
    cmd = [CLAUDE, "-p", prompt, "--system-prompt", persona[:6000]]
    if FOLDER:
        cmd += ["--add-dir", FOLDER]
    if MODEL:
        cmd += ["--model", MODEL]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8",
                             errors="replace", timeout=150).stdout.strip()
    except Exception:
        return None
    if not out or out.upper().rstrip(".!") == "PASS":
        return None
    return out


def respond_demo(cfg, persona, msg):
    return None  # demo agents just listen; --live makes them think


def main(name):
    cfg, persona = load_agent(name)
    ids = agent_ids()
    is_lead = cfg["id"] == os.environ.get("FLEETCHAT_LEAD", "lodestar")
    board = Board()
    intro = cfg.get("intro", cfg["name"] + " on the board.")
    board.post(cfg["id"], intro + ("  (live)" if LIVE else ""), tags=["join"])
    print("[%s] joined%s." % (cfg["id"], " (live)" if LIVE else ""))

    last = 0
    misses = 0
    last_reply = 0.0
    while True:  # re-arm forever; re-arming the watcher is how the agent stays responsive
        try:
            for m in board.watch(since=last, timeout=30):
                last = max(last, m["id"])
                if LIVE:
                    if should_engage(cfg, m, ids, is_lead) and (time.time() - last_reply) >= COOLDOWN:
                        ctx = board.messages(0)[-12:]
                        text = "\n".join((x["sender"] + ": " + x["text"]) for x in ctx)
                        reply = claude_reply(cfg, persona, text)
                        if reply:
                            board.post(cfg["id"], reply)
                            last_reply = time.time()
                else:
                    line = respond_demo(cfg, persona, m)
                    if line:
                        board.post(cfg["id"], line)
            misses = 0  # a clean watch cycle means the board is alive
        except KeyboardInterrupt:
            break
        except Exception:
            misses += 1
            if misses >= 30:  # board gone ~1 min -- self-exit instead of lingering as an orphan
                print("[%s] board unreachable; exiting." % cfg["id"])
                break
            time.sleep(2)


if __name__ == "__main__":
    _args = sys.argv[1:]
    if not _args:
        sys.exit("usage: python agents/run_agent.py <name> [--dir <folder>]")
    if "--dir" in _args:
        _i = _args.index("--dir")
        if _i + 1 < len(_args):
            FOLDER = _args[_i + 1]
    main(_args[0])
