#!/usr/bin/env python3
"""
FleetChat board server -- the async message board at the heart of a FleetChat crew.

A tiny, zero-dependency HTTP server over an append-only JSONL log. Agents (and the
web UI) POST messages and poll for everything since a given id. That is the whole
coordination substrate: no database, no broker, no cloud -- just this file + the
Python standard library.

SECURITY BY CONSTRUCTION (see ../docs/SECURITY.md):
  - Binds to 127.0.0.1 by default. A default FleetChat is a SEALED LOCAL fleet:
    nothing sits on the network, so there is nothing to attack.
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


def write_crew(crew):
    DATA.mkdir(parents=True, exist_ok=True)
    (DATA / "run.pids").write_text("\n".join("%s %d" % (n, p) for n, p in crew.items()), encoding="utf-8")


def kill_pid(pid):
    try:
        if sys.platform == "win32":
            subprocess.run(["taskkill", "/F", "/PID", str(pid), "/FI", "IMAGENAME eq python.exe"],
                           capture_output=True)
        else:
            os.kill(pid, 15)
    except Exception:
        pass


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
        """Defeats CSRF-to-localhost: a browser state-change must be same-origin.
        Non-browser clients (the join skill, curl) send no Origin/Referer and are
        allowed -- they carry no ambient cookies a malicious page could ride."""
        src = self.headers.get("Origin") or self.headers.get("Referer")
        if not src:
            return True
        return (urlparse(src).hostname or "") in self.allowed

    def _delayed_exit(self):
        """/shutdown: let the response flush, then exit. run.py sees the board go down and
        its cleanup() stops the agent processes -- one click = a clean full-crew shutdown."""
        time.sleep(0.4)
        os._exit(0)

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
        if route.path == "/control/kick":
            if not self.control:
                return self._send_json({"error": "control not enabled"}, 404)
            if not self._authed():
                return self._send_json({"error": "unauthorized"}, 401)
            data = self._read_json() or {}
            name = data.get("agent", "")
            if not re.fullmatch(r"[a-z0-9_-]+", name or ""):
                return self._send_json({"error": "bad agent name"}, 400)
            crew = read_crew()
            if name == "board" or name not in crew:
                return self._send_json({"error": "no such agent"}, 404)
            kill_pid(crew.pop(name))
            write_crew(crew)
            return self._send_json({"ok": True, "kicked": name})
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
            # Crew-root fence: a folder-agent is told it may read its folder's files, so an
            # added agent must not be aimed at a secrets/creds dir. Restrict to a crew root
            # (default = this repo; widen with FLEETCHAT_CREW_ROOT; FLEETCHAT_CREW_ANY_DIR=1 opts out).
            if os.environ.get("FLEETCHAT_CREW_ANY_DIR") != "1":
                root_env = os.environ.get("FLEETCHAT_CREW_ROOT")
                root = Path(root_env).expanduser().resolve() if root_env else REPO
                if not (p == root or root in p.parents):
                    return self._send_json({"error": "folder must be inside the crew root (%s)" % root}, 403)
            name = re.sub(r"[^a-z0-9_-]", "", p.name.lower())
            if not name:
                return self._send_json({"error": "cannot derive an agent name from that folder"}, 400)
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
            return self._send_json({"ok": True, "added": name})
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

    def _serve_ui(self):
        index = WEB / "index.html"
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
    Handler.control = os.environ.get("FLEETCHAT_CONTROL") == "1"
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
