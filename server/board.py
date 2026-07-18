#!/usr/bin/env python3
"""
FleetChat board server -- the async message board at the heart of a FleetChat crew.

A tiny, zero-dependency HTTP server over an append-only JSONL log. Agents (and the
web UI) POST messages and poll for everything since a given id. That is the whole
coordination substrate: no database, no broker, no cloud -- just this file + the
Python standard library.

SECURITY BY CONSTRUCTION (see ../docs/SECURITY.md):
  - Binds to 127.0.0.1 by default. A default FleetChat is a SEALED LOCAL fleet:
    nothing is reachable from another machine. A browser on the SAME machine still
    can reach loopback, though -- that's why cross-origin writes are gated too
    (see _origin_ok below).
  - Going cross-host is a single, explicit opt-in. Binding to any non-loopback
    address REQUIRES a shared token -- this server REFUSES to start otherwise.
    The switch that exposes the port is the same switch that turns on the gate;
    there is no footgun path to an open, unauthenticated board.
  - The token is never shipped in the repo. Generate one at setup and pass it in
    via config/env; the join skill reads it from the environment. No secret is
    baked into any file that gets committed.

Run it directly (`python server/board.py`) or, more usually, via the top-level
`run.py`, which also brings up the example agents.
"""

import hmac
import json
import os
import re
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse, parse_qs

BASE = Path(__file__).resolve().parent          # .../FleetChat/server
REPO = BASE.parent                              # .../FleetChat
WEB = BASE / "web"
DATA = REPO / "data"
BOARD_FILE = DATA / "board.jsonl"

LOOPBACK = {"127.0.0.1", "localhost", "::1"}


def host_part(host_header):
    """The host portion of a Host header, minus any port (handles [::1]:port)."""
    h = (host_header or "").strip()
    if h.startswith("["):
        return h[1:h.index("]")] if "]" in h else h
    return h.rsplit(":", 1)[0] if ":" in h else h


def allowed_hosts(cfg):
    """Host names this board legitimately answers to. Anything else is a DNS-rebinding
    attempt or a misdirected request, and is refused. Loopback is always allowed; a
    non-wildcard bind adds itself; FLEETCHAT_ALLOWED_HOSTS adds more (comma-separated)."""
    hosts = set(LOOPBACK)
    bind = str(cfg.get("bind", ""))
    if bind and bind not in ("0.0.0.0", "::"):
        hosts.add(bind)
    for h in os.environ.get("FLEETCHAT_ALLOWED_HOSTS", "").split(","):
        if h.strip():
            hosts.add(h.strip())
    return hosts


# --------------------------------------------------------------------------- #
# Crew registry (data/run.pids: "name pid" per line) -- used by the control    #
# endpoints to boot a member or record an added one. Names only, never paths.  #
# --------------------------------------------------------------------------- #
def read_crew():
    crew = {}
    f = DATA / "run.pids"
    if f.exists():
        for line in f.read_text(encoding="utf-8").splitlines():
            parts = line.split()
            if len(parts) >= 2 and parts[-1].isdigit():
                crew[parts[0]] = int(parts[-1])
    return crew


def read_roster():
    """The configured lineup for the sidebar: the PERSISTED roster.json entries, each resolved to
    {id, name, role} via its persona (personas.local/ then personas/). ALL agents equal -- no forced
    lead/star; add a leader only if you want one. Read-only; safe on a networked board."""
    pbases = []
    pd = os.environ.get("FLEETCHAT_PERSONAS_DIR")
    if pd:
        pbases.append(Path(pd).expanduser())
    pbases += [REPO / "personas.local", REPO / "personas"]
    roster = []
    for entry in read_roster_list():                    # the durable data/roster.json lineup
        cid = str(entry.get("name", ""))
        if not re.fullmatch(r"[a-z0-9_-]+", cid):
            continue
        name, role = cid.capitalize(), ""
        for base in pbases:
            aj = base / cid / "agent.json"
            if aj.is_file():
                try:
                    d = json.loads(aj.read_text(encoding="utf-8"))
                    name, role = d.get("name", name), d.get("role", "")
                except Exception:
                    pass
                break
        roster.append({"id": cid, "name": name, "role": role})
    return roster


def write_crew(crew):
    DATA.mkdir(parents=True, exist_ok=True)
    (DATA / "run.pids").write_text("\n".join("%s %d" % (n, p) for n, p in crew.items()), encoding="utf-8")


# --------------------------------------------------------------------------- #
# Persisted lineup (data/roster.json) -- the configured crew a RESTART re-launches. #
# run.pids is ephemeral (live PIDs); THIS is the durable "who's on the team" list.   #
# Git-ignored (lives under data/), so it survives open/close but never enters the    #
# shared repo. + Add agent appends here; the x button removes; run.py reads it on boot.#
# --------------------------------------------------------------------------- #
ROSTER_FILE = DATA / "roster.json"


def read_roster_list():
    """The persisted lineup: [{"name": str, "dir"?: str}, ...]. Corruption never crashes the board."""
    if ROSTER_FILE.is_file():
        try:
            d = json.loads(ROSTER_FILE.read_text(encoding="utf-8"))
            return [x for x in d if isinstance(x, dict) and x.get("name")] if isinstance(d, list) else []
        except Exception:
            return []
    return []


def write_roster_list(items):
    DATA.mkdir(parents=True, exist_ok=True)
    ROSTER_FILE.write_text(json.dumps(items, indent=2, ensure_ascii=False), encoding="utf-8")


def roster_add(name, folder=None):
    """Add an agent to the persisted lineup (idempotent by name), so a restart re-launches it."""
    items = read_roster_list()
    if not any(i.get("name") == name for i in items):
        entry = {"name": name}
        if folder:
            entry["dir"] = str(folder)
        items.append(entry)
        write_roster_list(items)


