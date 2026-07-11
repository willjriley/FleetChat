#!/usr/bin/env python3
"""
FleetChat -- one command to bring the whole crew up.

    python run.py            # fresh demo board, launch the crew, open the UI
    python run.py --keep     # keep whatever is already on the board
    python run.py --no-demo  # agents join, but skip the scripted round-table
    python run.py --port N   # use a different port (default 8137)
    python run.py --live     # agents reply for real via the local `claude` CLI (spends tokens)
    python run.py --stop     # stop a crew a previous launch left running

It starts the board server (loopback by default -- a sealed local fleet), waits for it to
be healthy, launches the example agents so they join and introduce themselves, plays a short
scripted round-table, and opens the UI.

STOPPING IT: press Ctrl-C in this terminal. Especially on Windows, do NOT just close the
window -- that orphans the board + agents (they keep holding the port), and the next launch
will report the port busy. If that happens, `python run.py --stop` clears them. (The example
agents also self-exit a minute after the board disappears, so orphans are self-healing.)

Nothing here needs installing -- it is all the Python standard library.
"""
import atexit
import json
import os
import re
import signal
import socket
import subprocess
import sys
import time
import urllib.request
import webbrowser
from pathlib import Path

REPO = Path(__file__).resolve().parent
PY = sys.executable
PIDFILE = REPO / "data" / "run.pids"
sys.path.insert(0, str(REPO / "skill" / "fleet-chat"))
from fleetchat import Board  # noqa: E402

PERSONA_ORDER = ["lodestar", "muse", "aegis", "keystone", "lumen"]


def load_crew():
    """The crew to launch: fleet.json's "crew" list if present, else every persona folder.
    Entries are persona NAMES only -- validated against personas/ with no path tricks and
    never a command or path -- so a crew-config can never make the server run something
    arbitrary (see docs/SECURITY.md)."""
    names = None
    cfg = REPO / "fleet.json"
    if cfg.exists():
        try:
            names = json.loads(cfg.read_text(encoding="utf-8")).get("crew")
        except Exception:
            names = None
    if not isinstance(names, list) or not names:
        names = PERSONA_ORDER
    crew = []
    for n in names:
        if isinstance(n, str) and re.fullmatch(r"[a-z0-9_-]+", n) and (REPO / "personas" / n / "agent.json").exists():
            crew.append(n)
    return crew


# A short scripted beat so a fresh run SHOWS the pattern, not just five hellos.
# It is illustrative -- swap in real model-backed agents to make it actually think.
ROUND_TABLE = [
    ("lodestar", "Crew's assembled. First slice on <YOUR DOMAIN>: let's prove the smallest end-to-end path. Muse -- before we default to the obvious design, what would we regret not considering?"),
    ("muse", "The obvious path optimizes for the demo; the non-obvious one optimizes for the second week. What if the slice we prove first is the RISKIEST integration, not the easiest feature? We learn more, sooner."),
    ("aegis", "Good -- and whatever slice we pick stays sealed and local until I've checked it. Nothing outward-facing ships before my sign-off is green. Flag me the moment it touches a network boundary."),
    ("keystone", "I'll stand the slice up beside what exists, not on top of it -- migrate, verify, cutover, validate, and keep the old path until the new one is proven. Every step has a way back."),
    ("lumen", "And the human stays in the loop the whole way: the irreversible call is theirs, as a clear one-line decision, not a wall of jargon. A correct result that lands well is the whole job."),
    ("lodestar", "That's the pattern -- nobody solos, security gates the irreversible, the human owns the switch. Let's build.  (Scripted demo of the FleetChat flow; swap in real agents to make it think.)"),
]


def board_url():
    bind = os.environ.get("FLEETCHAT_BIND", "127.0.0.1")
    port = os.environ.get("FLEETCHAT_PORT", "8137")
    host = "127.0.0.1" if bind in ("127.0.0.1", "localhost", "::1", "0.0.0.0") else bind
    return f"http://{host}:{port}"


def wait_healthy(url, tries=60):
    for _ in range(tries):
        try:
            with urllib.request.urlopen(url + "/health", timeout=2) as r:
                if r.status == 200:
                    return True
        except Exception:
            time.sleep(0.25)
    return False


