#!/usr/bin/env python3
"""
speaker.py -- optional SERVER-SIDE voice for the crew (high-quality kokoro neural TTS).

Watches the board and speaks each AGENT reply aloud, played on the machine running the board.
This is the nicer-sounding alternative to the web page's built-in browser voices: while this
speaker is live it heartbeats the board, so the UI's browser TTS stands DOWN automatically (no
double-up). Only roster agents are voiced -- humans are not.

    python scripts/download_voices.py    # once: fetch the engine + weights (~353 MB)
    python run.py --speak                # board + crew + speaker
    python agents/speaker.py             # or standalone, against an already-running board

Voices: each agent gets a stable, distinct English voice (American/British), assigned automatically.
Override any in data/voices.json as {"agent-id": "voice_id"} (e.g. {"aegis": "am_fenrir"}).
"""
import json
import os
import re
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
VOICES_DIR = REPO / "data" / "voices"
MODEL = VOICES_DIR / "kokoro-v1.0.onnx"
VOICES_BIN = VOICES_DIR / "voices-v1.0.bin"
BOARD = os.environ.get("FLEETCHAT_URL", "http://127.0.0.1:8137").rstrip("/")
TOKEN = os.environ.get("FLEETCHAT_TOKEN")

if not (MODEL.exists() and VOICES_BIN.exists()):
    sys.exit("[speaker] model weights not found in data/voices/. Run:  python scripts/download_voices.py")
try:
    from kokoro_onnx import Kokoro
    import soundfile as sf
except Exception:
    sys.exit("[speaker] kokoro-onnx not installed. Run:  python scripts/download_voices.py")

kokoro = Kokoro(str(MODEL), str(VOICES_BIN))
ALL_VOICES = sorted(kokoro.get_voices())
# English (American/British: af_/am_/bf_/bm_) voices sound right for English text; the other-language
# voices (zh/ja/es/...) don't. Auto-assignment draws from English; an override can still name any voice.
ENGLISH = [v for v in ALL_VOICES if v[:3] in ("af_", "am_", "bf_", "bm_")] or ALL_VOICES


def _headers(extra=None):
    h = dict(extra or {})
    if TOKEN:
        h["X-Fleet-Token"] = TOKEN
    return h


def _get_json(path):
    with urllib.request.urlopen(urllib.request.Request(BOARD + path, headers=_headers()), timeout=6) as r:
        return json.load(r)


def _post_json(path, obj):
    body = json.dumps(obj).encode("utf-8")
    req = urllib.request.Request(BOARD + path, data=body, headers=_headers({"Content-Type": "application/json"}))
    urllib.request.urlopen(req, timeout=5).read()


def _overrides():
    f = REPO / "data" / "voices.json"
    if f.is_file():
        try:
            d = json.loads(f.read_text(encoding="utf-8"))
            return d if isinstance(d, dict) else {}
        except Exception:
            return {}
    return {}


def build_voice_map(names):
    """Assign each agent a stable, distinct English voice. data/voices.json overrides win (and may
    name ANY installed voice); everyone else is anchored at a hash of their name and linear-probes to
    the next free English voice, so a small crew gets distinct voices without any hand-tuning."""
    overrides = _overrides()
    used, vmap = set(), {}
    for n in names:                        # pinned overrides first -- their voice leaves the free set
        ov = overrides.get(n)
        if isinstance(ov, str) and ov in ALL_VOICES:
            vmap[n] = ov
            used.add(ov)
    for n in sorted(names):                # deterministic order -> stable probing
        if n in vmap:
            continue
        h = 0
        for c in n:
            h = (h * 31 + ord(c)) % len(ENGLISH)
        for i in range(len(ENGLISH)):      # probe from the hash slot to the first unused voice
            v = ENGLISH[(h + i) % len(ENGLISH)]
            if v not in used:
                vmap[n] = v
                used.add(v)
                break
        else:
            vmap[n] = ENGLISH[h]           # more agents than voices -> allow reuse
    return vmap


