"""ChatBackend over HTTP: token usage, the fallback for endpoints that refuse
to report it, and a self-hosted inference server. No network — urlopen is
faked.

    cd sdk/python && PYTHONPATH=src python -m unittest test_llm -v
"""
import io
import json
import os
import threading
import unittest
import urllib.error
from unittest.mock import patch

from aura import Attestation
from aura.llm import ChatBackend


class _Response:
    def __init__(self, lines: list[bytes]) -> None:
        self._lines = lines
        self.closed = False

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        self.closed = True
        return False

    def __iter__(self):
        return iter(self._lines)


def _sse(*events: dict) -> list[bytes]:
    lines = [b"data: " + json.dumps(e).encode() + b"\n" for e in events]
    return lines + [b"data: [DONE]\n"]


def _delta(text: str) -> dict:
    return {"choices": [{"delta": {"content": text}}]}


def _clean_env():
    return patch.dict(os.environ, {}, clear=False)


class CloudUsage(unittest.TestCase):
    def setUp(self):
        for name in ("OPENAI_API_KEY", "AURA_INFERENCE_URL", "AURA_INFERENCE_MODEL",
                     "AURA_INFERENCE_EXTRA", "OPENAI_BASE_URL", "OPENAI_MODEL"):
            patcher = patch.dict(os.environ)
            patcher.start()
            os.environ.pop(name, None)
            self.addCleanup(patcher.stop)

    def _cloud(self) -> ChatBackend:
        backend = ChatBackend()
        backend._api_key = "sk-test"
        return backend

    def test_usage_is_requested_and_read_including_cache_hits(self):
        backend = self._cloud()
        seen = {}

        def fake_urlopen(req, timeout=None):
            seen["body"] = json.loads(req.data)
            seen["timeout"] = timeout
            seen["auth"] = req.get_header("Authorization")
            return _Response(_sse(
                _delta("he"), _delta("llo"),
                {"choices": [], "usage": {
                    "prompt_tokens": 900, "completion_tokens": 2,
                    "prompt_tokens_details": {"cached_tokens": 768}}},
            ))

        usage: dict = {}
        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
            out = "".join(backend.stream_chat(
                [{"role": "user", "content": "hi"}], usage=usage))

        self.assertEqual(out, "hello")
        self.assertEqual(seen["body"]["stream_options"], {"include_usage": True})
        self.assertEqual(seen["auth"], "Bearer sk-test")
        self.assertGreater(seen["timeout"], 0, "a remote read must be able to time out")
        self.assertEqual(usage, {"input_tokens": 900, "output_tokens": 2,
                                 "cached_input_tokens": 768})

    def test_an_endpoint_that_refuses_the_field_still_streams(self):
        backend = self._cloud()
        bodies = []

        def fake_urlopen(req, timeout=None):
            body = json.loads(req.data)
            bodies.append(body)
            if "stream_options" in body:
                raise urllib.error.HTTPError(req.full_url, 400, "Bad Request", {}, io.BytesIO(b""))
            return _Response(_sse(_delta("ok")))

        usage: dict = {}
        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
            first = "".join(backend.stream_chat([{"role": "user", "content": "a"}], usage=usage))
            second = "".join(backend.stream_chat([{"role": "user", "content": "b"}], usage=usage))

        self.assertEqual((first, second), ("ok", "ok"))
        self.assertEqual(usage, {}, "no usage reported means none recorded, not a guess")
        # Refused once, then never asked again: three requests, not four.
        self.assertEqual(["stream_options" in b for b in bodies], [True, False, False])

    def test_other_http_errors_are_not_swallowed(self):
        backend = self._cloud()

        def fake_urlopen(req, timeout=None):
            raise urllib.error.HTTPError(req.full_url, 401, "Unauthorized", {}, io.BytesIO(b""))

        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
            with self.assertRaises(urllib.error.HTTPError):
                list(backend.stream_chat([{"role": "user", "content": "a"}]))

    def test_stop_closes_the_connection_mid_stream(self):
        backend = self._cloud()
        stop = threading.Event()
        response = _Response(_sse(*[_delta(str(i)) for i in range(50)]))

        with patch("aura.llm.urllib.request.urlopen", return_value=response):
            got = []
            for token in backend.stream_chat([{"role": "user", "content": "a"}], stop=stop):
                got.append(token)
                if len(got) == 3:
                    stop.set()

        self.assertEqual(got, ["0", "1", "2"])
        self.assertTrue(response.closed)