def roster_remove(name):
    """Drop an agent from the persisted lineup so the next restart will NOT bring it back."""
    write_roster_list([i for i in read_roster_list() if i.get("name") != name])


CREW_LOCK = threading.Lock()   # serializes read-modify-write of run.pids across add/kick/respawn


def kill_pid(pid):
    """Stop a crew member AND its process tree. An in-flight model call is a CHILD of the
    watcher -- killing only the watcher orphans that call for up to the full reply timeout.
    Windows: verify the pid still belongs to python.exe (pid-recycling guard), then /T the
    whole tree. POSIX: single-pid SIGTERM only (the server shares the process group, so a
    killpg would take the board down with it; children may briefly outlive the watcher)."""
    try:
        if sys.platform == "win32":
            chk = subprocess.run(["tasklist", "/FI", "PID eq %d" % int(pid), "/FO", "CSV", "/NH"],
                                 capture_output=True, text=True)
            if "python.exe" not in (chk.stdout or ""):
                return                       # recycled or already gone -- never tree-kill a stranger
            subprocess.run(["taskkill", "/F", "/T", "/PID", str(pid)], capture_output=True)
        else:
            os.kill(pid, 15)
    except Exception:
        pass


# --------------------------------------------------------------------------- #
# Per-agent memory toggle -- persisted in data/settings.json (git-ignored).     #
# Self-contained: NO coupling to any fleet-config file, so the default          #
# empty-board flow never creates config files. run_agent.py reads it fresh      #
# each cycle, so a flip here takes effect on the agent's next message.          #
# --------------------------------------------------------------------------- #
SETTINGS_FILE = DATA / "settings.json"


def _settings_read():
    if SETTINGS_FILE.is_file():
        try:
            d = json.loads(SETTINGS_FILE.read_text(encoding="utf-8"))
            return d if isinstance(d, dict) else {}
        except Exception:
            return {}
    return {}


def memory_read():
    """The per-agent memory map {name: bool} from data/settings.json."""
    mem = _settings_read().get("memory")
    return {k: bool(v) for k, v in mem.items()} if isinstance(mem, dict) else {}


def memory_write(agent, on):
    """Flip one agent's memory flag in data/settings.json, preserving other keys."""
    data = _settings_read()
    mem = data.get("memory")
    if not isinstance(mem, dict):
        mem = {}
    mem[agent] = bool(on)
    data["memory"] = mem
    DATA.mkdir(parents=True, exist_ok=True)
    SETTINGS_FILE.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
    return {k: bool(v) for k, v in mem.items()}


# --------------------------------------------------------------------------- #
# Per-agent CLI-model override -- same shape as the memory toggle: persisted in
# data/settings.json, read FRESH each cycle by the runner, so a change here
# takes effect on an agent's NEXT turn with no restart (unlike FLEETCHAT_MODEL,
# a module-level env var baked in once at process start).
# --------------------------------------------------------------------------- #
def model_read():
    """{name: model_id} for every agent with an override set."""
    m = _settings_read().get("model")
    return {k: str(v) for k, v in m.items() if v} if isinstance(m, dict) else {}


def model_write(agent, model):
    """Set (or, given an empty string, clear) one agent's model override."""
    data = _settings_read()
    m = data.get("model")
    if not isinstance(m, dict):
        m = {}
    model = str(model or "").strip()
    if model:
        m[agent] = model
    else:
        m.pop(agent, None)
    data["model"] = m
    DATA.mkdir(parents=True, exist_ok=True)
    SETTINGS_FILE.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
    return {k: str(v) for k, v in m.items()}


# --------------------------------------------------------------------------- #
# Per-agent CLI invocation template -- for pointing an agent at a CLI that     #
# isn't Claude-Code-shaped. Same live, no-restart pattern as model overrides:  #
# persisted in settings.json, read fresh every engage. A template is a LIST OF #
# ARGV TOKENS (never a shell string) with {bin}/{prompt}/{persona}/{model}     #
# placeholders substituted per-token, then executed via subprocess's list form #
# (shell=False) -- the same parameterization SQL prepared statements use:      #
# structure (which token is a flag) and data (chat content) never share a      #
# channel a parser could conflate, so no substituted value -- however it's     #
# been crafted -- can ever be reinterpreted as another flag or shell syntax.   #
# v1 is intentionally narrow: stateless only (no --resume/--session-id -- see  #
# agent_model's docstring in run_agent.py for why memory mode doesn't apply    #
# here yet), and {prompt} is required so an agent can never be left mute.      #
# --------------------------------------------------------------------------- #
CLI_TEMPLATE_TOKEN_RE = re.compile(r"^[^\x00-\x1f]{1,4096}$")   # any char but control chars; generous length


def cli_template_read():
    """{name: [tokens...]} for every agent with a custom CLI invocation set."""
    t = _settings_read().get("cli_template")
    if not isinstance(t, dict):
        return {}
    out = {}
    for k, v in t.items():
        if isinstance(v, list) and v and all(isinstance(x, str) for x in v):
            out[k] = v
    return out


