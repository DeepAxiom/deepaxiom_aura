#!/usr/bin/env python3
"""robot_bench — a robot-shaped load generator for an OpenAI-compatible engine.

A robot does not load an inference engine the way a chat product does. It has
a short request that must come back on time, sent at a fixed rate whatever
else is going on, and it has long generations — a plan, a report, a summary —
that nobody is waiting on. The question that matters is what the second kind
does to the first. Mean throughput does not answer it.

So this sends two classes at one `/v1/chat/completions` streaming endpoint:

  critical    one short request every --critical-period-ms, each with an
              absolute deadline --critical-deadline-ms after it was sent.
              Open loop: the next one is sent on schedule whether or not the
              previous one came back, because a control loop does not wait.
  background  --background-concurrency long generations kept running
              back to back.

in two phases, back to back in one run:

  baseline    critical alone
  loaded      critical with the background running

and reports, per class and per phase: time to first token, end-to-end
latency, output tokens per second, deadline misses, and what the engine says
it counted. The number to look at is critical time to first token, loaded
against baseline.

Standard library only: it has to run on a Jetson with nothing installed.

    python robot_bench.py --base-url http://127.0.0.1:8080/v1 \
        --duration-s 60 --background-concurrency 8 --out run.json

Priority (--priority-mode)
--------------------------
  none    no `priority` field is sent. Use this against llama-server, which
          has no per-request priority, and against a vLLM on its default
          first-come-first-served policy.
  vllm    integer `priority`, LOWER is served earlier: critical 0,
          background 10. vLLM honours it only when it was started with
          `--scheduling-policy priority`. Without that flag it answers a
          non-zero priority with an error, so every background request fails.
          The bench probes for this before the run and stops with the
          engine's own message rather than measuring a broken run.
  sglang  integer `priority`, HIGHER is served earlier: critical 10,
          background 0.

How things are measured
-----------------------
- Time to first token (TTFT): from just before the TCP connect to the first
  stream chunk that carries text (`content`, `reasoning_content`, `reasoning`
  or a tool call). The chunk engines open with, a role and no text, does not
  count. One connection per request, so connect time is inside the number.
- End to end: from the same instant to `[DONE]`. Completed requests only.
- Percentiles are nearest-rank: the p-th percentile of n samples is the
  ceil(p/100 * n)-th smallest. It is always a value that was observed, never
  an interpolation between two, and with fewer than 100 samples p99 is the
  maximum. The sample count is printed beside every percentile.
- A class with no samples prints "n/a". It never prints a zero.
- Deadline misses are counted over the requests that were sent: one that
  failed, or was cancelled, did not produce a token in time either.
- With --cancel-at-deadline a request with no first token by its deadline is
  cut off, so it has no TTFT at all. The TTFT percentiles then describe only
  the requests that made it, which is why the count is beside them.
- Output tokens are the engine's `completion_tokens` when it reports usage,
  and the number of text chunks when it does not; the output says which.

What it does not measure: accuracy, anything about the model's answers, GPU
utilisation, memory, or the engine's own view of its queue. The clock is the
client's.
"""
from __future__ import annotations

import argparse
import datetime
import http.client
import itertools
import json
import math
import os
import platform
import random
import socket
import sys
import threading
import time
import urllib.parse
from collections import Counter

SCHEMA = 1

PERCENTILE_METHOD = (
    "nearest-rank: the p-th percentile of n samples is the ceil(p/100*n)-th smallest "
    "observed value; no interpolation; with n < 100, p99 is the maximum")

# `owned_by` of the bundled mock's model. Output produced against it is
# labelled, loudly, so a table from the mock can never pass for a measurement.
MOCK_OWNER = "aura-bench-mock"

# critical, background — for each way an engine spells a priority.
PRIORITIES = {
    "none": (None, None),
    "vllm": (0, 10),      # lower is served earlier
    "sglang": (10, 0),    # higher is served earlier
}

DEFAULT_CRITICAL_SYSTEM = (
    "You are the reflex layer of a mobile robot. Reply with exactly one word: "
    "STOP, SLOW or GO.")
DEFAULT_BACKGROUND_SYSTEM = (
    "You are the planning and reporting layer of a mobile robot. Write at length, "
    "in full sentences, and do not stop early.")
DEFAULT_BACKGROUND_USER = (
    "Write a detailed shift report for a warehouse robot that moved 212 totes between "
    "aisles 4 and 17. Cover route choices, battery use, every delay and its cause, and "
    "recommendations for the next shift.",
    "Explain step by step how a differential-drive robot should plan and execute a "
    "docking manoeuvre onto a charging station, including sensing, alignment, the ways "
    "it can fail and how to recover from each.",
    "Describe a full inspection round of a pump room for a legged robot: the order of "
    "the checkpoints, what to look and listen for at each one, and what to do when a "
    "reading is out of range.",
    "Write the maintenance history of a six-axis arm over one year of operation, with "
    "every intervention, the symptom that led to it, the part replaced and the "
    "downtime it caused.",
    "Summarise, as a long narrative, a day in which a delivery robot crossed a campus "
    "forty times: the weather, the pedestrians, the three blocked paths and how each "
    "detour was chosen.",
    "Draft a complete operating procedure for a robot that restocks shelves at night: "
    "start-up checks, the picking loop, handling of damaged goods, and shutdown.",
    "Compare, at length, three strategies for a mobile manipulator to clear a cluttered "
    "table, with the order of grasps, the risks of each and the expected time.",
    "Write an incident analysis for a robot that stopped in a doorway for nine minutes: "
    "the timeline, each hypothesis considered, the evidence for and against, and the "
    "corrective actions.",
)


def log(message: str) -> None:
    sys.stderr.write(f"robot_bench: {message}\n")
    sys.stderr.flush()


# --------------------------------------------------------------------------
# Small pure pieces: percentiles and the event-stream parser.
# --------------------------------------------------------------------------

