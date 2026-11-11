"""Tests for the decode gate (scheduler.py), without a model.

Real threads, plain functions. Every test holds the gate with a job that
blocks on an Event, queues what it wants to observe behind it, and only then
lets go — so the order asserted is the order the gate chose, not the order
the OS happened to start threads in.

    cd expertise/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import threading
import time
import unittest

from scheduler import FINAL, PARTIAL, DecodeGate, DecodeSkipped

TIMEOUT = 5.0


def wait_for(predicate, timeout=TIMEOUT):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.002)
    return False


class Harness:
    """Runs gate jobs on threads and records how each one ended."""

    def __init__(self, test, **gate_kwargs):
        self.test = test
        self.gate = DecodeGate(**gate_kwargs)
        self.order = []          # names, in the order their function STARTED
        self.results = {}        # name -> return value | exception instance
        self.threads = []
        self._lock = threading.Lock()

    def submit(self, name, priority, key=None, stop=None, fn=None, queued=True):
        """Start a job and, unless told otherwise, do not return until it is
        observably in the queue — submission order must be the test's."""
        before = self.gate.queued

        def job():
            with self._lock:
                self.order.append(name)
            return fn() if fn else name

        def body():
            try:
                self.results[name] = self.gate.run(job, (), priority, key, stop)
            except BaseException as exc:  # noqa: BLE001
                self.results[name] = exc

        t = threading.Thread(target=body, daemon=True)
        t.start()
        self.threads.append(t)
        if queued:
            self.test.assertTrue(
                wait_for(lambda: self.gate.queued > before or name in self.results),
                f"{name} never reached the queue")
        return t

    def hold(self):
        """Occupy the gate. Returns the Event that releases it."""
        release = threading.Event()
        self.submit("holder", FINAL, fn=lambda: release.wait(TIMEOUT), queued=False)
        self.test.assertTrue(wait_for(self.gate.busy), "holder never started")
        return release

    def join(self):
        for t in self.threads:
            t.join(TIMEOUT)
            self.test.assertFalse(t.is_alive(), "a gate caller is stuck")

    def skipped(self, name):
        return isinstance(self.results.get(name), DecodeSkipped)