def cli_template_write(agent, tokens):
    """Set (or, given an empty list, clear) one agent's CLI template. Validates: a list of
    strings, {prompt} present as its OWN token (so the agent can never be silently muted),
    each token charset/length-bounded. Returns (result_map, error) -- error is None on success."""
    if tokens:
        if not (isinstance(tokens, list) and all(isinstance(x, str) for x in tokens)):
            return None, "template must be a list of strings (one argv token each)"
        if "{prompt}" not in tokens:
            # Substitution (run_agent.py) is an EXACT-token dict lookup, never a substring
            # replace -- a token like "-p={prompt}" is not the key "{prompt}", so it would
            # survive substitution unchanged and the real prompt would never reach the agent.
            # This check has to require the same exactness the substitution actually uses,
            # or "validated" and "will actually substitute" silently diverge.
            return None, "template must include {prompt} as its own token (not glued to another flag)"
        for x in tokens:
            if not CLI_TEMPLATE_TOKEN_RE.match(x):
                return None, "a token is empty, over 4096 chars, or has a control character"
    data = _settings_read()
    t = data.get("cli_template")
    if not isinstance(t, dict):
        t = {}
    if tokens:
        t[agent] = list(tokens)
    else:
        t.pop(agent, None)
    data["cli_template"] = t
    DATA.mkdir(parents=True, exist_ok=True)
    SETTINGS_FILE.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
    return {k: list(v) for k, v in t.items()}, None


def tts_muted_read():
    """Board-wide TTS mute flag (settings.json). A server-side speaker reads this to decide whether
    to voice agent replies, so the UI's mute button gates the actual (server) speech, not the browser."""
    return bool(_settings_read().get("tts_muted"))


def tts_muted_write(muted):
    data = _settings_read()
    data["tts_muted"] = bool(muted)
    DATA.mkdir(parents=True, exist_ok=True)
    SETTINGS_FILE.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
    return bool(muted)


VOICE_MODES = ("auto", "server-only")


def voice_mode_read():
    """"auto" (default) lets the browser fall back to its own voices when no server speaker has
    heartbeated recently -- the right default for a fresh kit clone with no engine installed yet.
    "server-only" is a hard switch for an install that KNOWS the real engine is always present
    (e.g. one where downloading it is part of setup): the browser fallback code path goes
    permanently inert, so the whole browser-vs-server interaction-bug class (heartbeat timing,
    theme desync, process-supervision gaps -- every one we chased tonight) cannot exist, full stop.
    Silence during a real speaker outage is the deliberate trade -- never a wrong voice."""
    m = _settings_read().get("voice_mode")
    return m if m in VOICE_MODES else "auto"


def voice_mode_write(mode):
    mode = mode if mode in VOICE_MODES else "auto"
    data = _settings_read()
    data["voice_mode"] = mode
    DATA.mkdir(parents=True, exist_ok=True)
    SETTINGS_FILE.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
    return mode


# --------------------------------------------------------------------------- #
# Per-agent server-speaker VOICE map -- data/voices.json ({name: voice_id}).    #
# Its OWN file (not settings.json): the optional server-side TTS speaker reads   #
# it directly to pick each agent's voice. Same git-ignored data/ home, same      #
# read-merge-write discipline as the memory toggle, so setting one agent's voice  #
# never clobbers another's. Setting the voice to "off" removes the entry.         #
# --------------------------------------------------------------------------- #
VOICES_FILE = DATA / "voices.json"

# The known English voice ids in the v1.0 voice pack. Served by GET /control/voices
# as a static pick-list so the board never has to import or probe the (optional) speech
# engine -- keeping this server dependency-free. Update by hand if the pack changes.
VOICE_PACK_V1 = [
    "af_alloy", "af_aoede", "af_bella", "af_heart", "af_jessica", "af_kore",
    "af_nicole", "af_nova", "af_river", "af_sarah", "af_sky",
    "am_adam", "am_echo", "am_eric", "am_fenrir", "am_liam", "am_michael",
    "am_onyx", "am_puck", "am_santa",
    "bf_alice", "bf_emma", "bf_isabella", "bf_lily",
    "bm_daniel", "bm_fable", "bm_george", "bm_lewis",
]


def voices_read():
    """The per-agent voice map {name: voice_id} from data/voices.json. Corruption never crashes."""
    if VOICES_FILE.is_file():
        try:
            d = json.loads(VOICES_FILE.read_text(encoding="utf-8"))
            return {str(k): str(v) for k, v in d.items()} if isinstance(d, dict) else {}
        except Exception:
            return {}
    return {}


def voices_write(agent, voice):
    """Set one agent's voice, or remove it when voice == 'off', preserving every other entry."""
    data = voices_read()
    if voice == "off":
        data.pop(agent, None)
    else:
        data[agent] = voice
    DATA.mkdir(parents=True, exist_ok=True)
    VOICES_FILE.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
    return data
# Thread ledger -- the task board behind the Kanban view (data/threads.json,   #
# git-ignored). Every actionable task is a card: who opened it, who OWNS it    #
# (the claim = the soft lock against double-dispatch), who else is assigned,   #
# which lane it's in, and a liveness heartbeat so a dead owner's claim visibly #
# expires and the card becomes adoptable. Per-agent identity lives here, so    #
# the endpoints ride the AUTHED view only -- the public poll never changes.    #
# --------------------------------------------------------------------------- #
THREADS_FILE = DATA / "threads.json"
THREADS_LOCK = threading.Lock()
THREAD_LANES = ("backlog", "open", "claimed", "review", "done")
THREADS_CAP = 400          # sanity bound; oldest done cards are pruned past this
THREAD_HEARTBEAT_TTL = 300  # a claim with no heartbeat for 5 min reads as stale/adoptable


def _threads_read():
    if THREADS_FILE.is_file():
        try:
            d = json.loads(THREADS_FILE.read_text(encoding="utf-8"))
            if isinstance(d, dict) and isinstance(d.get("threads"), list):
                return d
        except Exception:
            pass
        # Corrupt/truncated ledger: QUARANTINE it (never silently treat it as empty -- the next
        # write would permanently overwrite every card). The bad file stays recoverable on disk.
        try:
            THREADS_FILE.replace(THREADS_FILE.with_name("threads.json.bad-%d" % int(time.time())))
        except Exception:
            pass
    return {"next": 1, "threads": []}


