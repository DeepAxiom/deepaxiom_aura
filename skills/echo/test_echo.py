"""Tests for the echo handler.

echo has no domain logic to speak of — the point of testing it is to prove
the handler contract itself (`skill.config`, `ctx.payload`, `ctx.emit`) holds
for the smallest possible skill, the same contract every other skill relies
on.

    cd skills/echo && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import asyncio
import unittest

import main
from aura import Envelope
from aura.envelope import KIND_DATA


def run_handler(payload, prefix=""):
    """Invoke the text_in handler with a synthetic incoming envelope and
    capture what it emits, without a real kernel connection.

    `ctx.emit` ends up in `Skill._send`, which requires a live websocket —
    swap it for a list-appending stub so the handler can run standalone.
    """
    main.skill.config["prefix"] = prefix
    sent: list[Envelope] = []

    async def fake_send(env: Envelope) -> None:
        sent.append(env)

    main.skill._send = fake_send

    incoming = Envelope(
        kind=KIND_DATA, session="s1", node="echo-1", port="text_in", payload=payload
    )
    ctx = main.Context(main.skill, incoming)
    asyncio.run(main.handle(ctx))
    return sent


class TextInHandler(unittest.TestCase):
    def test_prepends_the_configured_prefix(self):
        sent = run_handler({"text": "hello"}, prefix="bot: ")

        self.assertEqual(len(sent), 1)
        self.assertEqual(sent[0].port, "text_out")
        self.assertEqual(sent[0].payload, {"text": "bot: hello", "final": True})

    def test_empty_prefix_returns_the_text_unchanged(self):
        """The default config value: two echo skills should be
        indistinguishable until someone sets a prefix."""
        sent = run_handler({"text": "hello"}, prefix="")

        self.assertEqual(sent[0].payload, {"text": "hello", "final": True})

    def test_missing_text_key_falls_back_to_empty_string(self):
        sent = run_handler({}, prefix="bot: ")

        self.assertEqual(sent[0].payload, {"text": "bot: ", "final": True})

    def test_empty_payload_does_not_crash(self):
        """`ctx.payload` can be `None` (e.g. a bare `done`-adjacent probe) —
        the handler must not raise on it."""
        sent = run_handler(None, prefix="bot: ")

        self.assertEqual(sent[0].payload, {"text": "bot: ", "final": True})

    def test_reply_is_always_marked_final(self):
        """echo answers in one shot, so `final` is never conditional."""
        sent = run_handler({"text": "x"}, prefix="")

        self.assertTrue(sent[0].payload["final"])


if __name__ == "__main__":
    unittest.main()