def percentile(values, p: float) -> float | None:
    """Nearest-rank percentile, or None for an empty sample.

    The p-th percentile is the smallest observed value with at least p % of
    the sample at or below it: the ceil(p/100 * n)-th smallest. For the small
    samples a short run produces this is the honest choice. It never returns
    a value that was not measured, and it does not pretend ten samples know
    where their 99th percentile is: it gives the maximum.
    """
    if not 0 < p <= 100:
        raise ValueError("percentile must be in (0, 100]")
    ordered = sorted(values)
    if not ordered:
        return None
    # The epsilon is for p * n / 100 landing a hair above an integer in
    # floating point (99.9 * 1000 / 100), which would skip a rank.
    rank = math.ceil(p * len(ordered) / 100.0 - 1e-9)
    return ordered[max(rank, 1) - 1]


class SSEParser:
    """Incremental `text/event-stream` parser: bytes in, `data` payloads out.

    Handles what a server may legally do and a naive `line.startswith("data: ")`
    does not: bytes that arrive split anywhere (inside a line, inside a
    multi-byte character, between a CR and its LF), all three line endings,
    `data:` with and without the space, an event whose data spans several
    lines, comment lines (`: keep-alive`), and fields other than `data`.

    One leniency, on purpose: `close()` dispatches an event the server did
    not terminate with a blank line. The standard says to drop it. An engine
    that forgets the final blank line after `[DONE]` should not cost the run
    its last event.
    """

    def __init__(self) -> None:
        self._buffer = b""
        self._data: list[str] = []
        self._first_line = True

    def feed(self, chunk: bytes) -> list[str]:
        self._buffer += chunk
        return self._drain(final=False)

    def close(self) -> list[str]:
        out = self._drain(final=True)
        if self._data:
            out.append("\n".join(self._data))
            self._data = []
        return out

    def _drain(self, *, final: bool) -> list[str]:
        out: list[str] = []
        buf = self._buffer
        start = 0
        while True:
            lf = buf.find(b"\n", start)
            cr = buf.find(b"\r", start)
            if lf < 0 and cr < 0:
                break
            end = min(i for i in (lf, cr) if i >= 0)
            after = end + 1
            if buf[end] == 0x0D:
                if after == len(buf) and not final:
                    # A CR at the very end may be half of a CRLF. Wait.
                    break
                if after < len(buf) and buf[after] == 0x0A:
                    after += 1
            self._line(buf[start:end], out)
            start = after
        rest = buf[start:]
        if final and rest:
            self._line(rest, out)
            rest = b""
        self._buffer = rest
        return out

    def _line(self, raw: bytes, out: list[str]) -> None:
        line = raw.decode("utf-8", errors="replace")
        if self._first_line:
            self._first_line = False
            if line.startswith("\ufeff"):
                line = line[1:]
        if not line:
            if self._data:
                out.append("\n".join(self._data))
                self._data = []
            return
        if line.startswith(":"):
            return
        field, _, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if field == "data":
            self._data.append(value)


# --------------------------------------------------------------------------
# The endpoint and one request against it.
# --------------------------------------------------------------------------

