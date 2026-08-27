"""Guards for the mute control in speaker.py.

Written because a mute that fails open is indistinguishable from a mute that works,
right up until it talks over you. Both scenarios below returned False in the version
that shipped -- meaning the speaker spoke straight through a mute whenever the board
hiccuped -- and neither had a test.

Runs with plain stdlib unittest and no board, no network, no kokoro:

    python -m unittest agents.test_speaker -v
"""
import pathlib
import types
import unittest


def _load():
    """Load just the mute logic out of speaker.py.

    Imported directly, speaker.py pulls in kokoro/soundfile and starts talking to the
    board, so the test would need the whole rig up. Exec'ing the slice keeps this a
    unit test.
    """
    src = pathlib.Path(__file__).with_name("speaker.py").read_text(encoding="utf-8")
    start = src.index("_last_mute = None")
    end = src.index("def clean(")
    mod = types.ModuleType("speaker_mute")
    exec("import time\n_get_json = None\n" + src[start:end], mod.__dict__)
    return mod


class MuteFailsSafe(unittest.TestCase):
    def setUp(self):
        self.m = _load()
        self.m._last_mute = None
        self.m._last_mute_at = 0.0

    def _board(self, value):
        """value: True/False to answer, or 'down' to raise."""
        def _g(_path):
            if value == "down":
                raise RuntimeError("board unreachable")
            return {"muted": value}
        self.m._get_json = _g

    def test_reads_muted(self):
        self._board(True)
        self.assertTrue(self.m.is_muted())

    def test_reads_unmuted(self):
        self._board(False)
        self.assertFalse(self.m.is_muted())

    def test_never_read_and_board_down_assumes_muted(self):
        # The old code returned False here: with no idea of the operator's intent it
        # chose to speak, which is the irreversible option.
        self._board("down")
        self.assertTrue(self.m.is_muted(), "must assume muted when it has never read")

    def test_muted_then_board_down_stays_muted(self):
        # The old code returned False here too -- it spoke through a live mute the
        # instant the board blipped.
        self._board(True)
        self.m.is_muted()
        self._board("down")
        self.assertTrue(self.m.is_muted(), "a known mute must survive an outage")

    def test_unmuted_then_board_down_stays_unmuted(self):
        # The other direction matters as much: a hard fail-closed would silence the
        # speaker permanently on a transient blip.
        self._board(False)
        self.m.is_muted()
        self._board("down")
        self.assertFalse(self.m.is_muted(), "must not invent a mute that was never set")


class CachedCheckThrottles(unittest.TestCase):
    """play() polls should_stop() every 0.2s; is_muted() is an HTTP GET.

    Uncached that is ~300 requests at the daemon per 60s clip. The cache must cut the
    round-trips while still letting a mute take effect within about a second.
    """

    def setUp(self):
        self.m = _load()
        self.m._last_mute = None
        self.m._last_mute_at = 0.0
        self.calls = 0

    def _counting_board(self, value):
        def _g(_path):
            self.calls += 1
            return {"muted": value}
        self.m._get_json = _g

    def test_repeated_polls_hit_the_board_once(self):
        self._counting_board(False)
        for _ in range(20):
            self.m.is_muted_cached(ttl=5.0)
        self.assertEqual(self.calls, 1, "20 polls inside the TTL must be one request")

    def test_expired_ttl_refetches(self):
        self._counting_board(False)
        self.m.is_muted_cached(ttl=5.0)
        self.m._last_mute_at -= 10          # pretend the TTL elapsed
        self.m.is_muted_cached(ttl=5.0)
        self.assertEqual(self.calls, 2, "must re-read once the TTL has expired")

    def test_uncached_call_always_reads(self):
        self._counting_board(True)
        self.m.is_muted()
        self.m.is_muted()
        self.assertEqual(self.calls, 2, "is_muted() itself must never serve stale data")


if __name__ == "__main__":
    unittest.main(verbosity=2)