def _threads_write(d):
    DATA.mkdir(parents=True, exist_ok=True)
    # Atomic: write a temp file, then os.replace -- a crash mid-write leaves either the whole old
    # file or the whole new one, never a truncated ledger.
    tmp = THREADS_FILE.with_name("threads.json.tmp")
    tmp.write_text(json.dumps(d, indent=1, ensure_ascii=False), encoding="utf-8")
    os.replace(tmp, THREADS_FILE)


def threads_list():
    """All cards, oldest first, each annotated with a computed 'stale' flag (claimed but no
    heartbeat inside the TTL) so the UI can amber a dying claim without its own clock math."""
    with THREADS_LOCK:
        d = _threads_read()
    now = time.time()
    for t in d["threads"]:
        t["stale"] = bool(t.get("status") == "claimed" and now - t.get("heartbeat", 0) > THREAD_HEARTBEAT_TTL)
    return d["threads"]


def thread_op(op, data):
    """One serialized mutation on the ledger. Returns (result, error, http_status).
    Ops: create{title, by} · claim{id, agent} (also adopts a stale claim) · release{id, agent}
    · assign/unassign{id, agent} · status{id, lane} · heartbeat{id, agent} · close{id, summary?}."""
    title = str(data.get("title", ""))[:200].strip()
    agent = (data.get("agent") or data.get("by") or "").strip()
    tid = str(data.get("id", ""))
    if agent and not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", agent):
        return None, "bad agent name", 400
    with THREADS_LOCK:
        d = _threads_read()
        if op == "create":
            if not title:
                return None, "a title is required", 400
            card = {"id": "t%d" % d["next"], "title": title, "opened_by": agent or "?",
                    "owner": None, "assignees": [], "status": "open", "heartbeat": 0,
                    "created": time.time(), "updated": time.time(), "summary": ""}
            d["next"] += 1
            d["threads"].append(card)
            if len(d["threads"]) > THREADS_CAP:  # prune oldest DONE cards first, never live ones
                done = [t for t in d["threads"] if t.get("status") == "done"]
                for old in done[:len(d["threads"]) - THREADS_CAP]:
                    d["threads"].remove(old)
            _threads_write(d)
            return card, None, 201
        t = next((x for x in d["threads"] if x.get("id") == tid), None)
        if t is None:
            return None, "no such thread", 404
        now = time.time()
        if op == "claim":
            if not agent:
                return None, "an agent is required", 400
            fresh = t.get("owner") and t.get("status") == "claimed" and now - t.get("heartbeat", 0) <= THREAD_HEARTBEAT_TTL
            if fresh and t["owner"] != agent:
                return None, "already claimed by %s (heartbeat fresh)" % t["owner"], 409
            prev = t.get("owner")                       # capture BEFORE the update overwrites it
            adopted = bool(prev) and prev != agent
            t.update(owner=agent, status="claimed", heartbeat=now, updated=now)
            if adopted:
                t["adopted_from"] = prev                # provenance: whose stale claim this adopts
            else:
                t.pop("adopted_from", None)
        elif op == "release":
            if t.get("owner") == agent or not t.get("owner"):
                t.update(owner=None, status="open", updated=now)
            else:
                return None, "only the owner releases a live claim", 403
        elif op == "assign":
            if agent and agent not in t.setdefault("assignees", []):
                t["assignees"].append(agent)
            t["updated"] = now
        elif op == "unassign":
            if agent in t.get("assignees", []):
                t["assignees"].remove(agent)
            t["updated"] = now
        elif op == "status":
            lane = str(data.get("lane", ""))
            if lane not in THREAD_LANES:
                return None, "lane must be one of %s" % (THREAD_LANES,), 400
            if lane == "claimed":
                if not t.get("owner"):
                    return None, "claim it instead -- 'claimed' needs an owner", 400
                t["heartbeat"] = now       # moving back to working counts as life; never instantly stale
            t["status"] = lane
            if lane in ("open", "backlog"):
                t["owner"] = None
            t["updated"] = now
        elif op == "heartbeat":
            if t.get("owner") == agent:
                t["heartbeat"] = now
            t["updated"] = now
        elif op == "edit":
            if title:                                          # reuses the validated/capped title
                t["title"] = title
            if data.get("desc") is not None:
                t["desc"] = str(data.get("desc", ""))[:1000].strip()
            t["updated"] = now
        elif op == "close":
            t.update(status="done", updated=now, closed=now)   # closed = the durable done-date
            s = str(data.get("summary", ""))[:500].strip()
            if s:
                t["summary"] = s
        else:
            return None, "unknown op", 400
        _threads_write(d)
        return t, None, 200


# --------------------------------------------------------------------------- #
# Config + the security coupling                                              #
# --------------------------------------------------------------------------- #
def load_config():
    """Config precedence: environment overrides > config.json > safe defaults.

    Defaults are the SEALED LOCAL profile: loopback, no token needed.
    """
    cfg = {"bind": "127.0.0.1", "port": 8137, "require_token": False, "token": None}

    cfg_path = REPO / "config.json"
    if cfg_path.exists():
        try:
            cfg.update(json.loads(cfg_path.read_text(encoding="utf-8")))
        except Exception as e:
            sys.exit(f"[board] config.json is not valid JSON: {e}")

    # Environment always wins (this is where a generated token belongs -- never a file).
    cfg["bind"] = os.environ.get("FLEETCHAT_BIND", cfg["bind"])
    if os.environ.get("FLEETCHAT_PORT"):
        cfg["port"] = int(os.environ["FLEETCHAT_PORT"])
    if os.environ.get("FLEETCHAT_TOKEN"):
        cfg["token"] = os.environ["FLEETCHAT_TOKEN"]
        cfg["require_token"] = True
    return cfg