class Endpoint:
    """An OpenAI-compatible base URL, e.g. http://127.0.0.1:8080/v1."""

    def __init__(self, base_url: str, api_key: str | None = None) -> None:
        parts = urllib.parse.urlsplit(base_url)
        if parts.scheme not in ("http", "https") or not parts.hostname:
            raise ValueError(f"--base-url must be http(s)://host[:port][/v1], got {base_url!r}")
        self.base_url = base_url.rstrip("/")
        self.scheme = parts.scheme
        self.host = parts.hostname
        self.port = parts.port or (443 if parts.scheme == "https" else 80)
        self.prefix = parts.path.rstrip("/")
        # Engines put their own endpoints (/version, /props, /metrics) at the
        # server root, beside /v1 rather than under it.
        self.root = self.prefix[:-3] if self.prefix.endswith("/v1") else self.prefix
        self.api_key = api_key

    def connection(self, timeout: float) -> http.client.HTTPConnection:
        cls = (http.client.HTTPSConnection if self.scheme == "https"
               else http.client.HTTPConnection)
        return cls(self.host, self.port, timeout=timeout)

    def headers(self) -> dict:
        headers = {"Content-Type": "application/json", "Accept": "text/event-stream",
                   "Connection": "close"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        return headers

    def get_json(self, path: str, timeout: float = 5.0):
        """GET a JSON document, or None for any failure at all. Used only to
        describe the engine; nothing in a run depends on it."""
        conn = self.connection(timeout)
        try:
            headers = {"Accept": "application/json", "Connection": "close"}
            if self.api_key:
                headers["Authorization"] = f"Bearer {self.api_key}"
            conn.request("GET", path, headers=headers)
            resp = conn.getresponse()
            if resp.status != 200:
                return None
            return json.loads(resp.read(1 << 20))
        except (OSError, http.client.HTTPException, ValueError):
            return None
        finally:
            conn.close()


class UsageSupport:
    """Whether the engine accepts `stream_options`.

    Assumed until a request is refused with a 400 and the same request then
    succeeds without the field. Only that pair proves the field was the
    problem; a 400 that survives the retry was about something else, and
    forgetting usage because of it would throw away a number for nothing.
    """

    def __init__(self) -> None:
        self.supported = True


class Inflight:
    """The open connections of one class, so the end of a phase can hang up
    on them. Shutting the socket down is what unblocks a thread parked in a
    read, and it is also the cancel an engine understands."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._socks: set = set()
        self.aborting = False

    def add(self, sock) -> None:
        with self._lock:
            self._socks.add(sock)

    def discard(self, sock) -> None:
        with self._lock:
            self._socks.discard(sock)

    def abort(self) -> None:
        with self._lock:
            self.aborting = True
            socks = list(self._socks)
        for sock in socks:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


class _DeadlineReached(Exception):
    pass


def _int_or_none(value) -> int | None:
    if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
        return value
    return None


def usage_of(raw) -> dict | None:
    """The three counts of an OpenAI-shaped `usage` object. `cached_tokens`
    is None when the engine did not state it, which is not the same as 0."""
    if not isinstance(raw, dict):
        return None
    details = raw.get("prompt_tokens_details")
    return {
        "prompt_tokens": _int_or_none(raw.get("prompt_tokens")),
        "completion_tokens": _int_or_none(raw.get("completion_tokens")),
        "cached_tokens": (_int_or_none(details.get("cached_tokens"))
                          if isinstance(details, dict) else None),
    }


def _text_field(delta: dict) -> str | None:
    """Which field of a delta carries output, if any."""
    for name in ("content", "reasoning_content", "reasoning"):
        value = delta.get(name)
        if isinstance(value, str) and value:
            return name
    if delta.get("tool_calls"):
        return "tool_calls"
    return None


def _error_text(status: int, raw: bytes) -> str:
    text = raw.decode("utf-8", errors="replace").strip()
    try:
        parsed = json.loads(text)
        err = parsed.get("error", parsed) if isinstance(parsed, dict) else parsed
        if isinstance(err, dict):
            text = str(err.get("message") or err.get("detail") or err)
        elif isinstance(err, str):
            text = err
    except ValueError:
        pass
    return f"HTTP {status}: {text[:300]}"


def stream_request(
    endpoint: Endpoint, body: dict, *, klass: str, phase: str = "", req_id: int = 0,
    clock0: float = 0.0, deadline_ms: float | None = None, cancel_at_deadline: bool = False,
    stall_timeout_s: float = 120.0, usage_support: UsageSupport | None = None,
    inflight: Inflight | None = None,
) -> dict:
    """Send one streaming chat completion and time it. Never raises: whatever
    happened is in the record's `outcome` and `error`.

    outcome is one of
      completed              the stream reached [DONE] (or a finish_reason and
                             then the end of the connection)
      failed                 an HTTP error, a connection error, a stall, or a
                             stream that ended with neither
      cancelled_at_deadline  cut off by this client at the deadline
      aborted                cut off by this client because the phase ended
    """
    usage_support = usage_support or UsageSupport()
    t_send = time.perf_counter()
    deadline_at = t_send + deadline_ms / 1000.0 if deadline_ms is not None else None
    rec = {
        "id": req_id, "class": klass, "phase": phase, "outcome": "failed",
        "sent_unix_ms": int(time.time() * 1000), "t_send_s": round(t_send - clock0, 6),
        "t_end_s": None, "http_status": None, "error": None,
        "ttft_ms": None, "e2e_ms": None, "gen_ms": None, "first_token_field": None,
        "chunks": 0, "chars": 0, "finish_reason": None, "usage": None,
        "engine_timings": None, "output_tokens": 0, "output_tokens_source": "chunks",
        "priority": body.get("priority"), "max_tokens": body.get("max_tokens"),
        "deadline_ms": deadline_ms, "first_token_late": None, "completion_late": None,
        "retried_without_stream_options": False,
    }
    t_first = t_last = None

    def wait_budget() -> float:
        """How long the next socket wait may last."""
        if cancel_at_deadline and deadline_at is not None:
            left = deadline_at - time.perf_counter()
            if left <= 0:
                raise _DeadlineReached
            return min(stall_timeout_s, left)
        return stall_timeout_s

    def handle(event, now: float) -> None:
        nonlocal t_first, t_last
        if not isinstance(event, dict):
            return
        if event.get("error"):
            rec["error"] = f"error in stream: {str(event['error'])[:300]}"
        usage = usage_of(event.get("usage"))
        if usage is not None:
            rec["usage"] = usage
        if isinstance(event.get("timings"), dict):
            rec["engine_timings"] = event["timings"]
        choices = event.get("choices")
        if not isinstance(choices, list) or not choices or not isinstance(choices[0], dict):
            return
        delta = choices[0].get("delta")
        if isinstance(delta, dict):
            field = _text_field(delta)
            if field is not None:
                if t_first is None:
                    t_first = now
                    rec["first_token_field"] = field
                t_last = now
                rec["chunks"] += 1
                value = delta.get(field)
                if isinstance(value, str):
                    rec["chars"] += len(value)
        if choices[0].get("finish_reason") is not None:
            rec["finish_reason"] = choices[0]["finish_reason"]

    path = endpoint.prefix + "/chat/completions"
    done = False
    for attempt in (1, 2):
        ask_usage = usage_support.supported and attempt == 1
        payload = dict(body)
        payload["stream"] = True
        # Whatever --extra-body put in `stream_options` is kept beside the
        # request for usage, and goes with it when the engine refuses the field.
        options = payload.pop("stream_options", None)
        if ask_usage:
            payload["stream_options"] = {
                **(options if isinstance(options, dict) else {}), "include_usage": True}
        conn = sock = resp = None
        try:
            conn = endpoint.connection(wait_budget())
            conn.connect()
            # Kept by hand: http.client drops its own reference to the socket
            # as soon as the response says `Connection: close`, and the read
            # timeout has to be re-armed on it for as long as the stream runs.
            sock = conn.sock
            if inflight is not None:
                inflight.add(sock)
                if inflight.aborting:
                    # The phase ended while this was connecting, before
                    # abort() had a socket to shut down. It stops here.
                    rec["outcome"] = "aborted"
                    break
            conn.request("POST", path, body=json.dumps(payload).encode("utf-8"),
                         headers=endpoint.headers())
            sock.settimeout(wait_budget())
            resp = conn.getresponse()
            rec["http_status"] = resp.status
            if resp.status != 200:
                raw = resp.read(8192)
                if resp.status == 400 and ask_usage:
                    # Perhaps the engine does not know `stream_options`. One
                    # retry without it settles which.
                    rec["retried_without_stream_options"] = True
                    rec["error"] = _error_text(resp.status, raw)
                    continue
                rec["error"] = _error_text(resp.status, raw)
                break
            if attempt == 2:
                usage_support.supported = False
                rec["error"] = None
            parser = SSEParser()
            while not done:
                sock.settimeout(wait_budget())
                line = resp.readline()
                events = parser.feed(line) if line else parser.close()
                now = time.perf_counter()
                for data in events:
                    if data.strip() == "[DONE]":
                        done = True
                        break
                    try:
                        handle(json.loads(data), now)
                    except ValueError:
                        continue
                if not line:
                    break
            t_end = time.perf_counter()
            if rec["error"]:
                pass
            elif done or rec["finish_reason"] is not None:
                rec["outcome"] = "completed"
                rec["e2e_ms"] = round((t_end - t_send) * 1000.0, 3)
            elif inflight is not None and inflight.aborting:
                rec["outcome"] = "aborted"
            else:
                rec["error"] = "stream ended with neither a finish_reason nor [DONE]"
            break
        except _DeadlineReached:
            rec["outcome"] = "cancelled_at_deadline"
            break
        except (TimeoutError, socket.timeout):
            # The same class since Python 3.10; two different ones before it.
            if (cancel_at_deadline and deadline_at is not None
                    and time.perf_counter() >= deadline_at - 0.010):
                rec["outcome"] = "cancelled_at_deadline"
            else:
                rec["error"] = f"stalled: nothing received for {stall_timeout_s:g} s"
            break
        except (OSError, http.client.HTTPException) as exc:
            if inflight is not None and inflight.aborting:
                rec["outcome"] = "aborted"
            else:
                rec["error"] = f"{type(exc).__name__}: {exc}"
            break
        finally:
            if inflight is not None and sock is not None:
                inflight.discard(sock)
            # Closing is the cancel: an engine that sees its client go stops
            # generating for it. The response holds the socket once the
            # connection has let go of it, so both are closed.
            if resp is not None:
                resp.close()
            if conn is not None:
                conn.close()

    t_end = time.perf_counter()
    rec["t_end_s"] = round(t_end - clock0, 6)
    if t_first is not None:
        rec["ttft_ms"] = round((t_first - t_send) * 1000.0, 3)
        rec["gen_ms"] = round((t_last - t_first) * 1000.0, 3)
    usage = rec["usage"]
    if usage is not None and usage["completion_tokens"] is not None:
        rec["output_tokens"] = usage["completion_tokens"]
        rec["output_tokens_source"] = "engine"
    else:
        rec["output_tokens"] = rec["chunks"]
    if deadline_ms is not None:
        rec["first_token_late"] = rec["ttft_ms"] is None or rec["ttft_ms"] > deadline_ms
        rec["completion_late"] = rec["e2e_ms"] is None or rec["e2e_ms"] > deadline_ms
    return rec


def describe_engine(endpoint: Endpoint) -> dict:
    """What the engine says about itself. Every part is optional: /v1/models
    is the only one the OpenAI API defines, /version is vLLM's and /props is
    llama-server's. Failures are ignored; a missing key means nobody answered.
    """
    info: dict = {"base_url": endpoint.base_url, "is_mock": False}
    models = endpoint.get_json(endpoint.prefix + "/models")
    if models is not None:
        info["models"] = models
        data = models.get("data") if isinstance(models, dict) else None
        if isinstance(data, list):
            info["is_mock"] = any(
                isinstance(m, dict) and m.get("owned_by") == MOCK_OWNER for m in data)
    for name in ("version", "props"):
        found = endpoint.get_json(f"{endpoint.root}/{name}")
        if found is not None:
            info[name] = found
    return info


def first_model_id(engine: dict) -> str | None:
    models = engine.get("models")
    data = models.get("data") if isinstance(models, dict) else None
    if isinstance(data, list) and data and isinstance(data[0], dict):
        model_id = data[0].get("id")
        return model_id if isinstance(model_id, str) else None
    return None


# --------------------------------------------------------------------------
# The workload.
# --------------------------------------------------------------------------

def default_prompts(seed: int) -> dict:
    """The built-in workload: a stable system prompt and a short user turn
    that changes every request (the shape a prefix cache is built for) for
    the critical class, and tasks that ask for a long answer for the
    background."""
    rng = random.Random(seed)
    critical_user = [
        f"Front lidar: nearest obstacle at {rng.uniform(0.15, 6.0):.2f} m, closing at "
        f"{rng.uniform(0.0, 1.8):.2f} m/s. Bumper: clear. Battery: {rng.randint(11, 99)} %."
        for _ in range(64)]
    return {
        "critical": {"system": DEFAULT_CRITICAL_SYSTEM, "user": critical_user},
        "background": {"system": DEFAULT_BACKGROUND_SYSTEM,
                       "user": list(DEFAULT_BACKGROUND_USER)},
    }


def load_prompts(path: str | None, seed: int) -> dict:
    """Prompts from --prompt-file, over the built-in defaults.

    The file is JSON: {"critical": {"system": "...", "user": ["...", ...]},
    "background": {...}}. Every key is optional. `user` may be one string or a
    list; with a list, each request takes one at random (seeded). `system`
    may be "" for no system message.
    """
    prompts = default_prompts(seed)
    if not path:
        return prompts
    with open(path, encoding="utf-8") as handle:
        loaded = json.load(handle)
    if not isinstance(loaded, dict):
        raise ValueError("--prompt-file must contain a JSON object")
    for klass in ("critical", "background"):
        given = loaded.get(klass)
        if given is None:
            continue
        if not isinstance(given, dict):
            raise ValueError(f"--prompt-file: `{klass}` must be an object")
        if "system" in given:
            if not isinstance(given["system"], str):
                raise ValueError(f"--prompt-file: `{klass}.system` must be a string")
            prompts[klass]["system"] = given["system"]
        if "user" in given:
            user = given["user"]
            user = [user] if isinstance(user, str) else user
            if (not isinstance(user, list) or not user
                    or not all(isinstance(u, str) and u for u in user)):
                raise ValueError(
                    f"--prompt-file: `{klass}.user` must be a string or a list of strings")
            prompts[klass]["user"] = user
    return prompts


class Bench:
    def __init__(self, args, endpoint: Endpoint, model: str, prompts: dict) -> None:
        self.args = args
        self.endpoint = endpoint
        self.model = model
        self.prompts = prompts
        self.priorities = dict(zip(("critical", "background"), PRIORITIES[args.priority_mode]))
        self.usage_support = UsageSupport()
        self.clock0 = time.perf_counter()
        self.records: list[dict] = []
        self._lock = threading.Lock()
        self._ids = itertools.count(1)
        self._critical_active = 0
        self._seed = args.seed if args.seed is not None else 0

    def body(self, klass: str, rng: random.Random, max_tokens: int | None = None) -> dict:
        prompts = self.prompts[klass]
        messages = []
        if prompts["system"]:
            messages.append({"role": "system", "content": prompts["system"]})
        messages.append({"role": "user", "content": rng.choice(prompts["user"])})
        # The engine's knobs first, the bench's own fields over them: an
        # --extra-body that replaced `max_tokens` would make the two classes
        # the same class without anything in the output saying so.
        body = dict(self.args.extra_body)
        body["model"] = self.model
        body["messages"] = messages
        body["max_tokens"] = max_tokens or (
            self.args.critical_max_tokens if klass == "critical"
            else self.args.background_max_tokens)
        if self.priorities[klass] is not None:
            body["priority"] = self.priorities[klass]
        if self.args.seed is not None:
            body["seed"] = self.args.seed
        return body

    def _add(self, rec: dict) -> None:
        with self._lock:
            self.records.append(rec)

    # -- probe ------------------------------------------------------------

    def probe(self) -> list[dict]:
        """One token of each class's request shape, before anything is timed.

        It settles three things while a failure is still cheap: the endpoint
        answers at all, it accepts `stream_options` (so no measured request
        pays for finding out), and it accepts each class's `priority`. It also
        takes the model's first-request cost out of the baseline.
        """
        rng = random.Random(self._seed)
        out = []
        for klass in ("critical", "background"):
            if klass == "background" and self.args.background_concurrency == 0:
                continue
            rec = stream_request(
                self.endpoint, self.body(klass, rng, max_tokens=1), klass=klass,
                phase="probe", clock0=self.clock0,
                stall_timeout_s=self.args.request_timeout_s,
                usage_support=self.usage_support)
            out.append(rec)
            if rec["outcome"] != "completed":
                break
        return out

    # -- phases -----------------------------------------------------------

    def run_phase(self, name: str, background: int) -> dict:
        args = self.args
        started = time.perf_counter()
        meta = {"name": name, "background_concurrency": background,
                "started_unix_ms": int(time.time() * 1000)}
        log(f"phase {name}: critical every {args.critical_period_ms:g} ms for "
            f"{args.duration_s:g} s"
            + (f", with {background} background generation(s)" if background else ", alone"))

        bg_stop = threading.Event()
        bg_inflight = Inflight()
        bg_threads = []
        for worker in range(background):
            thread = threading.Thread(
                target=self._background_worker, args=(name, worker, bg_stop, bg_inflight),
                daemon=True)
            thread.start()
            bg_threads.append(thread)
        if background and args.warmup_s > 0:
            # Let the background take the engine's slots before the first
            # critical request arrives, or the first few are measured against
            # an engine that is not yet loaded.
            time.sleep(args.warmup_s)

        critical_inflight = Inflight()
        window_started = time.perf_counter()
        critical_threads = self._critical_stream(name, critical_inflight)
        meta["critical_send_window_s"] = round(time.perf_counter() - window_started, 3)

        # The background keeps running while the last critical requests
        # finish: they were sent under load and must be answered under it.
        give_up_at = time.perf_counter() + args.drain_timeout_s
        for thread in critical_threads:
            thread.join(timeout=max(give_up_at - time.perf_counter(), 0.0))
        drained = not any(t.is_alive() for t in critical_threads)
        if not drained:
            log(f"phase {name}: critical requests still open after "
                f"{args.drain_timeout_s:g} s; hanging up on them")
            critical_inflight.abort()
            for thread in critical_threads:
                thread.join(timeout=5.0)
        meta["critical_drained"] = drained

        bg_stop.set()
        bg_inflight.abort()
        for thread in bg_threads:
            thread.join(timeout=10.0)
        meta["wall_s"] = round(time.perf_counter() - started, 3)
        return meta

    def _critical_stream(self, phase: str, inflight: Inflight) -> list[threading.Thread]:
        args = self.args
        period = args.critical_period_ms / 1000.0
        rng = random.Random(self._seed)
        threads: list[threading.Thread] = []
        start = time.perf_counter()
        for n in itertools.count():
            due = start + n * period
            if due - start >= args.duration_s:
                break
            wait = due - time.perf_counter()
            if wait > 0:
                time.sleep(wait)
            req_id = next(self._ids)
            body = self.body("critical", rng)
            lag_ms = (time.perf_counter() - due) * 1000.0
            with self._lock:
                saturated = self._critical_active >= args.max_inflight_critical
                if not saturated:
                    self._critical_active += 1
            if saturated:
                # Not sent, and said so. Dropping it silently would make an
                # overloaded engine look better the worse it gets.
                self._add({"id": req_id, "class": "critical", "phase": phase,
                           "outcome": "skipped", "t_send_s": round(due - self.clock0, 6),
                           "error": "not sent: --max-inflight-critical reached"})
                continue
            thread = threading.Thread(
                target=self._critical_one, args=(phase, req_id, body, lag_ms, inflight),
                daemon=True)
            thread.start()
            threads.append(thread)
        return threads

    def _critical_one(self, phase, req_id, body, lag_ms, inflight) -> None:
        args = self.args
        try:
            rec = stream_request(
                self.endpoint, body, klass="critical", phase=phase, req_id=req_id,
                clock0=self.clock0, deadline_ms=args.critical_deadline_ms,
                cancel_at_deadline=args.cancel_at_deadline,
                stall_timeout_s=args.request_timeout_s,
                usage_support=self.usage_support, inflight=inflight)
            rec["send_lag_ms"] = round(lag_ms, 3)
            self._add(rec)
        finally:
            with self._lock:
                self._critical_active -= 1

    def _background_worker(self, phase, worker, stop, inflight) -> None:
        args = self.args
        rng = random.Random(self._seed * 1_000_003 + worker + 1)
        while not stop.is_set():
            rec = stream_request(
                self.endpoint, self.body("background", rng), klass="background",
                phase=phase, req_id=next(self._ids), clock0=self.clock0,
                stall_timeout_s=args.request_timeout_s,
                usage_support=self.usage_support, inflight=inflight)
            self._add(rec)
            if rec["outcome"] == "failed":
                # Do not spin against an engine that refuses everything.
                stop.wait(0.2)


# --------------------------------------------------------------------------
# Summary and output.
# --------------------------------------------------------------------------

def _round(value, digits: int = 3):
    return None if value is None else round(value, digits)


def summarize(records: list[dict], phases: list[dict]) -> list[dict]:
    rows = []
    for phase in phases:
        for klass in ("critical", "background"):
            if klass == "background" and not phase["background_concurrency"]:
                continue
            mine = [r for r in records if r["phase"] == phase["name"] and r["class"] == klass]
            sent = [r for r in mine if r["outcome"] != "skipped"]
            ttft = [r["ttft_ms"] for r in sent if r["ttft_ms"] is not None]
            e2e = [r["e2e_ms"] for r in sent if r["e2e_ms"] is not None]
            outcomes = Counter(r["outcome"] for r in mine)
            statuses = Counter(
                str(r["http_status"]) if r["http_status"] is not None else "no response"
                for r in sent)
            errors = Counter(r["error"] for r in sent if r["outcome"] == "failed" and r["error"])

            tokens = sum(r["output_tokens"] for r in sent)
            sources = {r["output_tokens_source"] for r in sent if r["output_tokens"]}
            span = None
            if sent:
                span = max(r["t_end_s"] for r in sent) - min(r["t_send_s"] for r in sent)
            # Decode rate of one stream: the tokens after the first, over the
            # time after the first. The first token's wait is TTFT, not speed.
            rates = [(r["output_tokens"] - 1) / (r["gen_ms"] / 1000.0)
                     for r in sent if r["output_tokens"] > 1 and r["gen_ms"]]
            with_usage = [r["usage"] for r in sent if r["usage"] is not None]
            with_cached = [u for u in with_usage if u["cached_tokens"] is not None]
            has_deadline = any(r["deadline_ms"] is not None for r in sent)
            lags = [r["send_lag_ms"] for r in sent if r.get("send_lag_ms") is not None]

            rows.append({
                "phase": phase["name"], "class": klass,
                "sent": len(sent),
                "completed": outcomes["completed"],
                "failed": outcomes["failed"],
                "cancelled_at_deadline": outcomes["cancelled_at_deadline"],
                "aborted": outcomes["aborted"],
                "skipped": outcomes["skipped"],
                "http_status": dict(sorted(statuses.items())),
                "errors": [{"count": n, "error": e} for e, n in errors.most_common(5)],
                "ttft_n": len(ttft),
                "ttft_p50_ms": _round(percentile(ttft, 50)),
                "ttft_p90_ms": _round(percentile(ttft, 90)),
                "ttft_p99_ms": _round(percentile(ttft, 99)),
                "ttft_max_ms": _round(max(ttft) if ttft else None),
                "e2e_n": len(e2e),
                "e2e_p50_ms": _round(percentile(e2e, 50)),
                "e2e_p90_ms": _round(percentile(e2e, 90)),
                "e2e_p99_ms": _round(percentile(e2e, 99)),
                "output_tokens": tokens if sent else None,
                "output_tokens_source": (
                    None if not sources else sources.pop() if len(sources) == 1 else "mixed"),
                "active_span_s": _round(span),
                "output_tokens_per_s": _round(tokens / span if sent and span else None, 2),
                "stream_tokens_per_s_n": len(rates),
                "stream_tokens_per_s_p50": _round(percentile(rates, 50), 2),
                "deadline_ms": sent[0]["deadline_ms"] if has_deadline else None,
                "deadline_miss_first_token": (
                    sum(1 for r in sent if r["first_token_late"]) if has_deadline else None),
                "deadline_miss_completion": (
                    sum(1 for r in sent if r["completion_late"]) if has_deadline else None),
                "usage_reported_n": len(with_usage),
                "prompt_tokens": (sum(u["prompt_tokens"] or 0 for u in with_usage)
                                  if with_usage else None),
                "completion_tokens": (sum(u["completion_tokens"] or 0 for u in with_usage)
                                      if with_usage else None),
                "cached_reported_n": len(with_cached),
                "cached_tokens": (sum(u["cached_tokens"] for u in with_cached)
                                  if with_cached else None),
                "send_lag_max_ms": _round(max(lags) if lags else None),
            })
    return rows


def compare(summary: list[dict]) -> dict:
    """Critical TTFT, loaded against baseline. None wherever either side has
    no samples: a ratio against nothing is not a number."""
    def row(phase):
        return next((r for r in summary if r["phase"] == phase and r["class"] == "critical"),
                    None)
    base, loaded = row("baseline"), row("loaded")
    out = {}
    for key in ("ttft_p50_ms", "ttft_p99_ms"):
        a = base[key] if base else None
        b = loaded[key] if loaded else None
        out[key] = {"baseline": a, "loaded": b,
                    "ratio": round(b / a, 2) if a and b is not None else None}
    return out


def _ms(value) -> str:
    if value is None:
        return "n/a"
    return f"{value:.1f}" if value < 100 else f"{value:.0f}"


def _num(value, fmt: str = "{}") -> str:
    return "n/a" if value is None else fmt.format(value)


def _of(part, whole) -> str:
    if part is None:
        return "n/a"
    return f"{part} of {whole}"


def _table(header: list[str], rows: list[list[str]]) -> list[str]:
    lines = ["| " + " | ".join(header) + " |", "|" + "|".join("---" for _ in header) + "|"]
    lines += ["| " + " | ".join(row) + " |" for row in rows]
    return lines


def markdown(result: dict) -> str:
    cfg = result["config"]
    engine = result["engine"]
    summary = result["summary"]
    out: list[str] = []
    out.append(f"### robot_bench: {result['model']}")
    out.append("")
    if engine.get("is_mock"):
        out.append("> **MOCK ENGINE.** These numbers come from `mock_engine.py`, which is "
                   "sleep() behind a queue. They show that the harness works. They say "
                   "nothing about any model, engine or GPU.")
        out.append("")
    version = engine.get("version")
    version = version.get("version") if isinstance(version, dict) else None
    out.append(f"- engine: `{engine['base_url']}`"
               + (f", reports version `{version}`" if version else "")
               + f"; started {result['started_utc']}")
    out.append(f"- critical: one request every {cfg['critical_period_ms']:g} ms, "
               f"max_tokens {cfg['critical_max_tokens']}, deadline "
               f"{cfg['critical_deadline_ms']:g} ms"
               + (", cancelled at the deadline" if cfg["cancel_at_deadline"] else ""))
    out.append(f"- background: {cfg['background_concurrency']} generation(s) at a time, "
               f"max_tokens {cfg['background_max_tokens']}; {cfg['duration_s']:g} s per phase")
    prio = result["priority"]
    out.append(f"- priority mode: `{prio['mode']}`"
               + ("" if prio["mode"] == "none" else
                  f" (critical {prio['critical']}, background {prio['background']})"))
    out.append(f"- command: `{result['command_line']}`")
    out.append("")

    rows = []
    for r in summary:
        approx = "" if r["output_tokens_source"] in (None, "engine") else " ~"
        rows.append([
            r["phase"], r["class"], str(r["sent"]), str(r["ttft_n"]),
            _ms(r["ttft_p50_ms"]), _ms(r["ttft_p90_ms"]), _ms(r["ttft_p99_ms"]),
            _ms(r["ttft_max_ms"]), str(r["e2e_n"]),
            _ms(r["e2e_p50_ms"]), _ms(r["e2e_p90_ms"]), _ms(r["e2e_p99_ms"]),
            _num(r["output_tokens_per_s"], "{:.1f}") + (approx if r["sent"] else ""),
            _num(r["stream_tokens_per_s_p50"], "{:.1f}"),
        ])
    out += _table(
        ["phase", "class", "sent", "TTFT n", "TTFT p50 ms", "p90", "p99", "max",
         "e2e n", "e2e p50 ms", "p90", "p99", "out tok/s (class)", "tok/s per stream p50"],
        rows)
    out.append("")

    rows = []
    for r in summary:
        status = ", ".join(f"{code} x{n}" for code, n in r["http_status"].items()) or "n/a"
        rows.append([
            r["phase"], r["class"], str(r["sent"]), str(r["completed"]), str(r["failed"]),
            str(r["cancelled_at_deadline"]), str(r["aborted"]), status,
            _of(r["deadline_miss_first_token"], r["sent"]),
            _of(r["deadline_miss_completion"], r["sent"]),
            _num(r["prompt_tokens"]), _num(r["cached_tokens"]), _num(r["completion_tokens"]),
        ])
    out += _table(
        ["phase", "class", "sent", "completed", "failed", "cancelled at deadline",
         "cut at phase end", "HTTP", "missed deadline: first token",
         "missed deadline: completion", "prompt tok", "cached tok", "completion tok"],
        rows)
    out.append("")

    cmp_ = result["comparison"]
    parts = []
    for key, label in (("ttft_p50_ms", "p50"), ("ttft_p99_ms", "p99")):
        c = cmp_[key]
        if c["ratio"] is None:
            parts.append(f"{label} n/a")
        else:
            parts.append(f"{label} x{c['ratio']:.1f} ({_ms(c['baseline'])} -> "
                         f"{_ms(c['loaded'])} ms)")
    # The ratio is over the requests that produced a first token. How many
    # did not belongs in the same sentence, or a run where most of them were
    # cancelled or refused reads as a good one.
    seen = [f"{r['ttft_n']} of {r['sent']} {r['phase']}"
            for r in summary if r["class"] == "critical"]
    out.append("**Critical time to first token, loaded against baseline:** "
               + ", ".join(parts) + ". A first token arrived for "
               + " and ".join(seen) + " critical requests.")
    out.append("")

    notes = [
        "Percentiles are nearest-rank (the ceil(p/100*n)-th smallest sample, no "
        "interpolation); with n < 100, p99 is the maximum.",
        "TTFT runs from just before the TCP connect to the first chunk that carries text. "
        "e2e covers completed requests only. Deadline misses are counted over requests sent.",
        "The token columns are what the engine reported in `usage`; n/a means it reported "
        "none.",
    ]
    if any(r["output_tokens_source"] not in (None, "engine") for r in summary):
        notes.append("`~` marks a token rate counted from stream chunks because the engine "
                     "reported no usage for some or all of those requests.")
    if not result["stream_options_supported"]:
        notes.append("The engine refused `stream_options`, so it was not asked for usage.")
    if cfg["cancel_at_deadline"]:
        notes.append("Requests cancelled at the deadline before their first token have no "
                     "TTFT, so the TTFT columns describe only the requests that made it.")
    skipped = sum(r["skipped"] for r in summary)
    if skipped:
        notes.append(f"{skipped} critical request(s) were NOT SENT: --max-inflight-critical "
                     "was reached. They are in none of the columns above.")
    lag = max((r["send_lag_max_ms"] or 0.0) for r in summary) if summary else 0.0
    if lag > cfg["critical_period_ms"] / 2:
        notes.append(f"The load generator itself fell behind: a critical request left up to "
                     f"{lag:.0f} ms late. Treat the numbers with suspicion.")
    for r in summary:
        for e in r["errors"][:2]:
            notes.append(f"{r['phase']}/{r['class']} failure x{e['count']}: {e['error']}")
    out += [f"- {n}" for n in notes]
    return "\n".join(out) + "\n"


def _redact(argv: list[str]) -> list[str]:
    """The command line as typed, minus the one thing in it that is a secret."""
    out, hide = [], False
    for arg in argv:
        if hide:
            out.append("***")
            hide = False
        elif arg == "--api-key":
            out.append(arg)
            hide = True
        elif arg.startswith("--api-key="):
            out.append("--api-key=***")
        else:
            out.append(arg)
    return out


def _quote(arg: str) -> str:
    if arg and all(c.isalnum() or c in "-_./:=,@%+" for c in arg):
        return arg
    return "'" + arg.replace("'", "'\"'\"'") + "'"


def utc_iso(unix_s: float) -> str:
    return datetime.datetime.fromtimestamp(unix_s, datetime.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%SZ")


def json_object_arg(text: str) -> dict:
    try:
        parsed = json.loads(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"not valid JSON: {exc}") from None
    if not isinstance(parsed, dict):
        raise argparse.ArgumentTypeError("must be a JSON object")
    return parsed


def positive_arg(text: str) -> float:
    value = float(text)
    if value <= 0:
        raise argparse.ArgumentTypeError("must be greater than 0")
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="robot_bench.py",
        description="Robot-shaped load against an OpenAI-compatible streaming endpoint: a "
                    "short critical request on a fixed period with a deadline, alone and "
                    "then with long background generations running.",
        epilog="See the module docstring and README.md for how each number is measured.")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080/v1",
                        help="OpenAI-compatible base URL (default http://127.0.0.1:8080/v1)")
    parser.add_argument("--model", default=None,
                        help="model name to send (default: the first one /v1/models lists)")
    parser.add_argument("--api-key", default=None,
                        help="sent as a Bearer token; never written to the output")
    parser.add_argument("--duration-s", type=positive_arg, default=30.0,
                        help="how long the critical stream runs in EACH phase (default 30)")
    parser.add_argument("--warmup-s", type=float, default=2.0,
                        help="in the loaded phase, how long the background runs before the "
                             "first critical request (default 2)")

    group = parser.add_argument_group("critical stream")
    group.add_argument("--critical-period-ms", type=positive_arg, default=200.0,
                       help="one request every this many ms, open loop (default 200)")
    group.add_argument("--critical-max-tokens", type=int, default=16)
    group.add_argument("--critical-deadline-ms", type=positive_arg, default=500.0,
                       help="deadline after each request is sent (default 500; an arbitrary "
                            "default, set it to the budget of the loop you care about)")
    group.add_argument("--cancel-at-deadline", action="store_true",
                       help="hang up on a critical request at its deadline, as a robot that "
                            "has moved on would")
    group.add_argument("--max-inflight-critical", type=int, default=256,
                       help="critical requests allowed open at once; beyond it they are "
                            "counted as not sent (default 256)")

    group = parser.add_argument_group("background load")
    group.add_argument("--background-concurrency", type=int, default=4,
                       help="long generations kept running in the loaded phase (default 4; "
                            "0 skips the loaded phase). To saturate, use more than the engine "
                            "runs at once")
    group.add_argument("--background-max-tokens", type=int, default=512)

    group = parser.add_argument_group("request contents")
    group.add_argument("--priority-mode", choices=sorted(PRIORITIES), default="none",
                       help="none: no priority. vllm: `priority`, lower first (critical 0, "
                            "background 10); needs vLLM started with --scheduling-policy "
                            "priority, which otherwise refuses a non-zero priority. sglang: "
                            "`priority`, higher first (critical 10, background 0)")
    group.add_argument("--extra-body", type=json_object_arg, default={},
                       help="JSON object merged into every request, e.g. "
                            "'{\"temperature\": 0, \"cache_prompt\": true}'. The bench's own "
                            "fields (model, messages, max_tokens, stream, priority) win")
    group.add_argument("--prompt-file", default=None,
                       help="JSON with the prompts of either class; see README.md")
    group.add_argument("--seed", type=int, default=None,
                       help="seeds the choice of prompts, and is sent to the engine as "
                            "`seed`. Without it the prompts use seed 0 and no `seed` is sent")

    group = parser.add_argument_group("run control")
    group.add_argument("--request-timeout-s", type=positive_arg, default=120.0,
                       help="a request that receives nothing for this long has failed "
                            "(default 120)")
    group.add_argument("--drain-timeout-s", type=positive_arg, default=60.0,
                       help="how long to wait for open critical requests when a phase ends "
                            "(default 60)")
    group.add_argument("--out", default=None,
                       help="JSON file for every request record and the summary "
                            "(default robot-bench-<UTC time>.json)")
    return parser


def run(args, argv: list[str]) -> tuple[int, dict | None]:
    """Run the bench. Returns (exit code, result); the result is None when the
    run never started."""
    started = time.time()
    try:
        endpoint = Endpoint(args.base_url, args.api_key)
        prompts = load_prompts(args.prompt_file, args.seed if args.seed is not None else 0)
    except (OSError, ValueError) as exc:
        log(str(exc))
        return 2, None
    if args.critical_max_tokens < 1 or args.background_max_tokens < 1:
        log("max_tokens must be at least 1")
        return 2, None
    if args.background_concurrency < 0 or args.max_inflight_critical < 1:
        log("--background-concurrency must be >= 0 and --max-inflight-critical >= 1")
        return 2, None

    engine = describe_engine(endpoint)
    model = args.model or first_model_id(engine)
    if not model:
        log(f"no --model given and {endpoint.base_url}/models did not name one")
        return 2, None
    if engine["is_mock"]:
        log("the engine is the bundled mock: the output shows the harness works, nothing more")

    bench = Bench(args, endpoint, model, prompts)
    probes = bench.probe()
    bad = next((p for p in probes if p["outcome"] != "completed"), None)
    if bad is not None:
        log(f"the engine refused a one-token {bad['class']} request: {bad['error']}")
        if args.priority_mode == "vllm" and bad["http_status"] == 400:
            log("with --priority-mode vllm this usually means vLLM was started without "
                "--scheduling-policy priority, which refuses a non-zero priority. Start it "
                "with the flag, or use --priority-mode none against the default policy.")
        return 2, None

    phases = [bench.run_phase("baseline", 0)]
    if args.background_concurrency > 0:
        phases.append(bench.run_phase("loaded", args.background_concurrency))
    else:
        log("--background-concurrency 0: there is no loaded phase to run")
    finished = time.time()

    records = sorted(bench.records, key=lambda r: r["id"])
    summary = summarize(records, phases)
    config = {k: v for k, v in vars(args).items() if k != "api_key"}
    config["api_key_given"] = bool(args.api_key)
    command = ["python", "robot_bench.py", *_redact(argv)]
    result = {
        "tool": "robot_bench",
        "schema": SCHEMA,
        "command": command,
        "command_line": " ".join(_quote(a) for a in command),
        "started_utc": utc_iso(started),
        "finished_utc": utc_iso(finished),
        "started_unix_ms": int(started * 1000),
        "finished_unix_ms": int(finished * 1000),
        "host": {
            "note": "the machine that generated the load, not the one running the engine",
            "platform": platform.platform(),
            "machine": platform.machine(),
            "python": platform.python_version(),
            "cpu_count": os.cpu_count(),
        },
        "engine": engine,
        "model": model,
        "config": config,
        "priority": {"mode": args.priority_mode,
                     "critical": bench.priorities["critical"],
                     "background": bench.priorities["background"]},
        "stream_options_supported": bench.usage_support.supported,
        "percentile_method": PERCENTILE_METHOD,
        "probe": probes,
        "phases": phases,
        "summary": summary,
        "comparison": compare(summary),
        "requests": records,
    }
    return 0, result


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    args = build_parser().parse_args(argv)
    code, result = run(args, argv)
    if result is None:
        return code
    out_path = args.out or time.strftime("robot-bench-%Y%m%dT%H%M%SZ.json", time.gmtime())
    with open(out_path, "w", encoding="utf-8") as handle:
        json.dump(result, handle, indent=1)
        handle.write("\n")
    log(f"{len(result['requests'])} request records written to {out_path}")
    sys.stdout.write(markdown(result))
    sys.stdout.flush()
    return code


if __name__ == "__main__":
    sys.exit(main())