class Ordering(unittest.TestCase):
    def test_an_idle_gate_runs_the_job_and_returns_its_value(self):
        gate = DecodeGate()
        self.assertEqual(gate.run(lambda a, b: a + b, (2, 3)), 5)
        self.assertEqual(gate.run(lambda: "x", (), PARTIAL, "k"), "x")
        self.assertFalse(gate.busy())
        self.assertEqual(gate.waiting(), 0)

    def test_finals_run_before_partials_that_queued_earlier(self):
        h = Harness(self)
        release = h.hold()
        h.submit("partial-a", PARTIAL, key="a")
        h.submit("partial-b", PARTIAL, key="b")
        h.submit("final-c", FINAL, key="c")
        h.submit("final-d", FINAL, key="d")
        release.set()
        h.join()

        self.assertEqual(
            h.order, ["holder", "final-c", "final-d", "partial-a", "partial-b"])

    def test_same_priority_is_first_come_first_served(self):
        h = Harness(self)
        release = h.hold()
        for i in range(4):
            h.submit(f"p{i}", PARTIAL, key=i)
        release.set()
        h.join()
        self.assertEqual(h.order, ["holder", "p0", "p1", "p2", "p3"])

    def test_only_one_job_runs_at_a_time(self):
        gate = DecodeGate(max_waiting_partials=100)
        running, peak, lock = [0], [0], threading.Lock()

        def job():
            with lock:
                running[0] += 1
                peak[0] = max(peak[0], running[0])
            time.sleep(0.002)
            with lock:
                running[0] -= 1

        threads = [threading.Thread(
            target=gate.run, args=(job, (), FINAL if i % 3 == 0 else PARTIAL, i))
            for i in range(24)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(TIMEOUT)
        self.assertEqual(peak[0], 1)
        self.assertEqual(gate.ran, 24)


class Skipping(unittest.TestCase):
    def test_a_newer_partial_for_the_same_utterance_replaces_the_queued_one(self):
        h = Harness(self)
        release = h.hold()
        h.submit("old", PARTIAL, key="utt")
        h.submit("new", PARTIAL, key="utt")
        self.assertTrue(wait_for(lambda: "old" in h.results))
        release.set()
        h.join()

        self.assertTrue(h.skipped("old"))
        self.assertIn("superseded", str(h.results["old"]))
        self.assertEqual(h.order, ["holder", "new"])

    def test_a_final_drops_the_queued_partial_of_its_own_utterance_only(self):
        h = Harness(self)
        release = h.hold()
        h.submit("mine", PARTIAL, key="utt-1")
        h.submit("theirs", PARTIAL, key="utt-2")
        h.submit("final", FINAL, key="utt-1")
        release.set()
        h.join()

        self.assertTrue(h.skipped("mine"))
        self.assertEqual(h.order, ["holder", "final", "theirs"])

    def test_partials_without_a_key_never_supersede_each_other(self):
        h = Harness(self)
        release = h.hold()
        h.submit("a", PARTIAL)
        h.submit("b", PARTIAL)
        release.set()
        h.join()
        self.assertEqual(h.order, ["holder", "a", "b"])

    def test_a_partial_cancelled_while_waiting_is_skipped(self):
        h = Harness(self)
        release = h.hold()
        stop = threading.Event()
        h.submit("cancelled", PARTIAL, key="a", stop=stop)
        h.submit("other", PARTIAL, key="b")
        stop.set()
        # It leaves the queue on its own, without the gate being released.
        self.assertTrue(wait_for(lambda: "cancelled" in h.results))
        self.assertTrue(h.gate.busy())
        release.set()
        h.join()

        self.assertTrue(h.skipped("cancelled"))
        self.assertEqual(h.order, ["holder", "other"])

    def test_a_partial_already_cancelled_is_refused_at_the_door(self):
        gate = DecodeGate()
        stop = threading.Event()
        stop.set()
        ran = []
        with self.assertRaises(DecodeSkipped):
            gate.run(lambda: ran.append(1), (), PARTIAL, "k", stop)
        self.assertEqual(ran, [])
        self.assertEqual(gate.skipped, 1)

    def test_a_cancelled_final_still_runs(self):
        """What a cancelled final means is the caller's decision (today the
        decode function returns "" and the handler still emits). The gate
        must not make that decision by dropping it."""
        gate = DecodeGate()
        stop = threading.Event()
        stop.set()
        self.assertEqual(gate.run(lambda: "ran", (), FINAL, "k", stop), "ran")

    def test_too_many_waiting_partials_sheds_the_newcomer(self):
        h = Harness(self, max_waiting_partials=2)
        release = h.hold()
        h.submit("p1", PARTIAL, key=1)
        h.submit("p2", PARTIAL, key=2)
        h.submit("p3", PARTIAL, key=3, queued=False)
        self.assertTrue(wait_for(lambda: "p3" in h.results))
        h.submit("final", FINAL, key=4)      # finals are never shed
        release.set()
        h.join()

        self.assertTrue(h.skipped("p3"))
        self.assertIn("shed", str(h.results["p3"]))
        self.assertEqual(h.order, ["holder", "final", "p1", "p2"])

    def test_a_superseded_slot_is_free_for_its_replacement(self):
        h = Harness(self, max_waiting_partials=1)
        release = h.hold()
        h.submit("old", PARTIAL, key="utt")
        h.submit("new", PARTIAL, key="utt")
        release.set()
        h.join()
        self.assertTrue(h.skipped("old"))
        self.assertEqual(h.results["new"], "new")


class Release(unittest.TestCase):
    def test_a_job_that_raises_releases_the_gate(self):
        gate = DecodeGate()

        def boom():
            raise RuntimeError("decode failed")

        with self.assertRaises(RuntimeError):
            gate.run(boom, (), FINAL, "k")
        self.assertFalse(gate.busy())
        self.assertEqual(gate.run(lambda: "next"), "next")

    def test_a_failing_job_does_not_strand_the_ones_behind_it(self):
        h = Harness(self)
        release = threading.Event()

        def boom():
            release.wait(TIMEOUT)
            raise RuntimeError("decode failed")

        h.submit("boom", FINAL, fn=boom, queued=False)
        self.assertTrue(wait_for(h.gate.busy))
        h.submit("after-1", PARTIAL, key=1)
        h.submit("after-2", FINAL, key=2)
        release.set()
        h.join()

        self.assertIsInstance(h.results["boom"], RuntimeError)
        self.assertEqual(h.order, ["boom", "after-2", "after-1"])

    def test_a_cancelled_head_of_line_does_not_block_those_behind(self):
        """The cancelled partial is first in line when the gate frees up; if
        being skipped did not hand the turn on, everyone behind it would wait
        forever."""
        h = Harness(self, poll_s=0.01)
        release = h.hold()
        stop = threading.Event()
        h.submit("head", PARTIAL, key="a", stop=stop)
        h.submit("behind", PARTIAL, key="b")
        stop.set()
        release.set()
        h.join()

        self.assertTrue(h.skipped("head"))
        self.assertEqual(h.results["behind"], "behind")
        self.assertFalse(h.gate.busy())
        self.assertEqual(h.gate.waiting(), 0)

    def test_everything_is_released_when_every_waiter_is_cancelled(self):
        h = Harness(self, max_waiting_partials=10)
        release = h.hold()
        stop = threading.Event()
        for i in range(6):
            h.submit(f"p{i}", PARTIAL, key=i, stop=stop)
        stop.set()
        self.assertTrue(wait_for(lambda: len(h.results) == 6))
        self.assertEqual(h.gate.waiting(), 0)
        release.set()
        h.join()

        self.assertEqual(h.order, ["holder"])
        self.assertFalse(h.gate.busy())
        self.assertEqual(h.gate.run(lambda: "still works"), "still works")

    def test_a_job_abandoning_itself_mid_run_releases_the_gate(self):
        """The decode function may raise DecodeSkipped itself when its stop
        event fires halfway. Same exception, same clean exit."""
        gate = DecodeGate()

        def abandon():
            raise DecodeSkipped("cancelled mid-decode")

        with self.assertRaises(DecodeSkipped):
            gate.run(abandon, (), PARTIAL, "k")
        self.assertFalse(gate.busy())


if __name__ == "__main__":
    unittest.main()