class SelfHostedServer(unittest.TestCase):
    def _backend(self, **env: str) -> ChatBackend:
        base = {"AURA_INFERENCE_URL": "http://127.0.0.1:8080/v1"}
        base.update(env)
        with patch.dict(os.environ, base):
            os.environ.pop("OPENAI_API_KEY", None)
            return ChatBackend()

    def test_it_is_a_keyless_openai_compatible_client(self):
        backend = self._backend(AURA_INFERENCE_MODEL="qwen3-4b",
                                AURA_INFERENCE_EXTRA='{"cache_prompt": true}')
        seen = {}

        def fake_urlopen(req, timeout=None):
            seen["url"] = req.full_url
            seen["auth"] = req.get_header("Authorization")
            seen["body"] = json.loads(req.data)
            return _Response(_sse(_delta("ok")))

        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
            out = "".join(backend.stream_chat([{"role": "user", "content": "a"}]))

        self.assertEqual(out, "ok")
        self.assertTrue(backend.is_cloud)
        self.assertEqual(seen["url"], "http://127.0.0.1:8080/v1/chat/completions")
        self.assertIsNone(seen["auth"])
        self.assertEqual(seen["body"]["model"], "qwen3-4b")
        self.assertIs(seen["body"]["cache_prompt"], True)

    def test_a_hosted_key_wins_over_the_server_variable(self):
        with patch.dict(os.environ, {"OPENAI_API_KEY": "sk-x",
                                     "AURA_INFERENCE_URL": "http://127.0.0.1:8080/v1",
                                     "AURA_INFERENCE_EXTRA": '{"cache_prompt": true}'}):
            os.environ.pop("OPENAI_BASE_URL", None)
            backend = ChatBackend()
        self.assertEqual(backend._base_url, "https://api.openai.com/v1")
        self.assertEqual(backend._extra_body, {}, "server knobs must never reach a hosted API")

    def test_malformed_extra_is_ignored_not_fatal(self):
        backend = self._backend(AURA_INFERENCE_EXTRA="{not json")
        self.assertEqual(backend._extra_body, {})

    def test_the_attestation_names_the_endpoint(self):
        backend = self._backend(AURA_INFERENCE_MODEL="qwen3-4b")
        att = backend.attestation(temperature=0.2, usage={"input_tokens": 5, "output_tokens": 1})
        wire = att.to_wire()
        self.assertEqual(wire["engine"], "openai-compatible")
        self.assertEqual(wire["engine_version"], "http://127.0.0.1:8080/v1")
        self.assertEqual(wire["usage"], {"input_tokens": 5, "output_tokens": 1})


class UsageOnTheWire(unittest.TestCase):
    def test_absent_usage_leaves_the_record_byte_identical_to_before(self):
        wire = Attestation(engine="llama.cpp", model="m").to_wire()
        self.assertEqual(wire, {"engine": "llama.cpp", "model": "m"})

    def test_only_the_three_known_counters_travel(self):
        wire = Attestation(engine="e", model="m", usage={
            "input_tokens": 10, "output_tokens": 3, "cached_input_tokens": 8,
            "prompt": "never this"}).to_wire()
        self.assertEqual(wire["usage"],
                         {"input_tokens": 10, "output_tokens": 3, "cached_input_tokens": 8})

    def test_a_nonsense_count_is_refused_at_the_call_site(self):
        for bad in (-1, 1.5, "7", True):
            with self.assertRaises(ValueError):
                Attestation(engine="e", model="m", usage={"input_tokens": bad}).to_wire()


if __name__ == "__main__":
    unittest.main()
