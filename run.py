#!/usr/bin/env python3
"""
FleetChat -- one command to bring the whole crew up.

    python run.py             # board + re-launch your saved data/roster.json lineup (empty on first run)
    python run.py --keep      # keep whatever is already on the board (don't wipe history)
    python run.py --demo      # opt-in showcase: the example crew + a short scripted round-table
    python run.py --live      # agents reply for real via the local `claude` CLI (spends tokens)
    python run.py --control   # force the add-agent / shutdown controls ON (already on for a loopback board)
    python run.py --port N    # use a different port (default 8137)
    python run.py --bind ADDR # bind address (default 127.0.0.1; a non-loopback bind requires a token)
    python run.py --stop      # stop a crew a previous launch left running

By DEFAULT it starts the board server (loopback -- a sealed local fleet), waits for it to be
healthy, opens the UI, and re-launches the PERSISTED lineup from data/roster.json. On a fresh
clone that lineup is empty: click '+ Add agent', point it at a project folder, and an agent
joins live -- the '+' button appends it to data/roster.json (git-ignored) and the 'x' button
removes it, so the crew you build is restored on the next launch. Pass --demo instead to bring
up the example personas and play a short scripted round-table that SHOWS the pattern.

STOPPING IT: press Ctrl-C in this terminal. Especially on Windows, do NOT just close the
window -- that orphans the board + agents (they keep holding the port), and the next launch
will report the port busy. If that happens, `python run.py --stop` clears them. (The agents
also self-exit a minute after the board disappears, so orphans are self-healing.)

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

ROSTER_FILE = REPO / "data" / "roster.json"


def read_roster_list():
    """The PERSISTED lineup (data/roster.json) a restart re-launches: [{"name":..., "dir"?:...}].
    Written by the board's + Add agent / x controls; git-ignored so it never enters the repo."""
    if ROSTER_FILE.is_file():
        try:
            d = json.loads(ROSTER_FILE.read_text(encoding="utf-8"))
            return [x for x in d if isinstance(x, dict) and x.get("name")] if isinstance(d, list) else []
        except Exception:
            return []
    return []


def fleet_file():
    """Resolve the ACTIVE fleet definition, most-specific first:
      1. $FLEETCHAT_FLEET_FILE  -- an EXTERNAL path (your real fleet, kept OUTSIDE the repo so it
                                   can never be committed and survives `git clean`). Preferred.
      2. fleet.local.json       -- a git-ignored in-repo override (quick local iteration).
      3. fleet.json             -- the committed demo default a fresh clone runs.
    Returns a Path, or None if nothing is found."""
    env = os.environ.get("FLEETCHAT_FLEET_FILE")
    if env:
        p = Path(env).expanduser()
        if p.is_file():
            return p
    for name in ("fleet.local.json", "fleet.json"):
        p = REPO / name
        if p.exists():
            return p
    return None


def persona_base_dirs():
    """Where persona folders are looked up, most-specific first: an external dir
    ($FLEETCHAT_PERSONAS_DIR), then personas.local/ (git-ignored), then the committed personas/.
    So a personal fleet can live wholly outside the repo, or mix its own personas with the demo ones."""
    dirs = []
    env = os.environ.get("FLEETCHAT_PERSONAS_DIR")
    if env:
        dirs.append(Path(env).expanduser())
    dirs.append(REPO / "personas.local")
    dirs.append(REPO / "personas")
    return dirs


def resolve_persona(name):
    """The folder for persona `name` -- first base dir with <name>/agent.json, else None.
    Name is charset-validated here, so an override file can never smuggle a path or command."""
    if not (isinstance(name, str) and re.fullmatch(r"[a-z0-9_-]+", name)):
        return None
    for base in persona_base_dirs():
        d = base / name
        if (d / "agent.json").is_file():
            return d
    return None


def _fleet_field(key, default=None):
    f = fleet_file()
    if f:
        try:
            return json.loads(f.read_text(encoding="utf-8")).get(key, default)
        except Exception:
            return default
    return default


def load_crew():
    """The crew to launch: the ACTIVE fleet file's "crew" list if present, else every persona
    folder found. Entries are persona NAMES only -- validated (name charset + the persona folder
    must exist), no path tricks, never a command or path -- so a fleet file can never make the
    server run something arbitrary (docs/SECURITY.md). The SAME validation runs whether the names
    come from the committed default or your own external/local fleet file."""
    names = _fleet_field("crew")
    if not isinstance(names, list) or not names:
        seen, names = set(), []                     # no crew list -> every persona folder we can see
        for base in persona_base_dirs():
            if base.is_dir():
                for p in sorted(base.iterdir()):
                    if (p / "agent.json").is_file() and p.name not in seen:
                        seen.add(p.name); names.append(p.name)
        if not names:
            names = PERSONA_ORDER
    return [n for n in names if resolve_persona(n)]