def enforce_security(cfg):
    """The one rule that keeps the footgun off the happy path:
    binding anywhere but loopback REQUIRES a token. No exceptions."""
    bind = str(cfg.get("bind", "127.0.0.1"))
    is_local = bind in LOOPBACK
    token = cfg.get("token")
    if not is_local:
        cfg["require_token"] = True  # exposing the port turns the gate ON, coupled
        if not token:
            sys.exit(
                "\n[board] REFUSING TO START: bind is '%s' (not loopback) but no token is set.\n"
                "        Cross-host exposure and the auth gate are the SAME switch.\n"
                "        Generate a token and set FLEETCHAT_TOKEN, or bind to 127.0.0.1.\n"
                "        See docs/SECURITY.md.\n" % bind
            )
    return cfg


# --------------------------------------------------------------------------- #
# Board state -- append-only JSONL, loaded once, guarded by a lock            #
# --------------------------------------------------------------------------- #
class Board:
    def __init__(self):
        self._lock = threading.Lock()
        self._messages = []
        self._next_id = 1
        self._typing = {}          # agent id -> last "composing" ping ts (drives the UI's animated …)
        self._speaking = {}        # agent id -> last "being voiced" ping ts (drives the UI's 🔊 pulse)
        self._speaker_seen = 0     # last heartbeat from a server-side TTS speaker -> browser-TTS auto-detect
        DATA.mkdir(parents=True, exist_ok=True)
        if BOARD_FILE.exists():
            for line in BOARD_FILE.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if not line:
                    continue
                try:
                    m = json.loads(line)
                    self._messages.append(m)
                    self._next_id = max(self._next_id, int(m["id"]) + 1)
                except Exception:
                    pass  # a corrupt line never takes the board down

    def post(self, sender, text, tags):
        with self._lock:
            msg = {
                "id": self._next_id,
                "sender": str(sender)[:64],
                "text": str(text),
                "tags": [str(t)[:32] for t in (tags or [])][:12],
                "ts": time.time(),
            }
            self._next_id += 1
            self._messages.append(msg)
            with BOARD_FILE.open("a", encoding="utf-8") as fh:
                fh.write(json.dumps(msg, ensure_ascii=False) + "\n")
            return msg

    def since(self, since_id):
        with self._lock:
            return [m for m in self._messages if m["id"] > since_id]

    TYPING_CAP = 64   # far above any real crew; bounds the dict so a valid-token flood can't balloon it

    def set_typing(self, agent, on):
        """Mark an agent as composing (on) or done (off) -- drives the animated … in the UI.
        Bounded: at the cap, stale pings are pruned first, then new names are ignored (live
        entries self-expire via typing_now's ttl), so a flood of distinct names can't grow it."""
        with self._lock:
            if not on:
                self._typing.pop(agent, None)
                return
            if agent not in self._typing and len(self._typing) >= self.TYPING_CAP:
                now = time.time()
                self._typing = {a: t for a, t in self._typing.items() if now - t <= 180}
            if agent in self._typing or len(self._typing) < self.TYPING_CAP:
                self._typing[agent] = time.time()

    def typing_now(self, ttl=180):
        """Ids composing right now. Pings older than ttl are dropped, so a responder that
        dies mid-reply never leaves a … stuck on the board."""
        now = time.time()
        with self._lock:
            return sorted(a for a, ts in self._typing.items() if now - ts <= ttl)

    def set_speaking(self, agent, on):
        """Mark an agent's reply as being voiced right now (the 🔊 pulse in the sidebar).
        Same bounded pattern as set_typing, sharing TYPING_CAP."""
        with self._lock:
            if not on:
                self._speaking.pop(agent, None)
                return
            if agent not in self._speaking and len(self._speaking) >= self.TYPING_CAP:
                now = time.time()
                self._speaking = {a: t for a, t in self._speaking.items() if now - t <= 90}
            if agent in self._speaking or len(self._speaking) < self.TYPING_CAP:
                self._speaking[agent] = time.time()

    def speaking_now(self, ttl=90):
        """Ids being voiced right now; stale pings drop so a speaker that dies mid-clip
        never leaves a 🔊 stuck on."""
        now = time.time()
        with self._lock:
            return sorted(a for a, ts in self._speaking.items() if now - ts <= ttl)

    def clear(self):
        """Wipe the board history -- in-memory + the JSONL. _next_id keeps climbing so message ids
        never repeat, and clients tracking a last-id still receive everything posted after the clear."""
        with self._lock:
            self._messages = []
            try:
                BOARD_FILE.write_text("", encoding="utf-8")
            except Exception:
                pass

    def speaker_ping(self):
        """A server-side TTS speaker announcing it's alive -> the UI drops its browser fallback so
        the two don't double up."""
        with self._lock:
            self._speaker_seen = time.time()

    def speaker_active(self, ttl=30):
        with self._lock:
            return (time.time() - self._speaker_seen) <= ttl


