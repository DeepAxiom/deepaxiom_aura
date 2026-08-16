"""Tests for the streaming buffer, without loading a speech model.

The parts that can regress silently are here: chunk ordering, gap detection
and the silence test. A wrong answer from any of them produces a transcript
that is merely *worse*, never an error — which is exactly the kind of bug
nobody notices until a user complains that it "sometimes mishears".

`Utterance` and `rms` live in engine.py (see its docstring for why that
module's `faster_whisper` import must stay lazy); this file imports it
directly, never `main`, so it keeps working even if `main`'s own import of
`aura` is unavailable in a given environment.

    cd skills/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import struct
import unittest

import engine


def pcm(samples):
    return struct.pack(f"<{len(samples)}h", *samples)


class UtteranceBuffering(unittest.TestCase):
    def test_chunks_are_assembled_in_seq_order_not_arrival_order(self):
        """Audio delivered out of order must not be stitched out of order.

        A realtime channel can reorder or drop; joining by arrival would splice
        two unrelated moments of speech together and the transcript would look
        plausible while being wrong.
        """
        u = engine.Utterance(id="u")
        u.add(pcm([3, 3]), seq=2)
        u.add(pcm([1, 1]), seq=0)
        u.add(pcm([2, 2]), seq=1)

        self.assertEqual(u.pcm(), pcm([1, 1, 2, 2, 3, 3]))

    def test_a_missing_chunk_is_counted_not_hidden(self):
        u = engine.Utterance(id="u")
        u.add(pcm([1]), seq=0)
        u.add(pcm([3]), seq=2)  # seq 1 was dropped in transit

        self.assertEqual(u.gaps(), 1)

    def test_no_gaps_when_the_stream_is_complete(self):
        u = engine.Utterance(id="u")
        for i in range(5):
            u.add(pcm([i]), seq=i)
        self.assertEqual(u.gaps(), 0)

    def test_chunks_without_a_seq_keep_their_arrival_order(self):
        """`seq` is optional in std/audio-chunk@1; a producer that omits it
        still has to work."""
        u = engine.Utterance(id="u")
        u.add(pcm([1]), seq=None)
        u.add(pcm([2]), seq=None)
        u.add(pcm([3]), seq=None)

        self.assertEqual(u.pcm(), pcm([1, 2, 3]))
        self.assertEqual(u.gaps(), 0)

    def test_partial_window_keeps_the_most_recent_audio(self):
        """Partials re-decode a bounded window. It has to be the tail: the
        newest speech is the part a partial is supposed to reveal."""
        u = engine.Utterance(id="u", sample_rate=1000)
        for i in range(10):  # 10 chunks of 1000 samples = 10 seconds
            u.add(pcm([i] * 1000), seq=i)

        window = u.pcm(max_seconds=2)
        self.assertEqual(len(window), 2 * 1000 * 2)
        self.assertEqual(window, pcm([8] * 1000 + [9] * 1000))

    def test_duration_is_reported_in_seconds_of_audio(self):
        u = engine.Utterance(id="u", sample_rate=16000)
        u.add(pcm([0] * 8000), seq=0)
        self.assertAlmostEqual(u.duration(), 0.5, places=3)


class SilenceDetection(unittest.TestCase):
    def test_digital_silence_is_silent(self):
        self.assertEqual(engine.rms(pcm([0] * 100)), 0.0)

    def test_loud_audio_is_not_silent(self):
        self.assertGreater(engine.rms(pcm([12000, -12000] * 50)), 1000)

    def test_room_tone_stays_below_the_default_threshold(self):
        """The endpoint detector must not fire on a quiet room, or it cuts
        people off between words."""
        quiet = engine.rms(pcm([60, -40, 30, -55] * 50))
        self.assertLess(quiet, 500)

    def test_an_empty_or_odd_length_chunk_does_not_crash(self):
        self.assertEqual(engine.rms(b""), 0.0)
        self.assertEqual(engine.rms(b"\x01"), 0.0)


class UtteranceSweep(unittest.TestCase):
    """The sweep that drops abandoned utterances (main.py has no session-end
    hook to rely on instead) — exercised directly against engine's module
    state, restored afterwards so tests stay isolated from each other."""

    def setUp(self):
        self._utterances_backup = dict(engine.utterances)
        self._last_seen_backup = dict(engine.last_seen)
        engine.utterances.clear()
        engine.last_seen.clear()

    def tearDown(self):
        engine.utterances.clear()
        engine.utterances.update(self._utterances_backup)
        engine.last_seen.clear()
        engine.last_seen.update(self._last_seen_backup)

    def test_an_idle_utterance_past_the_cutoff_is_dropped(self):
        import time

        key = ("session-1", "node-1")
        engine.utterances[key] = engine.Utterance(id="u")
        engine.last_seen[key] = time.monotonic() - 121  # just past the 120s cutoff

        engine.sweep()

        self.assertNotIn(key, engine.utterances)
        self.assertNotIn(key, engine.last_seen)

    def test_a_recently_active_utterance_survives_the_sweep(self):
        import time

        key = ("session-1", "node-1")
        engine.utterances[key] = engine.Utterance(id="u")
        engine.last_seen[key] = time.monotonic()

        engine.sweep()

        self.assertIn(key, engine.utterances)
        self.assertIn(key, engine.last_seen)


if __name__ == "__main__":
    unittest.main()
