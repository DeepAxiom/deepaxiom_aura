"""ChatBackend over HTTP: token usage, the fallback for endpoints that refuse
to report it, a self-hosted inference server, and the priority and deadline a
caller can attach to a request. No network: urlopen is faked, except where the
thing under test is a socket wait — a deadline that has to cut a read short —
and those run against a server on loopback.

    cd sdk/python && PYTHONPATH=src python -m unittest test_llm -v
"""
import http.server
import io
import json
import os
import threading
import time
import types
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

from aura import Attestation
from aura.llm import ChatBackend, _lines_until, _socket_under


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


_PRIORITY_ENV = ("OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL", "AURA_INFERENCE_URL",
                 "AURA_INFERENCE_MODEL", "AURA_INFERENCE_EXTRA", "AURA_INFERENCE_PRIORITY",
                 "AURA_LLM_TIMEOUT")


def _backend_with(**env: str) -> ChatBackend:
    """A backend built from exactly these variables and nothing inherited."""
    with patch.dict(os.environ, env):
        for name in _PRIORITY_ENV:
            if name not in env:
                os.environ.pop(name, None)
        return ChatBackend()


def _body_sent(backend: ChatBackend, **kwargs) -> dict:
    seen = {}

    def fake_urlopen(req, timeout=None):
        seen["body"] = json.loads(req.data)
        return _Response(_sse(_delta("ok")))

    with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
        out = "".join(backend.stream_chat([{"role": "user", "content": "a"}], **kwargs))
    assert out == "ok"
    return seen["body"]


def _in_ms(ms: float) -> int:
    return int(time.time() * 1000 + ms)


class Priority(unittest.TestCase):
    URL = "http://127.0.0.1:8080/v1"

    def test_vllm_gets_it_negated_because_it_serves_the_lowest_first(self):
        backend = _backend_with(AURA_INFERENCE_URL=self.URL, AURA_INFERENCE_PRIORITY="vllm")
        self.assertEqual(_body_sent(backend, priority=7)["priority"], -7)
        self.assertEqual(_body_sent(backend, priority=-3)["priority"], 3)
        self.assertEqual(_body_sent(backend, priority=0)["priority"], 0)

    def test_sglang_gets_it_as_is(self):
        backend = _backend_with(AURA_INFERENCE_URL=self.URL, AURA_INFERENCE_PRIORITY="sglang")
        self.assertEqual(_body_sent(backend, priority=7)["priority"], 7)

    def test_without_the_variable_nothing_is_sent(self):
        backend = _backend_with(AURA_INFERENCE_URL=self.URL)
        self.assertNotIn("priority", _body_sent(backend, priority=7))

    def test_an_engine_this_code_does_not_know_gets_nothing(self):
        with self.assertLogs("aura.llm", level="WARNING"):
            backend = _backend_with(AURA_INFERENCE_URL=self.URL,
                                    AURA_INFERENCE_PRIORITY="llama-server")
        self.assertNotIn("priority", _body_sent(backend, priority=7))

    def test_no_priority_given_means_no_field_in_either_mode(self):
        for mode in ("vllm", "sglang"):
            backend = _backend_with(AURA_INFERENCE_URL=self.URL, AURA_INFERENCE_PRIORITY=mode)
            self.assertNotIn("priority", _body_sent(backend), mode)

    def test_a_hosted_api_never_sees_it(self):
        # A key wins over the server variable, and the mode goes with it.
        backend = _backend_with(OPENAI_API_KEY="sk-x", AURA_INFERENCE_URL=self.URL,
                                AURA_INFERENCE_PRIORITY="vllm")
        self.assertNotIn("priority", _body_sent(backend, priority=7))
        # And a key that arrives after construction still keeps it in.
        backend = _backend_with(AURA_INFERENCE_URL=self.URL, AURA_INFERENCE_PRIORITY="vllm")
        backend._api_key = "sk-late"
        self.assertNotIn("priority", _body_sent(backend, priority=7))

    def test_the_callers_priority_wins_over_one_in_the_extra_body(self):
        backend = _backend_with(AURA_INFERENCE_URL=self.URL, AURA_INFERENCE_PRIORITY="vllm",
                                AURA_INFERENCE_EXTRA='{"priority": 99, "cache_prompt": true}')
        self.assertEqual(_body_sent(backend, priority=5)["priority"], -5)
        self.assertEqual(_body_sent(backend)["priority"], 99)

    def test_without_either_argument_the_body_is_what_it_always_was(self):
        backend = _backend_with(AURA_INFERENCE_URL=self.URL, AURA_INFERENCE_PRIORITY="vllm")
        self.assertEqual(
            sorted(_body_sent(backend)),
            ["max_tokens", "messages", "model", "stream", "stream_options", "temperature"])


