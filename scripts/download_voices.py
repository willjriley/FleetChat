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
import hashlib
import subprocess
import sys
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
DEST = REPO / "data" / "voices"

# Pinned for supply-chain reproducibility: an exact engine version + the SHA256 of each weight,
# verified against the official kokoro-onnx v1.0 release (Apache-2.0; byte sizes 325532387 / 28214398).
# A hash mismatch ABORTS rather than handing an unverified ~353MB binary to the model loader -- so a
# tampered mirror, MITM, or truncated download can never be loaded.
ENGINE_PIN = "kokoro-onnx==0.5.0"
_BASE = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0"
FILES = {
    "kokoro-v1.0.onnx": (_BASE + "/kokoro-v1.0.onnx",
                         "7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5"),
    "voices-v1.0.bin":  (_BASE + "/voices-v1.0.bin",
                         "bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d"),
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
        subprocess.run([sys.executable, "-m", "pip", "install", "--quiet", ENGINE_PIN, "soundfile"], check=True)
        print("[voices]   engine installed (%s)." % ENGINE_PIN)
        return True
    except Exception as e:
        print("[voices]   pip install failed (%s)." % e)
        print("[voices]   install it yourself:  %s -m pip install %s soundfile" % (sys.executable, ENGINE_PIN))
        return False


def sha256_of(path):
    """Streaming SHA256 so a 310MB weight is never read wholly into memory."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def download(name, url, expected_sha, dest):
    if dest.exists() and dest.stat().st_size > 1_000_000:
        if sha256_of(dest) == expected_sha:
            print("[voices]   %s already present + SHA256-verified -- skipping." % name)
            return
        print("[voices]   %s present but SHA256 mismatch -- re-downloading." % name)
        dest.unlink()
    tmp = dest.with_name(dest.name + ".part")
    # url is one of FILES' hardcoded literal values (this module, top) -- not a parameter
    # ever set from a CLI arg, env var, or any runtime input. The downloaded content is also
    # SHA256-verified against a hardcoded expected hash before it's ever kept (below).
    with urllib.request.urlopen(url, timeout=60) as r:  # nosemgrep: dynamic-urllib-use-detected
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
    actual = sha256_of(tmp)
    if actual != expected_sha:
        tmp.unlink()   # never keep an unverified weight
        raise ValueError("SHA256 mismatch for %s -- got %s, expected %s. File refused." %
                         (name, actual, expected_sha))
    tmp.replace(dest)
    print("\r[voices]   %s done + SHA256-verified (%d MB).            " % (name, dest.stat().st_size // (1024 * 1024)))


def main():
    DEST.mkdir(parents=True, exist_ok=True)
    engine_ok = ensure_engine()
    print("[voices] fetching model weights into %s ..." % DEST)
    for name, (url, sha) in FILES.items():
        try:
            download(name, url, sha, DEST / name)
        except Exception as e:
            print("\n[voices]   FAILED for %s: %s" % (name, e))
            print("[voices]   re-run to retry; partial files (*.part) are safe to delete.")
            return 1
    print("[voices] done -- high-quality voices are installed.")
    if not engine_ok:
        print("[voices] NOTE: the engine didn't install cleanly; run the pip line above, then:")
    print("[voices] start them with:  python run.py --speak")
    return 0


if __name__ == "__main__":
    sys.exit(main())
