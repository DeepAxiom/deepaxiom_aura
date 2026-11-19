#!/usr/bin/env python3
"""mock_engine — a fake OpenAI-compatible streaming engine: sleep() behind a queue.

It exists for one reason: so `robot_bench.py` and `calibrate_tokens.py` can be
validated on a machine with no GPU and no engine. It is not a model of any
real engine's performance, and no number it produces says anything about one.

What it does model is the *shape* that matters to the bench:

- `--slots` generations run at once. Each one waits `--ttft-ms` and then emits
  `max_tokens` tokens at `--tokens-per-s`. A running generation never slows
  down because others are running: this is not a batching model.
- Everything beyond `--slots` waits in a queue. `--queue fcfs` serves it in
  arrival order. `--queue priority` serves the lowest `priority` value first,
  arrival order within a value.
- Either way the order applies to requests that are still WAITING. A request
  that holds a slot keeps it until it finishes or its client hangs up. Nothing
  is preempted. So with every slot taken by a long generation, a critical
  request waits for a slot in both modes; priority only decides who is next.
  That is the behaviour vLLM issue #40004 describes, and the point of the mock
  is that the bench's output shows it.
- A client that hangs up frees its slot, or leaves the queue, within about
  20 ms.
- With `--queue fcfs`, a non-zero `priority` is refused with a 400, which is
  what vLLM is documented to do without `--scheduling-policy priority`. That
  was not checked against a real vLLM from here.

Standard library only.

    python mock_engine.py --port 8000 --slots 2 --ttft-ms 50 --tokens-per-s 40
    python mock_engine.py --port 8000 --slots 2 --queue priority
"""
from __future__ import annotations

import argparse
import heapq
import http.server
import itertools
import json
import socket
import sys
import threading
import time
from collections import deque

# What /v1/models reports as `owned_by`. The bench and the calibration script
# look for it and label their output as mock output when they find it.
MOCK_OWNER = "aura-bench-mock"

_POLL_S = 0.02


class Scheduler:
    """`slots` holders at once; the rest wait in order. Non-preemptive."""

    def __init__(self, slots: int, policy: str) -> None:
        if slots < 1:
            raise ValueError("slots must be at least 1")
        if policy not in ("fcfs", "priority"):
            raise ValueError("queue policy must be fcfs or priority")
        self.policy = policy
        self._cond = threading.Condition()
        self._free = slots
        self._waiting: list[tuple[int, int]] = []
        self._seq = itertools.count()
        self._stats = {
            "slots": slots, "running": 0, "waiting": 0, "max_running": 0, "max_waiting": 0,
            "started": 0, "completed": 0, "cancelled_running": 0, "cancelled_waiting": 0,
        }

    def acquire(self, priority: int, gone) -> bool:
        """Wait for a slot. False if `gone()` became true first."""
        with self._cond:
            key = priority if self.policy == "priority" else 0
            ticket = (key, next(self._seq))
            heapq.heappush(self._waiting, ticket)
            self._stats["waiting"] = len(self._waiting)
            self._stats["max_waiting"] = max(self._stats["max_waiting"], len(self._waiting))
            while True:
                if self._free > 0 and self._waiting[0] == ticket:
                    heapq.heappop(self._waiting)
                    self._free -= 1
                    self._stats["waiting"] = len(self._waiting)
                    self._stats["running"] += 1
                    self._stats["started"] += 1
                    self._stats["max_running"] = max(
                        self._stats["max_running"], self._stats["running"])
                    # Another slot may be free for whoever is next.
                    self._cond.notify_all()
                    return True
                self._cond.wait(timeout=_POLL_S)
                if gone():
                    self._waiting.remove(ticket)
                    heapq.heapify(self._waiting)
                    self._stats["waiting"] = len(self._waiting)
                    self._stats["cancelled_waiting"] += 1
                    self._cond.notify_all()
                    return False

    def release(self, *, completed: bool) -> None:
        with self._cond:
            self._free += 1
            self._stats["running"] -= 1
            self._stats["completed" if completed else "cancelled_running"] += 1
            self._cond.notify_all()

    def stats(self) -> dict:
        with self._cond:
            return dict(self._stats)


class PrefixCache:
    """Remembers recent prompts and says how long a prefix a new one shares
    with any of them. A stand-in for an engine's prefix cache, so the usage
    block has a `cached_tokens` that behaves like one: zero for a new prompt,
    the whole prompt when it is sent again."""

    def __init__(self, capacity: int = 64) -> None:
        self._lock = threading.Lock()
        self._prompts: deque[list[str]] = deque(maxlen=capacity)

    def lookup_and_store(self, tokens: list[str]) -> int:
        with self._lock:
            best = 0
            for seen in self._prompts:
                shared = 0
                for a, b in zip(seen, tokens):
                    if a != b:
                        break
                    shared += 1
                best = max(best, shared)
            self._prompts.append(tokens)
            return best