def agent_ids():
    try:
        return set(str(a.get("id")) for a in _get_json("/roster").get("roster", []))
    except Exception:
        return set()


def is_muted():
    try:
        return bool(_get_json("/control/tts").get("muted"))
    except Exception:
        return False


def clean(t):
    t = re.sub(r"```[\s\S]*?```", " . code block . ", t or "")
    t = re.sub(r"`([^`]+)`", r"\1", t)
    t = re.sub(r"\[([^\]]+)\]\([^)]+\)", r"\1", t)
    t = re.sub(r"https?://\S+", " link ", t)
    t = re.sub(r"[*#_>|]", "", t)
    t = re.sub(r"\s+", " ", t)
    return t.strip()[:600]


def play(samples, sr):
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
        p = tmp.name
    try:
        sf.write(p, samples, sr)
        if sys.platform == "win32":
            subprocess.run(["powershell", "-NoProfile", "-Command",
                            "(New-Object Media.SoundPlayer '%s').PlaySync()" % p], capture_output=True, timeout=90)
        elif sys.platform == "darwin":
            subprocess.run(["afplay", p], capture_output=True, timeout=90)
        else:  # linux: first available of paplay / aplay / ffplay
            for player in (["paplay", p], ["aplay", "-q", p],
                           ["ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", p]):
                try:
                    subprocess.run(player, capture_output=True, timeout=90, check=True)
                    break
                except Exception:
                    continue
    finally:
        try:
            os.unlink(p)
        except OSError:
            pass


def main():
    while True:                              # wait for the board; start from its head (don't voice history)
        try:
            last = max((m["id"] for m in _get_json("/messages?since=0").get("messages", [])), default=0)
            break
        except Exception:
            time.sleep(2)
    print("[speaker] up; %d English voices; watching from id %d" % (len(ENGLISH), last), flush=True)
    agents = agent_ids()
    vmap = build_voice_map(agents)

    # Heartbeat from a DAEMON THREAD, never the main loop: the loop blocks for a whole burst's
    # synth+playback, which would starve a loop-based heartbeat past the board's 30s TTL and flip
    # the UI back to browser TTS mid-queue (a stray robo-voice). A thread can't starve.
    def _hb_loop():
        while True:
            try:
                _post_json("/control/tts", {"heartbeat": True})   # browser TTS stands down while we're live
            except Exception:
                pass
            time.sleep(8)
    threading.Thread(target=_hb_loop, daemon=True).start()

    last_roster, misses = time.time(), 0
    while True:
        now = time.time()
        if now - last_roster > 15:
            agents = agent_ids() or agents
            vmap = build_voice_map(agents)
            last_roster = now
        try:
            for m in _get_json("/messages?since=%d" % last).get("messages", []):
                last = max(last, m["id"])
                s = m.get("sender", "")
                if s not in agents:           # only voice roster agents, never humans
                    continue
                txt = clean(m.get("text", ""))
                if not txt or is_muted():
                    continue
                try:      # 🔊 pulse: tell the board whose reply is being voiced (authed view only)
                    _post_json("/typing", {"agent": s, "on": True, "what": "speak"})
                except Exception:
                    pass
                try:
                    samples, sr = kokoro.create(txt, voice=vmap.get(s) or ENGLISH[0], speed=1.0, lang="en-us")
                    play(samples, sr)
                finally:
                    try:
                        _post_json("/typing", {"agent": s, "on": False, "what": "speak"})
                    except Exception:
                        pass
            misses = 0
        except Exception:
            misses += 1
            if misses >= 40:                  # board gone ~1 min -> exit instead of orphaning
                print("[speaker] board unreachable; exiting.", flush=True)
                return
            time.sleep(2)
        time.sleep(1.2)


if __name__ == "__main__":
    main()