class DeadlineWithoutASocket(unittest.TestCase):
    """What can be shown with urlopen faked: what is sent, and when not."""

    def _hosted(self) -> ChatBackend:
        return _backend_with(OPENAI_API_KEY="sk-test")

    def test_a_deadline_already_passed_sends_nothing(self):
        backend = self._hosted()
        with patch("aura.llm.urllib.request.urlopen") as urlopen:
            out = list(backend.stream_chat([{"role": "user", "content": "a"}],
                                           deadline=_in_ms(-1)))
        self.assertEqual(out, [])
        urlopen.assert_not_called()

    def test_no_engine_is_sent_the_deadline(self):
        for backend in (self._hosted(),
                        _backend_with(AURA_INFERENCE_URL="http://127.0.0.1:8080/v1",
                                      AURA_INFERENCE_PRIORITY="vllm")):
            body = _body_sent(backend, deadline=_in_ms(60_000))
            self.assertNotIn("deadline", body)
            self.assertEqual(
                sorted(body),
                ["max_tokens", "messages", "model", "stream", "stream_options", "temperature"])

    def test_the_socket_wait_is_never_longer_than_the_time_left(self):
        backend = self._hosted()
        timeouts = []

        def fake_urlopen(req, timeout=None):
            timeouts.append(timeout)
            return _Response(_sse(_delta("ok")))

        with patch("aura.llm.urllib.request.urlopen", side_effect=fake_urlopen):
            list(backend.stream_chat([{"role": "user", "content": "a"}]))
            list(backend.stream_chat([{"role": "user", "content": "a"}],
                                     deadline=_in_ms(500)))
            list(backend.stream_chat([{"role": "user", "content": "a"}],
                                     deadline=_in_ms(3_600_000)))

        self.assertEqual(timeouts[0], 120.0, "no deadline: the stall timeout, as before")
        self.assertGreater(timeouts[1], 0)
        self.assertLessEqual(timeouts[1], 0.5)
        self.assertEqual(timeouts[2], 120.0, "a far deadline does not lengthen the stall timeout")

    def test_the_stream_stops_between_chunks_and_closes_the_connection(self):
        backend = self._hosted()

        class Slow(_Response):
            def __iter__(self):
                for line in self._lines:
                    time.sleep(0.02)
                    yield line

        response = Slow(_sse(*[_delta(str(i)) for i in range(200)]))
        with patch("aura.llm.urllib.request.urlopen", return_value=response):
            started = time.time()
            got = list(backend.stream_chat([{"role": "user", "content": "a"}],
                                           deadline=_in_ms(200)))
            elapsed = time.time() - started

        self.assertGreater(len(got), 0)
        self.assertLess(len(got), 200, "the reply was cut, not read to the end")
        self.assertLess(elapsed, 2.0)
        self.assertTrue(response.closed)

    def test_the_read_timeout_is_re_armed_before_every_line(self):
        class Sock:
            def __init__(self) -> None:
                self.timeouts: list[float] = []

            def settimeout(self, value: float) -> None:
                self.timeouts.append(value)

        sock = Sock()

        class Resp:
            fp = types.SimpleNamespace(raw=types.SimpleNamespace(_sock=sock))

            def __iter__(self):
                for _ in range(3):
                    time.sleep(0.05)
                    yield b"data: x\n"

        lines = list(_lines_until(Resp(), _in_ms(5_000), 120.0))

        self.assertEqual(len(lines), 3)
        # Three lines, and the read that found the end of the stream.
        self.assertEqual(len(sock.timeouts), 4)
        self.assertTrue(all(0 < t <= 5.0 for t in sock.timeouts), sock.timeouts)
        self.assertEqual(sock.timeouts, sorted(sock.timeouts, reverse=True),
                         "each wait is shorter than the last: it is the time left")
        self.assertLess(sock.timeouts[-1], sock.timeouts[0])

    def test_a_stop_still_works_alongside_a_deadline(self):
        backend = self._hosted()
        stop = threading.Event()
        response = _Response(_sse(*[_delta(str(i)) for i in range(50)]))
        with patch("aura.llm.urllib.request.urlopen", return_value=response):
            got = []
            for token in backend.stream_chat([{"role": "user", "content": "a"}],
                                             stop=stop, deadline=_in_ms(60_000)):
                got.append(token)
                if len(got) == 3:
                    stop.set()
        self.assertEqual(got, ["0", "1", "2"])
        self.assertTrue(response.closed)


