"""C3 `deadline`, `respond_by` and `priority` on the Python side: read by a
handler, never carried back by one.

    cd sdk/python && PYTHONPATH=src python -m unittest test_deadline -v
"""
import asyncio
import time
import unittest

from aura import Context, Envelope, Skill

MANIFEST = {
    "id": "acme/logical/echo", "version": "1.0.0", "protocol": "1",
    "name": "echo", "description": "test", "capability": "logical.echo",
    "type": "logical", "format": "source",
    "ports": {"ingress": [{"name": "text_in", "schema": "std/text@1"}],
              "egress": [{"name": "text_out", "schema": "std/text@1"}]},
}


def now_ms() -> int:
    return int(time.time() * 1000)


def delivery(**extra) -> Envelope:
    wire = {"v": "1", "id": "in-1", "kind": "data", "session": "s", "node": "echo",
            "port": "text_in", "idem": "s:1", "schema": "std/text@1",
            "payload": {"text": "hi"}}
    wire.update(extra)
    return Envelope.from_wire(wire)


def context(**extra) -> Context:
    return Context(Skill(manifest=MANIFEST, token=""), delivery(**extra))


class BoundsOnTheEnvelope(unittest.TestCase):
    def test_round_trip_through_the_wire_form(self):
        env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data",
                                  "deadline": 1_800_000_000_500, "respond_by": 1_800_000_000_100,
                                  "priority": 7})
        self.assertEqual((env.deadline, env.respond_by, env.priority),
                         (1_800_000_000_500, 1_800_000_000_100, 7))
        wire = env.to_wire()
        self.assertEqual(wire["deadline"], 1_800_000_000_500)
        self.assertEqual(wire["respond_by"], 1_800_000_000_100)
        self.assertEqual(wire["priority"], 7)

    def test_absent_stays_absent(self):
        env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data"})
        self.assertEqual((env.deadline, env.respond_by, env.priority), (0, 0, 0))
        for key in ("deadline", "respond_by", "priority"):
            self.assertNotIn(key, env.to_wire())

    def test_nonsense_is_not_believed(self):
        # A handler told it is late on the strength of a field nobody could
        # have produced would throw away good work.
        for bad in ("soon", -5, 0, None, 1.5, True, [1]):
            env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data",
                                      "deadline": bad, "respond_by": bad})
            self.assertEqual((env.deadline, env.respond_by), (0, 0), f"believed {bad!r}")
        for bad in ("high", None, 2.5, True):
            env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data", "priority": bad})
            self.assertEqual(env.priority, 0, f"believed priority {bad!r}")

    def test_a_negative_priority_is_a_priority(self):
        # Lower than ordinary is a legitimate thing for an edge to declare.
        env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data", "priority": -3})
        self.assertEqual(env.priority, -3)


class WhatAHandlerSees(unittest.TestCase):
    def test_no_bounds(self):
        ctx = context()
        self.assertIsNone(ctx.deadline)
        self.assertIsNone(ctx.respond_by)
        self.assertIsNone(ctx.remaining_ms())
        self.assertIsNone(ctx.respond_within_ms())
        self.assertFalse(ctx.expired)
        self.assertEqual(ctx.priority, 0)

    def test_time_left(self):
        deadline = now_ms() + 60_000
        ctx = context(deadline=deadline, respond_by=deadline - 59_000, priority=4)
        self.assertEqual(ctx.deadline, deadline)
        self.assertEqual(ctx.respond_by, deadline - 59_000)
        self.assertEqual(ctx.priority, 4)
        left = ctx.remaining_ms()
        self.assertTrue(0 < left <= 60_000, left)
        self.assertTrue(0 < ctx.respond_within_ms() <= 1_000)
        self.assertFalse(ctx.expired)

    def test_a_deadline_that_has_passed(self):
        ctx = context(deadline=now_ms() - 5_000, respond_by=now_ms() - 9_000)
        self.assertEqual(ctx.remaining_ms(), 0, "time left must never be negative")
        self.assertEqual(ctx.respond_within_ms(), 0)
        self.assertTrue(ctx.expired)

    def test_a_late_response_is_not_an_expired_delivery(self):
        # The two bounds are independent: late to start is not too late to matter.
        ctx = context(respond_by=now_ms() - 1_000)
        self.assertFalse(ctx.expired)
        self.assertIsNone(ctx.remaining_ms())

    def test_an_emission_does_not_carry_the_bounds_back(self):
        # The kernel inherits a chain's bounds from its own record of this
        # delivery. An Expertise that carried its own budget could extend it.
        skill = Skill(manifest=MANIFEST, token="")
        sent: list[Envelope] = []

        async def capture(env: Envelope) -> None:
            sent.append(env)

        skill._send = capture
        incoming = delivery(deadline=now_ms() + 60_000, respond_by=now_ms() + 300, priority=9)
        asyncio.run(Context(skill, incoming).emit("text_out", {"text": "hi", "final": True}))
        wire = sent[0].to_wire()
        for key in ("deadline", "respond_by", "priority"):
            self.assertNotIn(key, wire, f"an emission echoed {key}")
        self.assertEqual(sent[0].cause_id, "in-1", "the cause is what the kernel inherits through")


if __name__ == "__main__":
    unittest.main()