def tokenize(messages: list) -> list[str]:
    """The mock's tokenizer: one token per whitespace-separated word, plus one
    per message for its role. Nothing like a real tokenizer; it only has to be
    deterministic and grow with the prompt."""
    tokens: list[str] = []
    for message in messages:
        if not isinstance(message, dict):
            continue
        tokens.append(f"<{message.get('role', '')}>")
        content = message.get("content")
        if isinstance(content, str):
            tokens.extend(content.split())
    return tokens


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "aura-bench-mock"
    # One small write per token: without this Nagle's algorithm batches them
    # and the client measures the kernel, not the mock.
    disable_nagle_algorithm = True

    def log_message(self, fmt, *args):  # noqa: A003 - http.server's name
        if self.server.engine.verbose:
            sys.stderr.write("mock_engine: " + (fmt % args) + "\n")

    # -- plumbing ---------------------------------------------------------

    def _json(self, status: int, obj: dict) -> None:
        data = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _refuse(self, message: str) -> None:
        self._json(400, {"error": {"message": message, "type": "BadRequestError", "code": 400}})

    def _gone(self) -> bool:
        """True once the client has hung up. The request body has been read in
        full, so the only thing left to arrive on this socket is its end."""
        sock = self.connection
        try:
            sock.settimeout(0)
            try:
                return sock.recv(1, socket.MSG_PEEK) == b""
            finally:
                sock.settimeout(None)
        except (BlockingIOError, InterruptedError):
            return False
        except OSError:
            return True

    def _sleep_until(self, due: float) -> bool:
        """Sleep until `due` (perf_counter). False if the client hung up."""
        while True:
            if self._gone():
                return False
            left = due - time.perf_counter()
            if left <= 0:
                return True
            time.sleep(min(left, _POLL_S))

    def _event(self, payload) -> None:
        text = payload if isinstance(payload, str) else json.dumps(payload)
        data = b"data: " + text.encode("utf-8") + b"\n\n"
        self.wfile.write(b"%x\r\n" % len(data) + data + b"\r\n")

    # -- routes -----------------------------------------------------------

    def do_GET(self):  # noqa: N802 - http.server's name
        engine = self.server.engine
        path = self.path.split("?", 1)[0].rstrip("/")
        if path == "/v1/models":
            self._json(200, {"object": "list", "data": [{
                "id": engine.model, "object": "model", "owned_by": MOCK_OWNER}]})
        elif path == "/version":
            self._json(200, {"version": "mock", "engine": MOCK_OWNER})
        elif path in ("", "/health"):
            self._json(200, {"status": "ok"})
        elif path == "/mock/stats":
            self._json(200, engine.scheduler.stats())
        else:
            self._json(404, {"error": {"message": f"no route {path}", "code": 404}})

    def do_POST(self):  # noqa: N802 - http.server's name
        engine = self.server.engine
        path = self.path.split("?", 1)[0].rstrip("/")
        try:
            length = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(length) or b"{}")
        except ValueError:
            self._refuse("request body is not valid JSON")
            return
        if path != "/v1/chat/completions":
            self._json(404, {"error": {"message": f"no route {path}", "code": 404}})
            return
        if not isinstance(body, dict) or not isinstance(body.get("messages"), list):
            self._refuse("`messages` must be a list")
            return
        if body.get("stream") is not True:
            self._refuse("this mock only streams: send \"stream\": true")
            return
        max_tokens = body.get("max_tokens", 16)
        if not isinstance(max_tokens, int) or isinstance(max_tokens, bool) or max_tokens < 1:
            self._refuse("`max_tokens` must be a positive integer")
            return
        priority = body.get("priority", 0)
        if not isinstance(priority, int) or isinstance(priority, bool):
            self._refuse("`priority` must be an integer")
            return
        if priority != 0 and engine.scheduler.policy != "priority":
            self._refuse(f"Got priority {priority} but priority scheduling is not enabled "
                         f"(mock started with --queue fcfs).")
            return
        options = body.get("stream_options")
        if options is not None and engine.reject_stream_options:
            self._refuse("unknown field `stream_options`")
            return
        want_usage = (engine.report_usage and isinstance(options, dict)
                      and options.get("include_usage") is True)

        self.close_connection = True
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.send_header("Connection", "close")
        self.end_headers()

        if not engine.scheduler.acquire(priority, self._gone):
            return
        completed = False
        try:
            completed = self._generate(engine, body, max_tokens, want_usage)
        except OSError:
            # The client hung up mid-write. That is a cancel, not an error.
            pass
        finally:
            engine.scheduler.release(completed=completed)

    def _generate(self, engine, body: dict, max_tokens: int, want_usage: bool) -> bool:
        granted = time.perf_counter()
        base = {"id": "chatcmpl-mock", "object": "chat.completion.chunk",
                "created": int(time.time()), "model": engine.model}
        # Real engines open with a chunk that carries the role and no text. A
        # client that counted it as the first token would report a time to
        # first token that is really the time to a slot.
        self._event({**base, "choices": [{
            "index": 0, "delta": {"role": "assistant", "content": ""},
            "finish_reason": None}]})

        prompt = tokenize(body["messages"])
        cached = engine.cache.lookup_and_store(prompt)
        fresh = len(prompt) - cached
        first_token_at = granted + (
            engine.ttft_ms + fresh * engine.ms_per_input_token
            + cached * engine.ms_per_cached_token) / 1000.0
        for i in range(max_tokens):
            if not self._sleep_until(first_token_at + i / engine.tokens_per_s):
                return False
            self._event({**base, "choices": [{
                "index": 0, "delta": {"content": f" w{i}"}, "finish_reason": None}]})
        self._event({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "length"}]})
        if want_usage:
            usage = {"prompt_tokens": len(prompt), "completion_tokens": max_tokens,
                     "total_tokens": len(prompt) + max_tokens}
            if engine.report_cached:
                usage["prompt_tokens_details"] = {"cached_tokens": cached}
            self._event({**base, "choices": [], "usage": usage})
        self._event("[DONE]")
        self.wfile.write(b"0\r\n\r\n")
        return True


