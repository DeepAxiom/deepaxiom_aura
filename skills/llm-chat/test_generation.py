"""Tests for llm-chat's generation-kwargs mapping, history trimming, and the
shared ChatBackend kwargs it depends on (sdk/python/src/aura/llm.py) — no
llama-cpp-python or network needed, everything is mocked.

    cd skills/llm-chat && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import json
import unittest
from unittest.mock import MagicMock, patch

from aura.llm import ChatBackend

import main


class GenerationKwargs(unittest.TestCase):
    def test_maps_config_to_stream_chat_kwargs(self):
        config = {
            "max_tokens": 512,
            "temperature": 0.3,
            "top_p": 0.9,
            "top_k": 20,
            "repeat_penalty": 1.1,
            "system_prompt": "irrelevant here",
            "context_window": 4096,
            "history_limit": 10,
        }

        self.assertEqual(main._generation_kwargs(config), {
            "max_tokens": 512,
            "temperature": 0.3,
            "top_p": 0.9,
            "top_k": 20,
            "repeat_penalty": 1.1,
        })


class _FakeContext:
    """Minimal stand-in for aura.Context — just what handle() touches."""

    def __init__(self, session: str, text: str) -> None:
        self.payload = {"text": text}
        self.session = session
        self.cancelled = False
        self.emitted: list[tuple[str, object]] = []

    async def status(self, port, state, detail=""):
        pass

    async def emit(self, port, payload, **_kwargs):
        self.emitted.append((port, payload))

    async def error(self, port, detail):
        self.emitted.append((port, {"error": detail}))


class HistoryLimit(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        main._history.clear()
        main._current.clear()

    async def test_history_trimmed_once_it_exceeds_history_limit(self):
        session = "session-history-limit"
        main.skill.config["history_limit"] = 4
        main._history[session] = [
            {"role": "system", "content": "sys"},
            {"role": "user", "content": "u1"},
            {"role": "assistant", "content": "a1"},
            {"role": "user", "content": "u2"},
        ]

        with patch.object(main.backend, "stream_chat", return_value=iter(["hi"])):
            await main.handle(_FakeContext(session, "u3"))

        messages = main._history[session]
        # 4 pre-seeded + user "u3" + assistant reply = 6 > history_limit (4),
        # so the oldest turn (indices 1:3) is dropped, back down to 4.
        self.assertEqual(len(messages), 4)
        self.assertEqual(messages[0]["role"], "system")

    async def test_history_left_alone_under_the_limit(self):
        session = "session-under-limit"
        main.skill.config["history_limit"] = 40
        main._history[session] = [
            {"role": "system", "content": "sys"},
        ]

        with patch.object(main.backend, "stream_chat", return_value=iter(["hi"])):
            await main.handle(_FakeContext(session, "u1"))

        messages = main._history[session]
        self.assertEqual(len(messages), 3)  # system + user + assistant


class ChatBackendLocalKwargs(unittest.TestCase):
    def test_only_forwards_kwargs_that_were_explicitly_set(self):
        backend = ChatBackend()
        fake_llm = MagicMock()
        fake_llm.create_chat_completion.return_value = iter([
            {"choices": [{"delta": {"content": "hi"}}]},
        ])
        backend._load_local = lambda: fake_llm

        out = list(backend._stream_local(
            [{"role": "user", "content": "hey"}], max_tokens=10, temperature=0.5,
            top_p=0.9, top_k=None, repeat_penalty=1.2,
        ))

        self.assertEqual(out, ["hi"])
        _, kwargs = fake_llm.create_chat_completion.call_args
        self.assertEqual(kwargs["top_p"], 0.9)
        self.assertNotIn("top_k", kwargs)
        self.assertEqual(kwargs["repeat_penalty"], 1.2)

    def test_no_new_kwargs_forwarded_when_all_none_matches_prior_behavior(self):
        backend = ChatBackend()
        fake_llm = MagicMock()
        fake_llm.create_chat_completion.return_value = iter([])
        backend._load_local = lambda: fake_llm

        list(backend._stream_local(
            [{"role": "user", "content": "hey"}], max_tokens=10, temperature=0.5,
            top_p=None, top_k=None, repeat_penalty=None,
        ))

        _, kwargs = fake_llm.create_chat_completion.call_args
        self.assertNotIn("top_p", kwargs)
        self.assertNotIn("top_k", kwargs)
        self.assertNotIn("repeat_penalty", kwargs)


class _FakeCloudResponse:
    def __init__(self, lines: list[bytes]) -> None:
        self._lines = lines

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def __iter__(self):
        return iter(self._lines)


class ChatBackendCloudKwargs(unittest.TestCase):
    def test_forwards_top_p_and_silently_drops_unsupported_ones(self):
        backend = ChatBackend()
        backend._api_key = "sk-test"
        captured = {}

        def fake_urlopen(req):
            captured["body"] = json.loads(req.data)
            return _FakeCloudResponse([
                b'data: {"choices": [{"delta": {"content": "hi"}}]}\n',
                b"data: [DONE]\n",
            ])

        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen), \
                self.assertLogs("aura.llm", level="DEBUG"):
            out = list(backend._stream_cloud(
                [{"role": "user", "content": "hey"}], max_tokens=10, temperature=0.5,
                top_p=0.9, top_k=40, repeat_penalty=1.1,
            ))

        self.assertEqual(out, ["hi"])
        self.assertEqual(captured["body"]["top_p"], 0.9)
        self.assertNotIn("top_k", captured["body"])
        self.assertNotIn("repeat_penalty", captured["body"])

    def test_no_top_p_key_when_not_set_matches_prior_behavior(self):
        backend = ChatBackend()
        backend._api_key = "sk-test"
        captured = {}

        def fake_urlopen(req):
            captured["body"] = json.loads(req.data)
            return _FakeCloudResponse([b"data: [DONE]\n"])

        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
            list(backend._stream_cloud(
                [{"role": "user", "content": "hey"}], max_tokens=10, temperature=0.5,
                top_p=None, top_k=None, repeat_penalty=None,
            ))

        self.assertNotIn("top_p", captured["body"])


if __name__ == "__main__":
    unittest.main()