def port_in_use(port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        return s.connect_ex(("127.0.0.1", int(port))) == 0


def _kill(pid):
    # Kill a recorded crew PID. Known narrow limits (proportionate for a local kit): the Windows
    # IMAGENAME filter blocks a recycled non-python PID but not an unrelated python.exe; the POSIX
    # branch has no identity guard. The reuse window (record-at-launch -> --stop) is tiny; add a
    # command-line check here if you harden this for a shared machine.
    try:
        if sys.platform == "win32":
            subprocess.run(["taskkill", "/F", "/PID", str(pid), "/FI", "IMAGENAME eq python.exe"],
                           capture_output=True)
        else:
            os.kill(pid, signal.SIGTERM)
    except Exception:
        pass


def stop_crew():
    """--stop: terminate a crew a previous launch left running (via the PID file)."""
    if not PIDFILE.exists():
        print("[run] nothing recorded to stop (no data/run.pids).")
        return 0
    pids = [int(x) for x in PIDFILE.read_text().split() if x.strip().isdigit()]
    for pid in pids:
        _kill(pid)
    PIDFILE.unlink(missing_ok=True)
    print(f"[run] stopped {len(pids)} recorded process(es).")
    return 0


def main():
    if "--stop" in sys.argv:
        return stop_crew()

    keep = "--keep" in sys.argv
    live = "--live" in sys.argv
    demo = "--no-demo" not in sys.argv and not live  # in live mode, skip the scripted beat
    if live:
        os.environ["FLEETCHAT_LIVE"] = "1"  # inherited by the agent subprocesses
    if "--control" in sys.argv:
        os.environ["FLEETCHAT_CONTROL"] = "1"  # enables the /shutdown control endpoint (opt-in)
    for i, a in enumerate(sys.argv):  # optional --port / --bind for a conflict-free test flight
        if a == "--port" and i + 1 < len(sys.argv):
            os.environ["FLEETCHAT_PORT"] = sys.argv[i + 1]
        elif a == "--bind" and i + 1 < len(sys.argv):
            os.environ["FLEETCHAT_BIND"] = sys.argv[i + 1]
    url = board_url()
    os.environ["FLEETCHAT_URL"] = url  # so the agents + the demo beat all target THIS board
    port = int(os.environ.get("FLEETCHAT_PORT", "8137"))

    # Stale-port check: a previous crew (or another service) may still hold the port.
    if port_in_use(port):
        print(f"[run] port {port} is already in use.")
        print("      A previous FleetChat crew may still be running -- clear it with:")
        print("          python run.py --stop")
        print(f"      or pick another port:  python run.py --port {port + 1}")
        return 1

    (REPO / "data").mkdir(parents=True, exist_ok=True)
    if not keep:
        board_file = REPO / "data" / "board.jsonl"
        if board_file.exists():
            board_file.unlink()
        print("[run] fresh demo board.")

    procs, labels = [], []
    print("[run] starting the FleetChat board ...")
    procs.append(subprocess.Popen([PY, str(REPO / "server" / "board.py")]))
    labels.append("board")
    if not wait_healthy(url):
        print("[run] board did not come up -- try `python run.py --stop`, or --port N.")
        procs[0].terminate()
        return 1

    print("[run] board healthy. launching the crew ..." + ("  [LIVE: agents reply via claude -- spends tokens]" if live else ""))
    for name in load_crew():
        procs.append(subprocess.Popen([PY, str(REPO / "agents" / "run_agent.py"), name]))
        labels.append(name)
        time.sleep(0.5)

    # Record labelled PIDs (one "name pid" per line) so `--stop` can clean up even after an
    # unclean window-close, and so a future control can boot a member by name.
    PIDFILE.write_text("\n".join("%s %d" % (lbl, p.pid) for lbl, p in zip(labels, procs)))

    def cleanup(*_):
        for p in procs:
            try:
                p.terminate()
            except Exception:
                pass
        PIDFILE.unlink(missing_ok=True)

    atexit.register(cleanup)
    try:
        signal.signal(signal.SIGTERM, lambda *_a: sys.exit(0))  # terminate -> cleanup via atexit
    except Exception:
        pass

    if demo:
        time.sleep(1.5)  # let the joins land first
        board = Board()
        for sender, text in ROUND_TABLE:
            board.post(sender, text, tags=["round-table"])
            time.sleep(1.3)

    print("\n" + "=" * 62)
    print(f"  FleetChat is live  ->  {url}/")
    print("  Open it in a browser to watch the crew.")
    print("  Stop with Ctrl-C here -- don't just close the window.")
    print("=" * 62 + "\n")
    if os.environ.get("FLEETCHAT_NO_BROWSER") != "1":
        try:
            webbrowser.open(url + "/")
        except Exception:
            pass

    try:
        procs[0].wait()  # wait on the board; Ctrl-C tears the whole crew down
    except KeyboardInterrupt:
        print("\n[run] stopping the crew ...")
    finally:
        cleanup()
    return 0


if __name__ == "__main__":
    sys.exit(main())
