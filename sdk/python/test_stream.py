"""DeltaCoalescer and compact_history — pure, no kernel, no model.

    cd sdk/python && PYTHONPATH=src python -m unittest test_stream -v
"""
import asyncio
import unittest

from aura import DeltaCoalescer, compact_history


class _Clock:
    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now


class Coalescer(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.sent: list[str] = []

    async def _emit(self, text: str) -> None:
        self.sent.append(text)

    async def test_first_delta_is_never_delayed(self):
        out = DeltaCoalescer(self._emit, max_delay=10.0)
        await out.add("first")
        self.assertEqual(self.sent, ["first"])
        await out.discard()

    async def test_deltas_inside_the_window_travel_together(self):
        clock = _Clock()
        out = DeltaCoalescer(self._emit, max_delay=0.05, clock=clock)
        await out.add("a")
        for piece in ("b", "c", "d"):
            clock.now += 0.001
            await out.add(piece)
        self.assertEqual(self.sent, ["a"])
        await out.close()
        self.assertEqual(self.sent, ["a", "bcd"])

    async def test_a_slow_stream_is_still_one_envelope_per_token(self):
        clock = _Clock()
        out = DeltaCoalescer(self._emit, max_delay=0.025, clock=clock)
        for piece in ("a", "b", "c"):
            await out.add(piece)
            clock.now += 0.05
        await out.close()
        self.assertEqual(self.sent, ["a", "b", "c"])

    async def test_a_stalled_stream_does_not_strand_a_buffered_delta(self):
        out = DeltaCoalescer(self._emit, max_delay=0.02)
        await out.add("a")
        await out.add("b")  # buffered: inside the window
        self.assertEqual(self.sent, ["a"])
        await asyncio.sleep(0.15)  # the producer stalls; the timer must flush
        self.assertEqual(self.sent, ["a", "b"])
        await out.close()
        self.assertEqual(self.sent, ["a", "b"])

    async def test_a_full_buffer_flushes_without_waiting(self):
        clock = _Clock()
        out = DeltaCoalescer(self._emit, max_delay=10.0, max_chars=4, clock=clock)
        await out.add("x")
        await out.add("ab")
        await out.add("cd")
        self.assertEqual(self.sent, ["x", "abcd"])
        await out.close()

    async def test_zero_delay_is_a_passthrough(self):
        out = DeltaCoalescer(self._emit, max_delay=0)
        for piece in ("a", "b", "c"):
            await out.add(piece)
        await out.close()
        self.assertEqual(self.sent, ["a", "b", "c"])

    async def test_nothing_is_lost_or_reordered_under_a_slow_emit(self):
        async def slow_emit(text: str) -> None:
            await asyncio.sleep(0.005)
            self.sent.append(text)

        out = DeltaCoalescer(slow_emit, max_delay=0.01)
        pieces = [f"{i}," for i in range(300)]
        for i, piece in enumerate(pieces):
            await out.add(piece)
            if i % 7 == 0:
                await asyncio.sleep(0.003)
        await out.close()
        self.assertEqual("".join(self.sent), "".join(pieces))
        self.assertLess(len(self.sent), len(pieces))

    async def test_discard_drops_what_was_buffered(self):
        clock = _Clock()
        out = DeltaCoalescer(self._emit, max_delay=5.0, clock=clock)
        await out.add("a")
        await out.add("b")
        await out.discard()
        await out.close()
        self.assertEqual(self.sent, ["a"])

    async def test_empty_deltas_are_not_envelopes(self):
        out = DeltaCoalescer(self._emit, max_delay=0)
        await out.add("")
        await out.close()
        self.assertEqual(self.sent, [])


def _conv(n: int, system: bool = True) -> list[dict]:
    msgs = [{"role": "system", "content": "sys"}] if system else []
    for i in range(n):
        msgs.append({"role": "user" if i % 2 == 0 else "assistant", "content": f"m{i}"})
    return msgs


class CompactHistory(unittest.TestCase):
    def test_under_the_limit_nothing_moves(self):
        msgs = _conv(6)
        self.assertEqual(compact_history(msgs, 40), 0)
        self.assertEqual(len(msgs), 7)

    def test_over_the_limit_the_oldest_half_goes_at_once(self):
        msgs = _conv(40)  # 41 with the system prompt
        dropped = compact_history(msgs, 40)
        self.assertEqual(dropped, 20)
        self.assertEqual(msgs[0]["role"], "system")
        self.assertEqual(msgs[1], {"role": "user", "content": "m20"})
        self.assertEqual(len(msgs), 21)

    def test_never_starts_on_an_assistant_turn(self):
        for n in range(3, 60):
            for limit in (2, 3, 4, 7, 12, 40):
                msgs = _conv(n)
                compact_history(msgs, limit)
                self.assertEqual(msgs[1]["role"], "user", (n, limit))

    def test_without_a_system_prompt(self):
        msgs = _conv(10, system=False)
        compact_history(msgs, 6)
        self.assertEqual(msgs[0]["role"], "user")
        self.assertLessEqual(len(msgs), 6)

    def test_a_degenerate_limit_is_left_alone(self):
        msgs = _conv(10)
        self.assertEqual(compact_history(msgs, 1), 0)
        self.assertEqual(len(msgs), 11)

    def test_the_prefix_is_stable_between_compactions(self):
        msgs = _conv(0)
        resets = 0
        previous: list[dict] = []
        for i in range(200):
            msgs.append({"role": "user", "content": f"q{i}"})
            prompt = [dict(m) for m in msgs]
            if prompt[:len(previous)] != previous:
                resets += 1
            msgs.append({"role": "assistant", "content": f"a{i}"})
            compact_history(msgs, 40)
            previous = prompt
        # 400 messages through a 40-message window: one reset per ~10 turns,
        # not one per turn.
        self.assertLess(resets, 25)
        self.assertGreater(resets, 5)


if __name__ == "__main__":
    unittest.main()
