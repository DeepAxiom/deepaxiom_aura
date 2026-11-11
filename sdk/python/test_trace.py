"""C3 v1.7 `trace` on the Python envelope: carried, never invented.

    cd sdk/python && PYTHONPATH=src python -m unittest test_trace -v
"""
import asyncio
import unittest

from aura import Context, Envelope, Skill

TRACE = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

MANIFEST = {
    "id": "acme/logical/echo", "version": "1.0.0", "protocol": "1",
    "name": "echo", "description": "test", "capability": "logical.echo",
    "type": "logical", "format": "source",
    "ports": {"ingress": [{"name": "text_in", "schema": "std/text@1"}],
              "egress": [{"name": "text_out", "schema": "std/text@1"}]},
}


class TraceOnTheEnvelope(unittest.TestCase):
    def test_round_trips_through_the_wire_form(self):
        env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data", "trace": TRACE})
        self.assertEqual(env.trace, TRACE)
        self.assertEqual(env.to_wire()["trace"], TRACE)

    def test_absent_stays_absent(self):
        env = Envelope.from_wire({"v": "1", "id": "a", "kind": "data"})
        self.assertEqual(env.trace, "")
        self.assertNotIn("trace", env.to_wire())

    def test_an_emitted_envelope_carries_the_incoming_context(self):
        skill = Skill(manifest=MANIFEST, token="")
        sent: list[Envelope] = []

        async def capture(env: Envelope) -> None:
            sent.append(env)

        skill._send = capture
        incoming = Envelope.from_wire({
            "v": "1", "id": "in-1", "kind": "data", "session": "s", "node": "echo",
            "port": "text_in", "idem": "s:1", "schema": "std/text@1",
            "payload": {"text": "hi"}, "trace": TRACE})
        asyncio.run(Context(skill, incoming).emit("text_out", {"text": "hi", "final": True}))
        self.assertEqual(sent[0].trace, TRACE)
        self.assertEqual(sent[0].cause_id, "in-1")

    def test_no_incoming_context_means_none_emitted(self):
        skill = Skill(manifest=MANIFEST, token="")
        sent: list[Envelope] = []

        async def capture(env: Envelope) -> None:
            sent.append(env)

        skill._send = capture
        incoming = Envelope.from_wire({
            "v": "1", "id": "in-1", "kind": "data", "session": "s", "node": "echo",
            "port": "text_in", "idem": "s:1", "schema": "std/text@1", "payload": {"text": "hi"}})
        asyncio.run(Context(skill, incoming).emit("text_out", {"text": "hi", "final": True}))
        self.assertNotIn("trace", sent[0].to_wire())


if __name__ == "__main__":
    unittest.main()
