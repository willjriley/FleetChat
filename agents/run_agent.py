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
  - a per-agent cooldown that DEFERS (never drops) whatever lands mid-turn,
  - it only engages when @-named (the lead also fields un-addressed human messages),
  - the model is told to answer with a bare "PASS" when it has nothing worth adding.

To use a different backend, point FLEETCHAT_CLAUDE at another command, or replace claude_reply().
"""
import json
import os
import re
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "skill" / "fleet-chat"))
from fleetchat import Board  # noqa: E402

LIVE = os.environ.get("FLEETCHAT_LIVE") == "1"
CLAUDE = os.environ.get("FLEETCHAT_CLAUDE", "claude")
MODEL = os.environ.get("FLEETCHAT_MODEL", "")
COOLDOWN = 3.0
REPLY_TIMEOUT = float(os.environ.get("FLEETCHAT_REPLY_TIMEOUT", "600"))

# --------------------------------------------------------------------------- #
# Memory mode -- an opt-in, per-agent dial that is OFF by default.            #
# --------------------------------------------------------------------------- #
# DEFAULT is bare `claude -p`: a fresh, stateless brain per message. Nothing is
# remembered, so nothing accumulates to clog the agent or bleed one project's
# concerns into another. This is the right default for a query-an-expert crew.
#
# Flip an agent ON (see in_memory_mode) and its replies run `claude -p` carrying
# that agent's OWN session (--session-id to create, --resume to continue), so it
# remembers across turns -- for a multi-turn event where continuity matters.
#
# Crucially, memory is ORTHOGONAL to monitoring: this run_agent.py process watches
# the board the whole time in BOTH modes. Memory only changes whether the brain-call
# carries state; it never changes whether the agent is watching. Each agent's session
# is keyed to its own name, so no two agents ever share a memory.
SESSION_NS = uuid.UUID("6ba7b812-9dad-11d1-80b4-00c04fd430c8")


def agent_session_id(name):
    """This agent's stable, private session id -- same every run, unique per agent."""
    return str(uuid.uuid5(SESSION_NS, "fleetchat:" + name))


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


def in_memory_mode(name):
    """Is THIS agent in persistent (memory) mode right now? Read FRESH each cycle, so flipping the
    book-icon toggle (the UI writes data/settings.json) takes effect on the next watch loop -- no
    restart. Default False => bare `claude -p` (clean default). Sources: FLEETCHAT_MEMORY env
    (all|1|* or a comma list of names) OR data/settings.json's "memory" {name: bool} map."""
    env = os.environ.get("FLEETCHAT_MEMORY", "").strip()
    if env in ("all", "1", "*"):
        return True
    if name in [x.strip() for x in env.split(",") if x.strip()]:
        return True
    sf = REPO / "data" / "settings.json"
    if sf.is_file():
        try:
            mem = json.loads(sf.read_text(encoding="utf-8")).get("memory")
        except Exception:
            mem = None
        if isinstance(mem, dict):
            return bool(mem.get(name))
    return False


def addressed(name, text):
    # The @ is REQUIRED: bare prose that happens to contain an agent name ("i hope so", "max effort") must not route -- with everyday-word
    # agent names, optional-@ both mis-engaged agents and suppressed the lead fallback.
    return re.search(r"(^|[\s(])@" + re.escape(name) + r"\b", text or "", re.I) is not None


def should_engage(cfg, msg, ids, is_lead):
    """Decide whether this agent should reply -- explicit routing, so a message only reaches
    who it's meant for (no cross-agent bleed):
    - @all           -> EVERY agent engages (explicit broadcast).
    - @name (1+ names) -> only those named agents engage (one-to-one or one-to-many, e.g. @aegis @keystone).
    - un-@-addressed human message -> the lead fields it (or, when no lead is designated -- the
      default add-your-own board -- any agent may, so a solo agent still answers; PASS/cooldown
      keep a crew from piling on).
    - another agent's message -> only if @-named, so the crew doesn't talk in circles."""
    if msg["sender"] == cfg["id"]:
        return False
    text = msg.get("text", "")
    if text.startswith("/vote") or text.startswith("/poll"):
        return False
    if re.search(r"(^|[\s(])@all\b", text, re.I):   # @all -> explicit broadcast to the whole crew
        return True
    if addressed(cfg["id"], text):                  # @-named (works for one or many names) -> just those
        return True
    # un-@-addressed message from a human (non-agent sender): the lead fields it -- and is_lead is
    # True for every agent when no lead is designated, so an add-your-own board is never silent
    if msg["sender"] not in ids and not any(addressed(a, text) for a in ids):
        return is_lead
    return False


