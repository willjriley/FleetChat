"""A STUB of the daemon's HTTP API, for driving index.html in a real browser.

Why this exists: the real daemon hardcodes port 8137 and kills other instances
of itself on startup, so running it to test the UI would put the operator's LIVE
fleet board at risk. This serves the same routes the page needs, on a different
port, with no agents and no board file -- so the client can be exercised for
real without touching anything live.

It is a TEST HARNESS, not a second board: it never spawns an agent, never writes
board.jsonl, and holds everything in memory.

It lives in scripts/ rather than beside the page it drives, because server/web/ is
served wholesale by http.FileServer -- anything dropped in there becomes a fetchable
asset on the live board (this file was briefly reachable at /stubboard.py). A test
harness should not be a served route, so it sits outside the served tree and is
pointed at the page over HTTP like any other client.

    python scripts/stubboard.py 8899
"""
import json
import os
import sys
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

# Two messages in one conversation (thread 1), so the page has something to
# reply to. Mirrors the daemon's wire shape: id/sender/text/to/thread/ts.
MESSAGES = [
    {"id": 1, "sender": "owner", "text": "kick off the render job", "to": ["alice", "carol"],
     "thread": 1, "ts": 1756000000.0},
    {"id": 2, "sender": "alice", "text": "starting now", "to": ["carol", "owner"],
     "thread": 1, "ts": 1756000060.0},
]
MEMBERS = {1: ["owner", "alice", "carol"]}
POSTED = []
OLD_DAEMON = os.environ.get("OLD_DAEMON") == "1"


class Handler(SimpleHTTPRequestHandler):
    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if u.path == "/messages":
            since = int((q.get("since") or ["0"])[0])
            # NB the wire shape is {"messages":[...]}, NOT a bare array -- the
            # client reads data.messages, so a bare list renders an empty board.
            return self._json({"messages": [m for m in MESSAGES if m["id"] > since]})
        if u.path == "/capabilities":
            # OLD_DAEMON=1 impersonates a binary that predates threading: it 404s
            # here exactly as the real one does, so the client's fallback can be
            # tested rather than assumed.
            if OLD_DAEMON:
                self.send_error(404)
                return
            return self._json({"threads": True})
        if u.path == "/conversation/members":
            if OLD_DAEMON:
                self.send_error(404)
                return
            tid = int((q.get("id") or ["0"])[0])
            return self._json({"thread": tid, "members": MEMBERS.get(tid, [])})
        if u.path == "/roster":
            return self._json([{"id": n, "name": n.capitalize(), "role": ""} for n in ("alice", "carol", "dave")])
        if u.path == "/typing":
            return self._json([])
        if u.path == "/threads":
            return self._json({"threads": []})
        if u.path == "/health":
            return self._json({"ok": True})
        if u.path.startswith("/control/"):
            return self._json({"ok": True, "muted": False, "voices": [], "debug": False})
        return SimpleHTTPRequestHandler.do_GET(self)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        u = urlparse(self.path)
        if u.path == "/post":
            try:
                POSTED.append(json.loads(raw))
            except Exception:
                POSTED.append({"_unparseable": raw.decode("utf-8", "replace")})
            # Echo back the shape the client expects from a successful post.
            # Mirror the daemon's misroute warning so the client path can be tested.
            body = POSTED[-1] if POSTED else {}
            warn = ""
            for t in (body.get("to") or []):
                if t.lower() not in ("alice", "carol", "dave", "all"):
                    warn = "these @names are not on this board and were not woken: " + t
            return self._json({"id": 99, "sender": "owner", "text": "", "ts": 1756000120.0,
                               "woke": [], "warning": warn})
        if u.path == "/_posted":          # test-only: what did the page send?
            return self._json(POSTED)
        return self._json({"ok": True})

    def log_message(self, *a):
        pass                              # keep the test output readable


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