def crew_lead():
    """Who fields un-@-addressed human messages: the fleet file's "lead", else the first crew
    member, else the historical default. Always a validated persona NAME, never a path/command."""
    lead = _fleet_field("lead")
    if isinstance(lead, str) and resolve_persona(lead):
        return lead
    crew = load_crew()
    return crew[0] if crew else "lodestar"


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


def crew_domain():
    """The domain the round-table names -- from the ACTIVE fleet file's "domain", with a friendly
    fallback so a fresh run never prints a raw <...> placeholder token."""
    dom = str(_fleet_field("domain", "") or "").strip()
    return dom if dom and not dom.startswith("<") else "your product"


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
    demo = "--demo" in sys.argv  # opt-in: --demo brings up the example crew + scripted round-table (the showcase)
    live = ("--live" in sys.argv) or not demo  # default (non-demo) board -> agents you add reply for real; --demo is scripted unless --live
    if live:
        os.environ["FLEETCHAT_LIVE"] = "1"  # inherited by added agents (and the example crew under --demo --live)
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
        print("[run] fresh board.")

    procs, labels = [], []
    print("[run] starting the FleetChat board ...")
    procs.append(subprocess.Popen([PY, str(REPO / "server" / "board.py")]))
    labels.append("board")
    if not wait_healthy(url):
        print("[run] board did not come up -- try `python run.py --stop`, or --port N.")
        procs[0].terminate()
        return 1

    print("[run] board healthy.")

    # Open the UI BEFORE the crew posts, so a first-time viewer watches the agents arrive
    # and the round-table unfold live -- and (after one click, which the browser's autoplay
    # rule requires) hears them -- instead of opening to an already-finished transcript.
    roster = read_roster_list() if not demo else []  # DEFAULT: the persisted lineup a restart restores
    print("\n" + "=" * 62)
    print(f"  FleetChat is live  ->  {url}/")
    if demo:
        print("  Example crew assembling -- add --live to make them think + speak for real.")
    elif roster:
        print("  Restoring your saved lineup -- click '+ Add agent' to add more; they persist.")
    else:
        print("  Empty board. Click '+ Add agent', point it at a project folder, and it joins live.")
    print("  Stop with Ctrl-C here -- don't just close the window.")
    print("=" * 62 + "\n")
    if os.environ.get("FLEETCHAT_NO_BROWSER") != "1":
        try:
            webbrowser.open(url + "/")
        except Exception:
            pass

    if demo:  # --demo only: bring up the example crew so a first run can SHOW the pattern
        os.environ["FLEETCHAT_LEAD"] = crew_lead()  # who fields un-@-addressed human messages
        print("[run] launching the example crew ..." + ("  [LIVE: agents reply via claude -- spends tokens]" if live else ""))
        for name in load_crew():
            procs.append(subprocess.Popen([PY, str(REPO / "agents" / "run_agent.py"), name]))
            labels.append(name)
            time.sleep(0.5)
    else:  # DEFAULT: re-launch the PERSISTED lineup (data/roster.json) -- what + Add agent / x edit and a restart restores
        if roster:
            # No forced lead -- all agents equal; a leader is opt-in (set "lead" in your fleet file).
            print("[run] restoring the saved lineup (%d agent%s) from data/roster.json ..." % (len(roster), "" if len(roster) == 1 else "s"))
            for entry in roster:
                nm = entry.get("name")
                if not (isinstance(nm, str) and re.fullmatch(r"[a-z0-9_-]+", nm)):
                    continue
                if entry.get("seat"):   # a live-session seat (e.g. an interactive conductor) -- shown in the
                    continue            # roster, but NOT spawned as a responder; a running session fills it
                args = [PY, str(REPO / "agents" / "run_agent.py"), nm]
                d = entry.get("dir")
                if isinstance(d, str) and d:
                    args += ["--dir", d]
                procs.append(subprocess.Popen(args))
                labels.append(nm)
                time.sleep(0.4)
        else:
            print("[run] empty board -- click '+ Add agent' and point it at a project folder to add your first agent.")

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
        domain = crew_domain()
        for sender, text in ROUND_TABLE:
            board.post(sender, text.replace("<YOUR DOMAIN>", domain), tags=["round-table"])
            time.sleep(1.3)

    print("[run] the crew is live -- watching the board. Ctrl-C to stop.")

    try:
        procs[0].wait()  # wait on the board; Ctrl-C tears the whole crew down
    except KeyboardInterrupt:
        print("\n[run] stopping the crew ...")
    finally:
        cleanup()
    return 0


if __name__ == "__main__":
    sys.exit(main())