# Redact-before-surface: runner stderr (and anything spooled to disk) can carry tokens/keys via
# error paths -- env dumps, auth failures, curl verbose. Scrub known credential SHAPES before the
# text leaves the runner. Deliberately narrow: bare hex (e.g. sha256 digests agents legitimately
# quote) is NOT masked; named key=value pairs keep the key and mask only the value.
_REDACT_TOKENS = [
    re.compile(r"\b(?:sk|rk)-[A-Za-z0-9_-]{16,}\b"),                                # api secret keys
    re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b"),                                  # github tokens
    re.compile(r"\bgithub_pat_[A-Za-z0-9_]{20,}\b"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),                                            # aws key id
    re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}\b"),                                # slack
    re.compile(r"\bAIza[0-9A-Za-z_-]{30,}\b"),                                      # google
    re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{5,}\b"),  # jwt
]
_REDACT_KV = re.compile(
    r"(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|secret|passw(?:or)?d|authorization|bearer)"
    r"(\s*[=:]\s*)(\S{6,})")


def redact(text):
    """Mask credential-shaped substrings; the surrounding text survives untouched."""
    if not text:
        return text
    for pat in _REDACT_TOKENS:
        text = pat.sub("[REDACTED]", text)
    return _REDACT_KV.sub(lambda m: m.group(1) + m.group(2) + "[REDACTED]", text)


def claude_reply(cfg, persona, context, session_id=None, state=None):
    """One headless `claude` call, persona as system prompt. Returns the reply, or None to stay
    silent. If session_id is given (memory mode), the call carries that session so the agent
    remembers across turns: --session-id creates it the first time, --resume continues it after.
    `state` is a tiny per-agent dict this uses to remember -- across calls, and across a PASS reply
    -- that the session now exists; if a prior process already created it, a create->resume fallback
    recovers. With no session_id it is the plain, stateless default: a fresh brain, no memory."""
    prompt = ("You are in a live team chat. Recent messages:\n\n" + context +
              "\n\n---\nYou are " + cfg["name"] + " (" + cfg.get("role", "") + "). Reply IN CHARACTER, "
              "warmly and briefly (1-3 sentences), IF: you're addressed by name, OR your lane is "
              "relevant, OR it's a greeting/opener a friendly crew member would naturally answer. "
              "markdown / `code` / links are fine. NEVER invent facts, status, or numbers you can't "
              "confirm from the messages above -- if asked something specific you don't actually know, "
              "say you'll check or defer to the lead rather than "
              "guessing. But if another member is clearly better placed and "
              "you'd just be echoing, reply with exactly: PASS and nothing else. Don't all pile on -- "
              "one or two good replies beat five.")
    base = [CLAUDE, "-p", prompt, "--system-prompt", persona[:6000]]
    if FOLDER:
        base += ["--add-dir", FOLDER]
    if MODEL:
        base += ["--model", MODEL]

    fail = {}

    def _run(extra):
        try:
            return subprocess.run(base + extra, capture_output=True, text=True,
                                  encoding="utf-8", errors="replace", timeout=REPLY_TIMEOUT)
        except subprocess.TimeoutExpired:
            fail["why"] = "TIMEOUT after %ds" % REPLY_TIMEOUT
            return None
        except Exception as e:
            fail["why"] = "FAILED to launch claude (%s)" % type(e).__name__
            return None

    if session_id:
        made = bool(state and state.get("made"))
        res = _run(["--resume", session_id] if made else ["--session-id", session_id])
        if (res is None or res.returncode != 0) and not made:
            res = _run(["--resume", session_id])   # a prior process already created it -> resume
        if res is not None and res.returncode == 0 and state is not None:
            state["made"] = True                   # the session exists now (remember, even on a PASS)
    else:
        res = _run([])                             # default: stateless, no memory

    if res is None or res.returncode != 0:
        # dead-man's-switch: never end a turn in silence -- post a terminal status instead
        why = fail.get("why") or ("FAILED (exit %s)" % (res.returncode if res is not None else "?"))
        err = redact(((res.stderr or "").strip()[-300:])) if res is not None else ""  # stderr can carry tokens/keys
        return ("⚠ headless turn %s -- task NOT completed." % why) + ((" stderr tail: `%s`" % err) if err else "")
    out = (res.stdout or "").strip()
    if not out or out.upper().rstrip(".!") == "PASS":
        return None
    return out


def respond_demo(cfg, persona, msg):
    return None  # demo agents just listen; --live makes them think