class _SilentEngine(http.server.BaseHTTPRequestHandler):
    """An engine that goes quiet: after the headers and a few tokens, or
    before sending anything at all. It returns when the client hangs up,
    which is the only thing these tests need it to notice."""

    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass

    def do_POST(self):
        server = self.server
        length = int(self.headers.get("Content-Length") or 0)
        server.bodies.append(json.loads(self.rfile.read(length)))
        if server.answers:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            for text in server.tokens:
                self.wfile.write(b"data: " + json.dumps(_delta(text)).encode() + b"\n\n")
                self.wfile.flush()
        self.close_connection = True
        self.connection.settimeout(10)
        try:
            hung_up = self.rfile.read(1) == b""
        except TimeoutError:
            hung_up = False
        except OSError:
            hung_up = True
        if hung_up:
            server.hung_up.set()


class DeadlineOnASocket(unittest.TestCase):
    """The part a fake cannot show: a read that is blocked when the deadline
    passes. Timing margins are wide on purpose; what is asserted is that the
    stream ended near the deadline rather than at the 120 s stall timeout."""

    def _engine(self, *, answers: bool, tokens: tuple[str, ...] = ()):
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _SilentEngine)
        server.daemon_threads = True
        server.answers = answers
        server.tokens = tokens
        server.bodies = []
        server.hung_up = threading.Event()
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return server

    def _backend(self, server, **env: str) -> ChatBackend:
        url = f"http://127.0.0.1:{server.server_address[1]}/v1"
        return _backend_with(AURA_INFERENCE_URL=url, **env)

    def test_a_stalled_reply_ends_at_the_deadline_and_the_engine_is_released(self):
        server = self._engine(answers=True, tokens=("one", "two"))
        backend = self._backend(server)

        started = time.time()
        got = list(backend.stream_chat([{"role": "user", "content": "a"}],
                                       deadline=_in_ms(400)))
        elapsed = time.time() - started

        self.assertEqual(got, ["one", "two"])
        self.assertGreater(elapsed, 0.2, "it waited for the deadline, it did not bail early")
        self.assertLess(elapsed, 5.0, "it did not wait for the stall timeout")
        self.assertTrue(server.hung_up.wait(5), "the connection was closed, so the engine can stop")

    def test_an_engine_that_never_answers_is_abandoned_at_the_deadline(self):
        server = self._engine(answers=False)
        backend = self._backend(server)

        started = time.time()
        got = list(backend.stream_chat([{"role": "user", "content": "a"}],
                                       deadline=_in_ms(300)))
        elapsed = time.time() - started

        self.assertEqual(got, [], "an expired deadline ends the stream; it is not an error")
        self.assertLess(elapsed, 5.0)
        self.assertTrue(server.hung_up.wait(5))

    def test_the_socket_under_a_real_response_can_be_found(self):
        # _lines_until re-arms the read timeout through a layout CPython keeps
        # but does not promise. If a release moves it, this fails here instead
        # of the bound quietly loosening to the timeout the request was opened
        # with.
        server = self._engine(answers=True, tokens=("one",))
        url = f"http://127.0.0.1:{server.server_address[1]}/v1/chat/completions"
        request = urllib.request.Request(url, data=b"{}")
        with urllib.request.urlopen(request, timeout=5) as resp:
            sock = _socket_under(resp)
            self.assertIsNotNone(sock)
            sock.settimeout(0.2)
            self.assertEqual(json.loads(resp.readline()[len(b"data: "):]), _delta("one"))
            self.assertEqual(resp.readline(), b"\n")
            with self.assertRaises(TimeoutError):
                resp.readline()

    def test_a_stall_with_time_left_is_still_an_error(self):
        server = self._engine(answers=True, tokens=("one",))
        backend = self._backend(server, AURA_LLM_TIMEOUT="0.3")

        with self.assertRaises(TimeoutError):
            list(backend.stream_chat([{"role": "user", "content": "a"}],
                                     deadline=_in_ms(60_000)))

    def test_without_a_deadline_the_stall_timeout_behaves_as_before(self):
        server = self._engine(answers=True, tokens=("one",))
        backend = self._backend(server, AURA_LLM_TIMEOUT="0.3")

        with self.assertRaises(TimeoutError):
            list(backend.stream_chat([{"role": "user", "content": "a"}]))


