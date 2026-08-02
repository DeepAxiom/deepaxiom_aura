"""Tests for memory-context's text-command parsing and dispatch.

`_parse_remember` is exercised directly as a pure function. The full command
routing in `handle()` goes through the real `aura.Context`/`Skill` handler
contract (same technique as skills/echo/test_echo.py): a synthetic envelope
in, `Skill._send` stubbed to a list instead of a live websocket.

Importing `main` runs its module-level `store.connect(skill.config[...])`,
which would otherwise write to the real `~/.aura/memory/context.sqlite`.
`store.connect` is patched to a throwaway temp file *before* `main` is
imported so that side effect lands in a temp directory instead.

    cd skills/memory-context && PYTHONPATH=../../sdk/python/src python -m unittest test_commands -v
"""
import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import store

_tmpdir = tempfile.TemporaryDirectory()
_real_connect = store.connect
with mock.patch.object(
    store, "connect", lambda path: _real_connect(str(Path(_tmpdir.name) / "context.sqlite"))
):
    import main  # noqa: E402 — must import after the store.connect patch above

from aura import Envelope
from aura.envelope import KIND_DATA


def tearDownModule():
    # Windows keeps an exclusive lock on an open sqlite file — close the
    # connection main.py opened at import time before the temp dir cleans up.
    main._DB.close()
    _tmpdir.cleanup()


def run_handler(payload, session="s1", config=None):
    """Invoke the event_in handler with a synthetic incoming envelope and
    capture what it emits, without a real kernel connection."""
    main.skill.config.update(config or {})
    sent: list[Envelope] = []

    async def fake_send(env: Envelope) -> None:
        sent.append(env)

    main.skill._send = fake_send

    incoming = Envelope(
        kind=KIND_DATA, session=session, node="memory-1", port="event_in", payload=payload
    )
    ctx = main.Context(main.skill, incoming)
    asyncio.run(main.handle(ctx))
    return sent


class ParseRemember(unittest.TestCase):
    def test_defaults_role_to_user_when_absent(self):
        self.assertEqual(main._parse_remember("hola"), ("user", "hola"))

    def test_picks_up_an_explicit_role(self):
        self.assertEqual(main._parse_remember("assistant: hi there"), ("assistant", "hi there"))

    def test_is_case_insensitive_on_the_role(self):
        self.assertEqual(main._parse_remember("SYSTEM: be nice"), ("system", "be nice"))

    def test_an_unknown_role_prefix_is_treated_as_content_not_a_role(self):
        self.assertEqual(main._parse_remember("bob: hi"), ("user", "bob: hi"))

    def test_empty_content_returns_none(self):
        self.assertIsNone(main._parse_remember(""))
        self.assertIsNone(main._parse_remember("user:   "))


class RememberCommand(unittest.TestCase):
    def test_stores_and_acknowledges(self):
        session = f"remember-{id(self)}"
        sent = run_handler({"text": "remember: hola"}, session=session, config={"max_turns": 200})

        self.assertEqual(sent[0].payload, {"text": "remembered (user)", "final": True})
        self.assertEqual(store.load(main._DB, session), [(0, "user", "hola")])

    def test_explicit_role_is_stored(self):
        session = f"remember-role-{id(self)}"
        sent = run_handler({"text": "remember: assistant: sure"}, session=session,
                            config={"max_turns": 200})

        self.assertEqual(sent[0].payload["text"], "remembered (assistant)")
        self.assertEqual(store.load(main._DB, session), [(0, "assistant", "sure")])

    def test_empty_content_is_rejected_without_writing(self):
        session = f"remember-empty-{id(self)}"
        sent = run_handler({"text": "remember:   "}, session=session)

        self.assertIn("needs content", sent[0].payload["text"])
        self.assertEqual(store.load(main._DB, session), [])


class ClearCommand(unittest.TestCase):
    def test_wipes_the_session(self):
        session = f"clear-{id(self)}"
        run_handler({"text": "remember: hola"}, session=session, config={"max_turns": 200})

        sent = run_handler({"text": "clear"}, session=session)

        self.assertEqual(sent[0].payload, {"text": "memory cleared", "final": True})
        self.assertEqual(store.load(main._DB, session), [])

    def test_is_case_insensitive(self):
        session = f"clear-case-{id(self)}"
        sent = run_handler({"text": "CLEAR"}, session=session)
        self.assertEqual(sent[0].payload["text"], "memory cleared")


class RecallCommand(unittest.TestCase):
    def test_empty_session_recalls_empty_text(self):
        sent = run_handler({"text": "recall"}, session=f"recall-empty-{id(self)}")
        self.assertEqual(sent[0].payload, {"text": "", "final": True})

    def test_drop_oldest_never_calls_summarize(self):
        session = f"recall-drop-{id(self)}"
        run_handler({"text": "remember: turn 1"}, session=session,
                     config={"max_turns": 200, "max_context_tokens": 4096,
                              "trim_strategy": "drop-oldest"})

        called = []
        main._summarize = lambda turns: called.append(turns) or "should not be used"
        sent = run_handler({"text": "recall"}, session=session)

        self.assertEqual(sent[0].payload["text"], "user: turn 1")
        self.assertEqual(called, [])

    def test_summarize_strategy_prefixes_a_summary_of_dropped_turns(self):
        session = f"recall-summarize-{id(self)}"
        for i in range(5):
            run_handler({"text": f"remember: turn number {i} " + "x" * 20}, session=session,
                         config={"max_turns": 200})

        main._summarize = lambda turns: "the user discussed several numbered turns"
        sent = run_handler({"text": "recall"}, session=session,
                            config={"max_context_tokens": 10, "trim_strategy": "summarize"})

        text = sent[0].payload["text"]
        self.assertTrue(text.startswith("[summary of"))
        self.assertIn("the user discussed several numbered turns", text)
        self.assertIn("turn number 4", text)

    def test_summarize_failure_falls_back_to_drop_oldest(self):
        """_summarize returning '' (e.g. ChatBackend failed) must not surface
        an empty/broken summary — recall should degrade silently."""
        session = f"recall-fallback-{id(self)}"
        for i in range(5):
            run_handler({"text": f"remember: turn number {i} " + "x" * 20}, session=session,
                         config={"max_turns": 200})

        main._summarize = lambda turns: ""
        sent = run_handler({"text": "recall"}, session=session,
                            config={"max_context_tokens": 10, "trim_strategy": "summarize"})

        text = sent[0].payload["text"]
        self.assertFalse(text.startswith("[summary of"))
        self.assertIn("turn number 4", text)


class UnknownCommand(unittest.TestCase):
    def test_replies_with_a_help_message(self):
        sent = run_handler({"text": "what time is it"}, session=f"unknown-{id(self)}")
        self.assertIn("Comandos:", sent[0].payload["text"])

    def test_empty_text_emits_nothing(self):
        sent = run_handler({"text": "   "}, session=f"empty-{id(self)}")
        self.assertEqual(sent, [])

    def test_none_payload_emits_nothing(self):
        sent = run_handler(None, session=f"none-{id(self)}")
        self.assertEqual(sent, [])


if __name__ == "__main__":
    unittest.main()