class _Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    # The default backlog is 5. A load generator opens more than that at once,
    # and a dropped SYN shows up as a one-second latency that is nobody's.
    request_queue_size = 256


class MockEngine:
    """The mock as an object, so a test can run it in-process:

        with MockEngine(slots=2, queue="priority") as engine:
            ... engine.base_url ...
    """

    def __init__(
        self, *, slots: int = 1, ttft_ms: float = 50.0, tokens_per_s: float = 50.0,
        queue: str = "fcfs", model: str = "mock-model", host: str = "127.0.0.1",
        port: int = 0, report_usage: bool = True, report_cached: bool = True,
        reject_stream_options: bool = False, ms_per_input_token: float = 0.0,
        ms_per_cached_token: float = 0.0, verbose: bool = False,
    ) -> None:
        if tokens_per_s <= 0:
            raise ValueError("tokens_per_s must be positive")
        self.scheduler = Scheduler(slots, queue)
        self.cache = PrefixCache()
        self.ttft_ms = float(ttft_ms)
        self.tokens_per_s = float(tokens_per_s)
        self.model = model
        self.report_usage = report_usage
        self.report_cached = report_cached
        self.reject_stream_options = reject_stream_options
        self.ms_per_input_token = float(ms_per_input_token)
        self.ms_per_cached_token = float(ms_per_cached_token)
        self.verbose = verbose
        self._server = _Server((host, port), _Handler)
        self._server.engine = self
        self.host = host
        self.port = self._server.server_address[1]
        self._thread: threading.Thread | None = None

    @property
    def base_url(self) -> str:
        return f"http://{self.host}:{self.port}/v1"

    def stats(self) -> dict:
        return self.scheduler.stats()

    def start(self) -> "MockEngine":
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        if self._thread is not None:
            self._thread.join(timeout=5)

    def serve_forever(self) -> None:
        self._server.serve_forever()

    def __enter__(self) -> "MockEngine":
        return self.start()

    def __exit__(self, *_exc) -> None:
        self.stop()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="A fake OpenAI-compatible streaming engine for validating the bench. "
                    "Its numbers describe sleep(), not a model.")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--slots", type=int, default=1,
                        help="generations that run at once (default 1)")
    parser.add_argument("--ttft-ms", type=float, default=50.0,
                        help="delay between getting a slot and the first token (default 50)")
    parser.add_argument("--tokens-per-s", type=float, default=50.0,
                        help="rate of each running generation (default 50)")
    parser.add_argument("--queue", choices=("fcfs", "priority"), default="fcfs",
                        help="order of the WAITING requests; priority = lowest value first. "
                             "Neither preempts a running generation (default fcfs)")
    parser.add_argument("--model", default="mock-model")
    parser.add_argument("--ms-per-input-token", type=float, default=0.0,
                        help="extra delay before the first token per uncached prompt token")
    parser.add_argument("--ms-per-cached-token", type=float, default=0.0,
                        help="extra delay before the first token per cached prompt token")
    parser.add_argument("--no-usage", action="store_true",
                        help="never send a usage chunk, even when asked")
    parser.add_argument("--no-cached-tokens", action="store_true",
                        help="leave prompt_tokens_details out of the usage chunk")
    parser.add_argument("--reject-stream-options", action="store_true",
                        help="answer 400 to any request that carries stream_options")
    parser.add_argument("--verbose", action="store_true", help="log every request to stderr")
    args = parser.parse_args(argv)

    engine = MockEngine(
        slots=args.slots, ttft_ms=args.ttft_ms, tokens_per_s=args.tokens_per_s,
        queue=args.queue, model=args.model, host=args.host, port=args.port,
        report_usage=not args.no_usage, report_cached=not args.no_cached_tokens,
        reject_stream_options=args.reject_stream_options,
        ms_per_input_token=args.ms_per_input_token,
        ms_per_cached_token=args.ms_per_cached_token, verbose=args.verbose)
    sys.stderr.write(
        f"mock_engine: {engine.base_url}  slots={args.slots} queue={args.queue} "
        f"ttft={args.ttft_ms:g}ms rate={args.tokens_per_s:g}tok/s\n"
        f"mock_engine: this is sleep() behind a queue. Nothing measured against it "
        f"describes a model.\n")
    try:
        engine.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
