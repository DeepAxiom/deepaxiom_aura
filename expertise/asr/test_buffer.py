"""Tests for how `Utterance` holds its audio.

test_streaming.py pins down WHAT the buffer returns (order, gaps, the partial
window). This file pins down that it no longer re-joins every chunk on every
read — the handler asks for the length several times per chunk, which made an
utterance quadratic — and the pieces added for incremental decoding: reading
from an offset, and reporting chunks that arrived out of order.

    cd expertise/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import random
import struct
import threading
import time
import unittest

import engine


def pcm(samples):
    return struct.pack(f"<{len(samples)}h", *samples)


def joined(u):
    """What the old implementation returned: the definition of correct."""
    return b"".join(u.chunks[k] for k in sorted(u.chunks))


class AppendsAreCheap(unittest.TestCase):
    def test_in_order_appends_extend_one_buffer_in_place(self):
        u = engine.Utterance(id="u")
        u.add(pcm([1, 2]), seq=0)
        buf = u._buf
        for i in range(1, 50):
            u.add(pcm([i, i]), seq=i)
            u.duration()
            u.nbytes

        self.assertIs(u._buf, buf)          # never rebuilt
        self.assertEqual(u.pcm(), joined(u))
        self.assertEqual(u.nbytes, 50 * 4)

    def test_length_and_duration_do_not_copy_the_audio(self):
        """A long utterance in small chunks, asking for the duration after
        every chunk as the handler does. The old join-per-call version moves
        about 30 GB for this and takes minutes; this takes milliseconds. The
        bound is generous on purpose — it separates linear from quadratic,
        not fast machines from slow ones."""
        u = engine.Utterance(id="u", sample_rate=16000)
        chunk = b"\x01\x00" * 160
        started = time.perf_counter()
        for i in range(20000):
            u.add(chunk, seq=i)
            u.duration()
            u.nbytes
        elapsed = time.perf_counter() - started

        self.assertLess(elapsed, 5.0)
        self.assertAlmostEqual(u.duration(), 200.0)

    def test_chunks_without_seq_take_the_fast_path_too(self):
        u = engine.Utterance(id="u")
        u.add(pcm([1]), None)
        buf = u._buf
        u.add(pcm([2]), None)
        u.add(pcm([3]), None)
        self.assertIs(u._buf, buf)
        self.assertEqual(u.pcm(), pcm([1, 2, 3]))

    def test_a_gap_does_not_force_a_rebuild(self):
        """A dropped chunk leaves a hole in the seqs, not in the buffer."""
        u = engine.Utterance(id="u")
        u.add(pcm([1]), seq=0)
        buf = u._buf
        u.add(pcm([3]), seq=2)
        u.add(pcm([4]), seq=3)
        self.assertIs(u._buf, buf)
        self.assertEqual(u.pcm(), pcm([1, 3, 4]))
        self.assertEqual(u.gaps(), 1)
        self.assertEqual(u.take_inserts(), [])


class SameAnswersAsBefore(unittest.TestCase):
    def test_any_arrival_order_gives_the_seq_ordered_audio(self):
        rng = random.Random(3)
        for _ in range(50):
            order = list(range(rng.randint(1, 30)))
            rng.shuffle(order)
            u = engine.Utterance(id="u", sample_rate=1000)
            for n, seq in enumerate(order):
                u.add(pcm([seq] * rng.randint(0, 5)), seq=seq)
                if n % 3 == 0:     # read mid-way, so rebuilds interleave with adds
                    self.assertEqual(u.pcm(), joined(u))
                    self.assertEqual(u.nbytes, len(joined(u)))
            self.assertEqual(u.pcm(), joined(u))
            self.assertAlmostEqual(u.duration(), len(joined(u)) / 2 / 1000)

    def test_a_resent_seq_replaces_the_chunk_it_names(self):
        u = engine.Utterance(id="u")
        u.add(pcm([1, 1]), seq=0)
        u.add(pcm([2, 2]), seq=1)
        u.add(pcm([9]), seq=0)
        self.assertEqual(u.pcm(), pcm([9, 2, 2]))
        self.assertEqual(u.nbytes, 6)
        self.assertEqual(u.gaps(), 0)

    def test_the_tail_window_matches_a_slice_of_the_whole(self):
        u = engine.Utterance(id="u", sample_rate=1000)
        for i in range(7):
            u.add(pcm([i] * 300), seq=i)
        whole = u.pcm()
        for seconds in (0.001, 0.5, 1, 2.05, 100):
            keep = int(seconds * 1000) * 2
            want = whole[-keep:] if len(whole) > keep else whole
            self.assertEqual(u.pcm(max_seconds=seconds), want)
        self.assertEqual(u.pcm(max_seconds=None), whole)

    def test_pcm_returns_immutable_bytes_detached_from_the_buffer(self):
        """A decode on a worker thread holds what it was given while the
        utterance keeps growing."""
        u = engine.Utterance(id="u")
        u.add(pcm([1, 2]), seq=0)
        snapshot = u.pcm()
        u.add(pcm([3]), seq=1)
        self.assertIsInstance(snapshot, bytes)
        self.assertEqual(snapshot, pcm([1, 2]))

    def test_an_empty_utterance(self):
        u = engine.Utterance(id="u")
        self.assertEqual(u.pcm(), b"")
        self.assertEqual(u.pcm(max_seconds=1), b"")
        self.assertEqual(u.pcm_from(0), b"")
        self.assertEqual(u.nbytes, 0)
        self.assertEqual(u.duration(), 0.0)

    def test_writing_to_chunks_directly_is_survived(self):
        """Not the supported way in, but nothing stops it: the count no
        longer matches and the buffer is rebuilt rather than trusted."""
        u = engine.Utterance(id="u")
        u.add(pcm([1]), seq=0)
        u.chunks[1] = pcm([2])
        self.assertEqual(u.pcm(), pcm([1, 2]))
        self.assertEqual(u.nbytes, 4)
        u.add(pcm([3]), seq=2)
        self.assertEqual(u.pcm(), pcm([1, 2, 3]))


class ForIncrementalDecoding(unittest.TestCase):
    def test_pcm_from_returns_the_audio_after_an_offset(self):
        u = engine.Utterance(id="u")
        for i in range(4):
            u.add(pcm([i, i]), seq=i)
        self.assertEqual(u.pcm_from(0), u.pcm())
        self.assertEqual(u.pcm_from(4), pcm([1, 1, 2, 2, 3, 3]))
        self.assertEqual(u.pcm_from(16), b"")
        self.assertEqual(u.pcm_from(999), b"")
        self.assertEqual(u.pcm_from(-5), u.pcm())

    def test_an_out_of_order_chunk_is_reported_with_where_it_landed(self):
        u = engine.Utterance(id="u")
        u.add(pcm([0, 0]), seq=0)
        u.add(pcm([2, 2, 2]), seq=2)
        self.assertEqual(u.take_inserts(), [])
        u.add(pcm([1]), seq=1)                 # fills the gap, late

        self.assertEqual(u.take_inserts(), [(4, 2)])   # after seq 0, 2 bytes
        self.assertEqual(u.take_inserts(), [])         # reported once
        self.assertEqual(u.pcm(), pcm([0, 0, 1, 2, 2, 2]))

    def test_a_resend_reports_the_change_in_length(self):
        u = engine.Utterance(id="u")
        u.add(pcm([1, 1]), seq=0)
        u.add(pcm([2]), seq=1)
        u.add(pcm([7, 7, 7]), seq=0)
        self.assertEqual(u.take_inserts(), [(0, 2)])

    def test_reads_from_another_thread_see_whole_chunks(self):
        """The local_agreement decode reads on a worker while the event loop
        appends. Every read must be a prefix of the final audio."""
        u = engine.Utterance(id="u")
        chunks = [pcm([i] * 64) for i in range(2000)]
        seen, stop = [], threading.Event()

        def reader():
            while not stop.is_set():
                seen.append(u.pcm_from(0))

        t = threading.Thread(target=reader)
        t.start()
        for i, c in enumerate(chunks):
            u.add(c, seq=i)
        stop.set()
        t.join(5)

        whole = b"".join(chunks)
        self.assertEqual(u.pcm(), whole)
        for data in seen:
            self.assertEqual(len(data) % 128, 0)
            self.assertEqual(data, whole[:len(data)])


if __name__ == "__main__":
    unittest.main()