def main(name):
    cfg, persona = load_agent(name)
    ids = agent_ids()
    # A designated lead (set by --demo, or FLEETCHAT_LEAD) fields un-@-addressed human messages so a
    # defined crew doesn't all pile on. On the default add-your-own board there is NO designated lead
    # -> every agent may field an open message: a solo agent just answers (never a silent board), and
    # the PASS reply + per-agent cooldown keep a larger crew from piling on.
    lead = os.environ.get("FLEETCHAT_LEAD")
    if not lead:  # honor a designated lead on the REAL board too (was effectively --demo-only)
        lf = REPO / "fleet.local.json"
        if lf.is_file():
            try:
                lead = (json.loads(lf.read_text(encoding="utf-8")) or {}).get("lead")
            except Exception:
                lead = None
    is_lead = (cfg["id"] == lead) if lead else True
    board = Board()
    intro = cfg.get("intro", cfg["name"] + " on the board.")
    joined = board.post(cfg["id"], intro + ("  (live)" if LIVE else ""), tags=["join"])
    print("[%s] joined%s." % (cfg["id"], " (live)" if LIVE else ""))

    # Resume from the last message this agent actually SAW (persisted pointer) so a restart
    # doesn't silently skip everything posted while the stack was down. The deferred-engage
    # queue below collapses any backlog into ONE turn, so resuming can't churn claude calls.
    # A huge gap (>100 msgs) falls back to the join point -- that's a fresh start, not a restart.
    last = int(joined.get("id", 0)) if isinstance(joined, dict) else 0
    seenf = REPO / "data" / ("seen-%s.json" % cfg["id"])
    if seenf.is_file():
        try:
            stored = int((json.loads(seenf.read_text(encoding="utf-8")) or {}).get("last", 0))
            if stored and 0 <= last - stored <= 100:
                last = stored
        except Exception:
            pass

    misses = 0
    last_reply = 0.0
    mem_state = {}  # per-agent, persists for this process's life: tracks its memory session
    pending = []    # engage-worthy messages DEFERRED (never dropped) while the cooldown runs

    def post_with_retry(text, attempts=3):
        """A finished reply must never die at the final step: retry transient post failures,
        then spool to data/outbox-<agent>.txt as the last resort (drained next cycle)."""
        for i in range(attempts):
            try:
                return board.post(cfg["id"], text)
            except Exception:
                time.sleep(2 * (i + 1))
        try:
            with open(REPO / "data" / ("outbox-%s.txt" % cfg["id"]), "a", encoding="utf-8") as f:
                f.write(text + "\n---\n")
        except Exception:
            pass
        return None

    def drain_outbox():
        f = REPO / "data" / ("outbox-%s.txt" % cfg["id"])
        if f.is_file():
            try:
                body = f.read_text(encoding="utf-8").strip()
                if body:  # post FIRST, unlink only on success -- the recovery path must not itself eat the reply
                    board.post(cfg["id"], "(recovered reply -- original post failed)\n" + body[-4000:])
                f.unlink()
            except Exception:
                pass

    def engage(anchor_id):
        nonlocal last_reply
        # memory mode is read FRESH here, so a toggle takes effect on the next engage
        sid = agent_session_id(cfg["id"]) if in_memory_mode(cfg["id"]) else None
        # Context is ANCHORED at the oldest unanswered trigger -- never a blind newest-tail
        # slice -- so a slow turn can't scroll its own trigger (or a crossing reply, or a
        # memory-mode gap) out of view. Floor keeps the old minimum; cap bounds mega-bursts.
        allm = board.messages(0)
        idx = next((i for i, x in enumerate(allm) if x["id"] >= anchor_id), max(len(allm) - 1, 0))
        window = allm[max(0, idx - 3):]
        floor = 4 if sid else 12
        if len(window) < floor:
            window = allm[-floor:]
        window = window[-60:]
        text = "\n".join((x["sender"] + ": " + x["text"]) for x in window)
        stop = threading.Event()

        def _keep_typing():  # typing TTL is 180s server-side; a turn may legally run far longer
            while not stop.wait(60):
                board.set_typing(cfg["id"], True)

        board.set_typing(cfg["id"], True)      # animated … in the UI while the model thinks
        threading.Thread(target=_keep_typing, daemon=True).start()
        try:
            reply = claude_reply(cfg, persona, text, session_id=sid, state=mem_state)
        finally:
            stop.set()
            board.set_typing(cfg["id"], False)
        last_reply = time.time()  # cool down after EVERY engage (even a PASS) so a burst
        if reply:                 # can't rapid-fire claude / flicker the typing …
            post_with_retry(reply)

    while True:  # re-arm forever; re-arming the watcher is how the agent stays responsive
        try:
            drain_outbox()
            for m in board.watch(since=last, timeout=30):
                last = max(last, m["id"])
                if LIVE:
                    if should_engage(cfg, m, ids, is_lead):
                        pending.append(m)   # DEFER -- the cooldown gate moved below, out of the drop path
                else:
                    line = respond_demo(cfg, persona, m)
                    if line:
                        board.post(cfg["id"], line)
            try:
                seenf.write_text(json.dumps({"last": last}), encoding="utf-8")
            except Exception:
                pass
            if LIVE and pending:
                # ONE collapsed turn answers everything that queued during the previous engage:
                # deferred, never dropped -- and a burst still costs one claude call, not N.
                gap = COOLDOWN - (time.time() - last_reply)
                if gap > 0:
                    time.sleep(gap)
                anchor_id = pending[0]["id"]
                engage(anchor_id)
                pending = []  # cleared only AFTER a completed engage; a thrown engage retries next cycle
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