class _SlowLlama:
    """Stands in for llama_cpp.Llama: a token every 20 ms."""

    n_tokens = 0

    def __init__(self) -> None:
        self.calls = 0
        self.closed = threading.Event()

    def create_chat_completion(self, **_kwargs):
        self.calls += 1

        def gen():
            try:
                for i in range(200):
                    time.sleep(0.02)
                    yield {"choices": [{"delta": {"content": str(i)}}]}
            finally:
                self.closed.set()
        return gen()


class DeadlineInProcess(unittest.TestCase):
    def _local(self, llama) -> ChatBackend:
        backend = _backend_with()
        backend._load_local = lambda: llama
        return backend

    def test_a_generation_is_cut_at_the_deadline_and_the_model_released(self):
        llama = _SlowLlama()
        backend = self._local(llama)

        started = time.time()
        got = list(backend.stream_chat([{"role": "user", "content": "a"}],
                                       deadline=_in_ms(200)))
        elapsed = time.time() - started

        self.assertGreater(len(got), 0)
        self.assertLess(len(got), 200)
        self.assertLess(elapsed, 3.0)
        self.assertTrue(llama.closed.is_set(), "the engine's generator was closed, not dropped")
        self.assertTrue(backend._local_gate.acquire(timeout=1))
        backend._local_gate.release()

    def test_a_reply_still_queued_at_its_deadline_gives_up_its_place(self):
        llama = _SlowLlama()
        backend = self._local(llama)
        backend._local_gate.acquire()  # someone else is generating
        self.addCleanup(backend._local_gate.release)

        started = time.time()
        got = list(backend.stream_chat([{"role": "user", "content": "a"}],
                                       deadline=_in_ms(150)))
        elapsed = time.time() - started

        self.assertEqual(got, [])
        self.assertLess(elapsed, 3.0)
        self.assertEqual(llama.calls, 0, "it never reached the model")

    def test_priority_is_accepted_and_ignored_there_is_no_queue_to_order(self):
        llama = _SlowLlama()
        backend = self._local(llama)
        stream = backend.stream_chat([{"role": "user", "content": "a"}], priority=9)
        self.assertEqual(next(stream), "0")
        stream.close()


if __name__ == "__main__":
    unittest.main()

