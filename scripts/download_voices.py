#!/usr/bin/env python3
"""
download_voices.py -- one-command setup for high-quality, server-side agent voices.

By default FleetChat voices agents with the browser's built-in speech synthesis (zero
dependency, works everywhere). This fetches the open kokoro-82M neural TTS so an optional
SERVER-SIDE speaker can voice replies in far nicer voices, played on the machine running the
board. Run it once:

    python scripts/download_voices.py

It (1) pip-installs the kokoro-onnx engine (+ soundfile), and (2) downloads the model weights
(~353 MB, Apache-2.0) into data/voices/ (git-ignored -- too big for the repo). Then start the
speaker alongside the board:

    python run.py --speak       # board + crew + the voice speaker
    # (or run it standalone:  python agents/speaker.py)

Idempotent: re-running skips the engine if importable and any weight already downloaded.
"""
import subprocess
import sys
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
DEST = REPO / "data" / "voices"

# kokoro-onnx v1.0 model weights (Apache-2.0). Same files the kokoro-onnx project ships.
FILES = {
    "kokoro-v1.0.onnx": "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx",
    "voices-v1.0.bin":  "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin",
}


def ensure_engine():
    """pip-install kokoro-onnx + soundfile if kokoro isn't importable yet. Returns True on success."""
    try:
        import kokoro_onnx  # noqa: F401
        import soundfile    # noqa: F401
        print("[voices] engine already installed.")
        return True
    except Exception:
        pass
    print("[voices] installing the kokoro-onnx engine (+ soundfile) -- this pulls onnxruntime, may take a minute ...")
    try:
        subprocess.run([sys.executable, "-m", "pip", "install", "--quiet", "kokoro-onnx", "soundfile"], check=True)
        print("[voices]   engine installed.")
        return True
    except Exception as e:
        print("[voices]   pip install failed (%s)." % e)
        print("[voices]   install it yourself:  %s -m pip install kokoro-onnx soundfile" % sys.executable)
        return False


def download(name, url, dest):
    if dest.exists() and dest.stat().st_size > 1_000_000:
        print("[voices]   %s already present (%d MB) -- skipping." % (name, dest.stat().st_size // (1024 * 1024)))
        return
    tmp = dest.with_name(dest.name + ".part")
    with urllib.request.urlopen(url, timeout=60) as r:
        total = int(r.headers.get("Content-Length", 0))
        got = 0
        with open(tmp, "wb") as f:
            while True:
                chunk = r.read(1024 * 256)
                if not chunk:
                    break
                f.write(chunk)
                got += len(chunk)
                if total:
                    pct = got * 100 // total
                    sys.stdout.write("\r[voices]   %s  %3d%%  (%d / %d MB)" %
                                     (name, pct, got // (1024 * 1024), total // (1024 * 1024)))
                    sys.stdout.flush()
    tmp.replace(dest)
    print("\r[voices]   %s done (%d MB).                    " % (name, dest.stat().st_size // (1024 * 1024)))


def main():
    DEST.mkdir(parents=True, exist_ok=True)
    engine_ok = ensure_engine()
    print("[voices] fetching model weights into %s ..." % DEST)
    for name, url in FILES.items():
        try:
            download(name, url, DEST / name)
        except Exception as e:
            print("\n[voices]   FAILED to download %s: %s" % (name, e))
            print("[voices]   check your connection and re-run; partial files (*.part) are safe to delete.")
            return 1
    print("[voices] done -- high-quality voices are installed.")
    if not engine_ok:
        print("[voices] NOTE: the engine didn't install cleanly; run the pip line above, then:")
    print("[voices] start them with:  python run.py --speak")
    return 0


if __name__ == "__main__":
    sys.exit(main())