# --------------------------------------------------------------------------- #
# HTTP handler                                                                #
# --------------------------------------------------------------------------- #
class Handler(BaseHTTPRequestHandler):
    board = None      # injected in main()
    cfg = None        # injected in main()
    allowed = set()   # injected in main() -- host names we answer to
    control = False   # injected in main() -- whether the /shutdown control endpoint is enabled
    server_version = "FleetChat/1.0"

    def log_message(self, fmt, *args):
        # One quiet line; a starter kit should not spew.
        sys.stderr.write("[board] %s - %s\n" % (self.address_string(), fmt % args))

    # -- helpers ------------------------------------------------------------ #
    def _send_json(self, obj, status=200):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authed(self):
        """Token gate for the API. Open when require_token is False (local mode).
        Constant-time compare so the token cannot be recovered by request timing."""
        if not self.cfg.get("require_token"):
            return True
        return hmac.compare_digest(self.headers.get("X-Fleet-Token", ""), self.cfg.get("token") or "")

    def _host_ok(self):
        """Defeats DNS-rebinding: the Host header must name a host we actually serve.
        A page that rebinds its own domain to 127.0.0.1 still sends its domain here."""
        return host_part(self.headers.get("Host", "")) in self.allowed

    def _origin_ok(self):
        """Defeats CSRF-to-localhost. FAILS CLOSED: a state-change is allowed only when at least
        one of three independent, hard-to-forge signals says so -- absence of a header is never
        itself proof of anything, because a bare HTML <form> (the entire CSRF surface) trivially
        omits any header it likes.
          1. A valid same-host Origin/Referer -- genuine browser navigation/fetch.
          2. X-Fleet-Client -- every real client (the UI, the join skill, agents, the speaker)
             sends this. A plain <form> POST cannot set a custom header at all; a script that
             tries via fetch() triggers a CORS preflight this server never approves, so the
             browser blocks the real request before it's sent.
          3. Sec-Fetch-Site: same-origin|none -- a newer Fetch-Metadata header the BROWSER sets
             from ground truth; no page content can spoof it into claiming same-origin falsely.
        (Earlier shape here allowed any request with NO Origin/Referer through at all, reasoning
        that only non-browser clients send neither -- true in 2016, not once a well-known
        text/plain-enctype form + Referrer-Policy: no-referrer can suppress both from a real
        browser. Closed by requiring a signal to be PRESENT, never inferring from absence.)"""
        if self.headers.get("X-Fleet-Client"):
            return True
        sfs = self.headers.get("Sec-Fetch-Site", "")
        if sfs in ("same-origin", "none"):
            return True
        src = self.headers.get("Origin") or self.headers.get("Referer")
        if src:
            return (urlparse(src).hostname or "") in self.allowed
        return False

    def _delayed_exit(self, code=0):
        """/shutdown (0) or /control/restart (42): let the response flush, then exit. run.py sees
        the board go down and its cleanup() stops the agent processes -- and on exit code 42 it
        relaunches the whole stack fresh instead of staying down."""
        time.sleep(0.4)
        os._exit(code)

    def _read_json(self):
        length = int(self.headers.get("Content-Length", 0) or 0)
        if length <= 0 or length > 64 * 1024:
            return None
        try:
            return json.loads(self.rfile.read(length).decode("utf-8"))
        except Exception:
            return None

    # -- routes ------------------------------------------------------------- #
    def do_GET(self):
        if not self._host_ok():
            return self._send_json({"error": "bad host"}, 403)
        route = urlparse(self.path)
        if route.path in ("/", "/index.html"):
            return self._serve_ui()
        if route.path == "/tasks":
            return self._serve_ui(WEB / "tasks.html")   # the full-page task board (data stays authed)
        if route.path == "/health":
            return self._send_json({"ok": True, "version": self.server_version, "control": self.control})
        if route.path == "/messages":
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            since = 0
            q = parse_qs(route.query)
            if q.get("since"):
                try:
                    since = int(q["since"][0])
                except ValueError:
                    since = 0
            return self._send_json({"messages": self.board.since(since)})
        if route.path == "/typing":
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            # per-agent activity detail rides ONLY this authed view; the public GET /control/tts
            # stays frozen at {muted, server} (two bools, no identity)
            return self._send_json({"typing": self.board.typing_now(),
                                    "speaking": self.board.speaking_now()})
        if route.path == "/roster":
            return self._send_json({"roster": read_roster()})
        if route.path == "/threads":
            # the task ledger: owners/assignees are per-agent identity -> authed view only
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            return self._send_json({"threads": threads_list()})
        if route.path == "/control/memory":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            return self._send_json({"memory": memory_read()})
        if route.path == "/control/model":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            return self._send_json({"model": model_read()})
        if route.path == "/control/clitemplate":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            return self._send_json({"cli_template": cli_template_read()})
        if route.path == "/control/voices":
            # Gated exactly like /control/memory (control 404 -> authed 401). Returns the STATIC
            # v1.0 voice-pack id list plus the current per-agent assignments -- the board never
            # probes the speech engine, so it stays dependency-free.
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            return self._send_json({"voices": VOICE_PACK_V1, "assigned": voices_read()})
        if route.path == "/control/tts":
            # DELIBERATE, reasoned widen of a shape earlier locked at {muted,server}: "mode" is a
            # board-wide config enum, not per-agent identity, so it doesn't cross the line that
            # shape-freeze was protecting (see docs history) -- still zero identity on this
            # unauthenticated poll, just one more bool-shaped fact the page needs to render itself.
            return self._send_json({"muted": tts_muted_read(), "server": self.board.speaker_active(),
                                    "mode": voice_mode_read()})
        return self._send_json({"error": "not found"}, 404)

    def do_POST(self):
        if not self._host_ok():
            return self._send_json({"error": "bad host"}, 403)
        if not self._origin_ok():
            return self._send_json({"error": "cross-origin request refused"}, 403)
        route = urlparse(self.path)
        # /shutdown is a control endpoint: it inherits the _host_ok + _origin_ok guards
        # above (so a cross-origin page can't CSRF-to-localhost kill the board), plus the
        # token gate below, and is only present when the operator opted in with --control.
        if route.path == "/shutdown":
            if not self.control:
                return self._send_json({"error": "shutdown control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            self._send_json({"ok": True, "shutting_down": True})
            threading.Thread(target=self._delayed_exit, daemon=True).start()
            return
        if route.path == "/control/restart":
            # Full clean relaunch: flushes runaway agent processes and restarts the whole stack.
            # Same gates as /shutdown; exit code 42 tells the run.py supervisor "come back up".
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            self.board.post("board", "Restarting the whole crew -- back in a few seconds.", tags=["restart"])
            self._send_json({"ok": True, "restarting": True})
            threading.Thread(target=self._delayed_exit, args=(42,), daemon=True).start()
            return
        if route.path == "/control/kick":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            if not re.fullmatch(r"[a-z0-9_-]+", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            with CREW_LOCK:       # serialize the run.pids read-modify-write vs add/respawn
                crew = read_crew()
                in_roster = any(i.get("name") == name for i in read_roster_list())
                if name == "board" or (name not in crew and not in_roster):
                    return self._send_json({"error": "no such agent"}, 404)
                if name in crew:
                    kill_pid(crew.pop(name))
                    write_crew(crew)
                roster_remove(name)   # drop from the persisted lineup so a restart won't bring it back
            return self._send_json({"ok": True, "kicked": name})
        if route.path == "/control/clear":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            self.board.clear()
            return self._send_json({"ok": True, "cleared": True})
        if route.path == "/threads":
            # ledger mutations: same gates as the other state-changing controls
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            op = str(data.get("op", ""))
            result, err, code = thread_op(op, data)
            if err:
                return self._send_json({"error": err}, code)
            return self._send_json({"ok": True, "thread": result}, code)
        if route.path == "/control/memory":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            if not re.fullmatch(r"[a-z0-9_-]+", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            try:
                mem = memory_write(name, bool(data.get("on")))
            except Exception as e:
                return self._send_json({"error": "could not write settings: %s" % e}, 500)
            return self._send_json({"ok": True, "agent": name, "on": bool(data.get("on")), "memory": mem})
        if route.path == "/control/model":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            if not re.fullmatch(r"[a-z0-9_-]+", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            raw = data.get("model", "")
            if raw and not re.fullmatch(r"[\w.\-:/]{1,128}", str(raw)):
                return self._send_json({"error": "model id: letters/digits/._-:/ only, max 128 chars"}, 400)
            try:
                mdl = model_write(name, raw)
            except Exception as e:
                return self._send_json({"error": "could not write settings: %s" % e}, 500)
            return self._send_json({"ok": True, "agent": name, "model": mdl.get(name, ""), "all": mdl})
        if route.path == "/control/clitemplate":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            if not re.fullmatch(r"[a-z0-9_-]+", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            tokens = data.get("tokens", [])
            if not isinstance(tokens, list):
                return self._send_json({"error": "tokens must be a list (empty list clears it)"}, 400)
            tpl, err = cli_template_write(name, tokens)
            if err:
                return self._send_json({"error": err}, 400)
            return self._send_json({"ok": True, "agent": name, "tokens": tpl.get(name, []), "all": tpl})
        if route.path == "/control/voice":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            voice = data.get("voice", "")
            if not re.fullmatch(r"[a-z0-9_-]{1,64}", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            # voice ids are [A-Za-z0-9_]; the sentinel "off" also matches and clears the entry
            if not re.fullmatch(r"[A-Za-z0-9_]{1,64}", voice or ""):
                return self._send_json({"error": "bad voice id"}, 400)
            try:
                vmap = voices_write(name, voice)
            except Exception as e:
                return self._send_json({"error": "could not write voices: %s" % e}, 500)
            return self._send_json({"ok": True, "agent": name, "voice": voice, "voices": vmap})
        if route.path == "/control/tts":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            if data.get("heartbeat"):
                self.board.speaker_ping()          # a server speaker announcing it's alive
                return self._send_json({"ok": True, "server": True})
            if "mode" in data:
                return self._send_json({"ok": True, "mode": voice_mode_write(str(data.get("mode", "")))})
            return self._send_json({"ok": True, "muted": tts_muted_write(bool(data.get("muted")))})
        if route.path == "/control/add":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            folder = data.get("folder", "")
            if not folder or not isinstance(folder, str):
                return self._send_json({"error": "a folder is required"}, 400)
            try:
                p = Path(folder).expanduser().resolve()
            except Exception:
                return self._send_json({"error": "bad folder path"}, 400)
            if not p.is_dir():
                return self._send_json({"error": "that folder does not exist"}, 400)
            # Crew-root fence -- ONLY on a NETWORKED board. A folder-agent may read its folder's
            # files, so on a shared/networked board a remote actor must not aim one at a secrets dir.
            # On a LOCAL (loopback) board it's the user's own machine adding their own projects, so
            # ANY folder is fine -- no fence. Networked default = this repo; widen with
            # FLEETCHAT_CREW_ROOT; FLEETCHAT_CREW_ANY_DIR=1 opts out even when networked.
            loopback = self.cfg.get("bind", "127.0.0.1") in LOOPBACK
            if not loopback and os.environ.get("FLEETCHAT_CREW_ANY_DIR") != "1":
                root_env = os.environ.get("FLEETCHAT_CREW_ROOT")
                root = Path(root_env).expanduser().resolve() if root_env else REPO
                if not (p == root or root in p.parents):
                    return self._send_json({"error": "folder must be inside the crew root (%s); set FLEETCHAT_CREW_ROOT to widen" % root}, 403)
            name = re.sub(r"[^a-z0-9_-]", "", p.name.lower())
            if not name:
                return self._send_json({"error": "cannot derive an agent name from that folder"}, 400)
            with CREW_LOCK:       # serialize the run.pids read-modify-write vs kick/respawn
                crew = read_crew()
                if name == "board" or name in crew:
                    return self._send_json({"error": "an agent named '%s' is already here" % name}, 409)
                # fixed command, folder as a validated list-arg -> no shell, no injection. The agent
                # inherits this board's URL + live flag from the environment (see docs/SECURITY.md).
                env = dict(os.environ)
                env["FLEETCHAT_AGENT_DIR"] = str(p)
                proc = subprocess.Popen([sys.executable, str(REPO / "agents" / "run_agent.py"),
                                         name, "--dir", str(p)], env=env)
                crew[name] = proc.pid
                write_crew(crew)
                roster_add(name, str(p))   # persist to the lineup so a restart re-launches it
            return self._send_json({"ok": True, "added": name})
        if route.path == "/control/respawn":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            if not re.fullmatch(r"[a-z0-9_-]+", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            with CREW_LOCK:       # serialize the run.pids read-modify-write vs add/kick
                crew = read_crew()
                entry = next((i for i in read_roster_list() if i.get("name") == name), None)
                if name == "board" or (name not in crew and entry is None):
                    return self._send_json({"error": "no such agent"}, 404)
                if name in crew:                   # running -> stop the watcher AND its process tree
                    kill_pid(crew.pop(name))
                # Relaunch with the SAME fixed list-arg Popen /control/add uses (no shell). The optional
                # project folder comes from the persisted roster entry; without one it launches bare.
                folder = entry.get("dir") if entry else None
                args = [sys.executable, str(REPO / "agents" / "run_agent.py"), name]
                env = dict(os.environ)
                if folder:
                    args += ["--dir", str(folder)]
                    env["FLEETCHAT_AGENT_DIR"] = str(folder)
                proc = subprocess.Popen(args, env=env)
                crew[name] = proc.pid
                write_crew(crew)
            return self._send_json({"ok": True, "respawned": name, "pid": proc.pid})
        if route.path == "/control/pick":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            # open a native folder dialog on the local desktop (in a fresh process so tkinter
            # gets its own main thread). Returns the picked absolute path; the UI then adds it.
            script = ("import tkinter as tk\nfrom tkinter import filedialog\n"
                      "r=tk.Tk();r.withdraw();r.attributes('-topmost',True)\n"
                      "print(filedialog.askdirectory(title='Pick a project folder for the new agent') or '')")
            try:
                out = subprocess.run([sys.executable, "-c", script], capture_output=True,
                                     text=True, timeout=180).stdout.strip()
            except Exception:
                return self._send_json({"error": "native picker unavailable"}, 500)
            if not out:
                return self._send_json({"error": "cancelled"}, 400)
            return self._send_json({"ok": True, "folder": out})
        if route.path == "/typing":
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            agent = (data.get("agent") or "").strip()
            if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", agent):
                return self._send_json({"error": "bad agent"}, 400)
            if data.get("what") == "speak":     # a TTS speaker marking whose reply it is voicing
                self.board.set_speaking(agent, bool(data.get("on")))
            else:
                self.board.set_typing(agent, bool(data.get("on")))
            return self._send_json({"ok": True, "agent": agent, "on": bool(data.get("on"))})
        if route.path != "/post":
            return self._send_json({"error": "not found"}, 404)
        if not self._authed():
            return self._send_json({"error": "unauthorized"}, 401)
        length = int(self.headers.get("Content-Length", 0) or 0)
        if length <= 0 or length > 64 * 1024:
            return self._send_json({"error": "bad request"}, 400)
        try:
            data = json.loads(self.rfile.read(length).decode("utf-8"))
        except Exception:
            return self._send_json({"error": "invalid json"}, 400)
        sender = (data.get("sender") or "").strip()
        text = data.get("text")
        if not sender or not text:
            return self._send_json({"error": "sender and text are required"}, 400)
        msg = self.board.post(sender, text, data.get("tags"))
        return self._send_json(msg, 201)

    def _serve_ui(self, page=None):
        index = page or (WEB / "index.html")
        if not index.exists():
            return self._send_json({"error": "UI not found"}, 404)
        body = index.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    cfg = enforce_security(load_config())
    Handler.board = Board()
    Handler.cfg = cfg
    Handler.allowed = allowed_hosts(cfg)
    # Control endpoints (add-agent / kick / shutdown) drive the sidebar's "+ Add agent" button.
    # Default them ON for a SEALED LOCAL (loopback) board -- it's your own machine, nothing is on
    # the network, so exposing the add-agent UI is safe and it should always be there. For a
    # NETWORKED bind they stay OFF unless explicitly enabled, so a shared board never hands
    # process-spawn to the network by default. FLEETCHAT_CONTROL=1 forces on; =0 forces off.
    _ctl = os.environ.get("FLEETCHAT_CONTROL")
    Handler.control = (_ctl == "1") or (_ctl != "0" and cfg["bind"] in LOOPBACK)
    httpd = ThreadingHTTPServer((cfg["bind"], cfg["port"]), Handler)
    scope = "SEALED LOCAL (loopback)" if cfg["bind"] in LOOPBACK else "NETWORKED (token required)"
    print(f"[board] FleetChat board up on http://{cfg['bind']}:{cfg['port']}  --  {scope}")
    print(f"[board] open the UI:  http://{'127.0.0.1' if cfg['bind'] in LOOPBACK else cfg['bind']}:{cfg['port']}/")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\n[board] shutting down.")
        httpd.shutdown()


if __name__ == "__main__":
    main()
