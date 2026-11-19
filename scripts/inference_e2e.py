#!/usr/bin/env python3
"""Inference through the node, against a real node and a real SDK Expertise.

The unit tests prove the plane relays a stream and cuts it. This proves the
part they cannot: a process built on the SDK, holding only a credential scoped
to its own capability, calling a model *through its node* — and the node
placing that request in the right graph, taking back the priority the request
claimed, stopping the model when the chain is cancelled, and stopping
everything when the node itself is told to stop.

One real node (authentication ON, which is the default and the point), one
fake engine that this script controls, and two Expertise built on the SDK:

  talker   calls the model through the node for every message it is handed
  thief    names talker's delivery on its own request, to borrow its priority

It checks:

  - the request reaches the engine, placed in the graph its session is running;
  - the node asked the engine for usage, the caller's stream does not show it,
    and /v1/stats carries what the engine counted;
  - the engine is sent the priority of the *edge*, in the engine's own
    convention, and not the one the request wrote;
  - the node's credential and the delivery headers stop at the node;
  - a request under someone else's delivery is refused, and never reaches
    the engine;
  - a client's cancel closes the request at the engine — and how long that took;
  - a deadline closes it too;
  - a node stopped mid-generation cuts the request and exits inside its drain
    window — and how long that took;
  - what going through the node costs, per request, against the same engine
    reached directly.

    python scripts/inference_e2e.py ./kernel/aura [--port 9398]

Needs: websockets, pyyaml (for the SDK Expertise). POSIX only: it sends SIGTERM.
"""
from __future__ import annotations

import argparse
import asyncio
import http.server
import json
import os
import re
import shutil
import signal
import statistics
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

import websockets

ROOT = Path(__file__).resolve().parent.parent

ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def new_id() -> str:
    now = int(time.time() * 1000)
    ts = []
    for _ in range(10):
        ts.append(ALPHABET[now & 31])
        now >>= 5
    return "".join(reversed(ts)) + "".join(ALPHABET[b & 31] for b in os.urandom(16))


class Check:
    def __init__(self) -> None:
        self.failed: list[str] = []

    def __call__(self, name: str, ok: bool, detail: str = "") -> None:
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail and not ok else ""))
        if not ok:
            self.failed.append(name)


# --------------------------------------------------------------------------- engine

class Engine:
    """An OpenAI-compatible server this script can hold open and watch."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.requests: list[dict] = []
        self.hold = threading.Event()   # set = streams wait after their first token
        self.released: list[float] = []  # when each held stream's caller went away
        self.first_token: list[float] = []
        engine = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):  # noqa: D102 - silence the default access log
                pass

            def do_GET(self):  # noqa: N802
                body = json.dumps({"object": "list", "data": [{"id": "e2e-model"}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):  # noqa: N802
                raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                body = json.loads(raw or b"{}")
                with engine.lock:
                    engine.requests.append({
                        "path": self.path, "body": body,
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                    })
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()

                def event(obj) -> bool:
                    try:
                        self.wfile.write(b"data: " + (obj if isinstance(obj, bytes) else
                                                       json.dumps(obj).encode()) + b"\n\n")
                        self.wfile.flush()
                        return True
                    except OSError:
                        return False

                event({"choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}}]})
                event({"choices": [{"index": 0, "delta": {"content": "hel"}}]})
                with engine.lock:
                    engine.first_token.append(time.monotonic())
                if engine.hold.is_set():
                    # Hold the stream open the way a long generation does, and
                    # notice the moment the other side goes away.
                    while engine.hold.is_set():
                        if not event({"choices": [{"index": 0, "delta": {"content": "."}}]}):
                            with engine.lock:
                                engine.released.append(time.monotonic())
                            return
                        time.sleep(0.01)
                event({"choices": [{"index": 0, "delta": {"content": "lo"}}]})
                event({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
                if (body.get("stream_options") or {}).get("include_usage"):
                    event({"choices": [], "usage": {
                        "prompt_tokens": 40, "completion_tokens": 2,
                        "prompt_tokens_details": {"cached_tokens": 32}}})
                event(b"[DONE]")

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.port = self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/v1"

    def count(self) -> int:
        with self.lock:
            return len(self.requests)

    def last(self) -> dict:
        with self.lock:
            return self.requests[-1]

    def wait_requests(self, n: int, limit: float = 15.0) -> bool:
        end = time.monotonic() + limit
        while time.monotonic() < end:
            if self.count() >= n:
                return True
            time.sleep(0.005)
        return False

    def wait_first_token(self, n: int, limit: float = 15.0) -> bool:
        end = time.monotonic() + limit
        while time.monotonic() < end:
            with self.lock:
                if len(self.first_token) >= n:
                    return True
            time.sleep(0.002)
        return False

    def wait_released(self, n: int, limit: float = 15.0) -> float | None:
        end = time.monotonic() + limit
        while time.monotonic() < end:
            with self.lock:
                if len(self.released) >= n:
                    return self.released[n - 1]
            time.sleep(0.002)
        return None


# --------------------------------------------------------------------------- Expertise

EXPERTISE = textwrap.dedent('''
    """An SDK Expertise that calls a model through its node for every message."""
    import asyncio
    import os

    from aura import ChatBackend, Context, DeliveryRef, Skill

    skill = Skill()
    backend = ChatBackend()


    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        delivery = ctx.delivery
        steal = os.getenv("E2E_STEAL_FROM", "")
        if steal:
            # Name another Expertise's delivery instead of our own: the one
            # talker was handed for this same message, in this same session.
            theirs = ""
            for _ in range(500):
                try:
                    session, _, theirs = open(steal).read().strip().partition(" ")
                except OSError:
                    session = ""
                if session == ctx.session and theirs:
                    break
                await asyncio.sleep(0.01)
            delivery = DeliveryRef(session=ctx.session, id=theirs, node="talker", port="text_in")
        elif os.getenv("E2E_RECORD_DELIVERY"):
            with open(os.environ["E2E_RECORD_DELIVERY"], "w") as fh:
                fh.write(f"{ctx.session} {ctx.id}")

        def run() -> str:
            # priority=1000: this process naming its own urgency. Through the
            # node it is not this process's to state.
            return "".join(backend.stream_chat(
                [{"role": "user", "content": (ctx.payload or {}).get("text", "")}],
                stop=ctx.cancel_event, priority=1000, deadline=ctx.deadline, delivery=delivery))

        try:
            text = await asyncio.to_thread(run)
        except Exception as exc:  # noqa: BLE001 - the answer is what happened
            text = f"ERROR {type(exc).__name__}: {exc}"
        await ctx.emit("text_out", {"text": text, "final": True})


    if __name__ == "__main__":
        skill.run()
''')


def manifest(name: str) -> str:
    return textwrap.dedent(f'''
        id: "e2e/cognitive/{name}"
        version: "1.0.0"
        protocol: "1"
        name: "{name}"
        description: "Calls a model through its node."
        capability: "cognitive.e2e.{name}"
        type: cognitive
        format: source
        runtime:
          language: python
          version: ">=3.11"
        ports:
          ingress:
            - name: text_in
              schema: "std/text@1"
          egress:
            - name: text_out
              schema: "std/text@1"
        requirements:
          memory: "32Mi"
          accelerator: none
        permissions:
          egress_http: []
          filesystem: none
          channels: declared-only
    ''')


# --------------------------------------------------------------------------- the run

class Node:
    def __init__(self, binary: str, port: int, data: str, logs: str, engine: Engine) -> None:
        self.binary, self.port, self.data, self.logs = binary, port, data, logs
        self.engine = engine
        self.proc: subprocess.Popen | None = None
        self.token = ""

    def issue(self, capability: str) -> str:
        out = subprocess.run(
            [self.binary, "token", "issue", "--data", self.data, "--capability", capability],
            capture_output=True, text=True, check=True).stdout
        found = re.findall(r"^\s{2}(\S{20,})\s*$", out, flags=re.M)
        if not found:
            raise SystemExit("could not read the issued token from:\n" + out)
        return found[0]

    def start(self) -> None:
        self.proc = subprocess.Popen(
            [self.binary, "up", "--port", str(self.port), "--data", self.data,
             "--inference-url", self.engine.url, "--inference-priority", "vllm"],
            stdout=open(os.path.join(self.logs, "node.log"), "a"), stderr=subprocess.STDOUT,
            env=dict(os.environ, AURA_ENGINE_API_KEY="engine-key"))
        start = time.monotonic()
        while True:
            try:
                with urllib.request.urlopen(f"http://localhost:{self.port}/healthz", timeout=0.5) as r:
                    if r.status == 200:
                        break
            except Exception:  # noqa: BLE001 - still starting
                pass
            if time.monotonic() - start > 30:
                raise SystemExit("the node never became healthy; see " + self.logs)
            time.sleep(0.05)
        self.token = (Path(self.data) / "node.token").read_text(encoding="utf-8").strip()

    def call(self, method: str, path: str, body: dict | None = None, token: str | None = None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(
            f"http://localhost:{self.port}{path}", data=data, method=method,
            headers={"Content-Type": "application/json",
                     "Authorization": f"Bearer {self.token if token is None else token}"})
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            raw = e.read().decode(errors="replace")
            try:
                return e.code, json.loads(raw)
            except ValueError:
                return e.code, {"error": raw}

    def stats(self) -> dict:
        code, body = self.call("GET", "/v1/stats")
        return (body.get("inference") or {}) if code == 200 else {}

    def graph(self, name: str) -> dict:
        for g in self.stats().get("graphs", []):
            if g["graph"] == name:
                return g
        return {}

    def wait_expertise(self, capability: str, limit: float = 30.0) -> bool:
        start = time.monotonic()
        while time.monotonic() - start < limit:
            code, body = self.call("GET", "/v1/skills")
            if code == 200 and capability in json.dumps(body):
                return True
            time.sleep(0.05)
        return False


class Client:
    """One session: send messages, read what comes back."""

    def __init__(self, node: Node, graph: str) -> None:
        self.url = (f"ws://localhost:{node.port}/v1/stream?graph={graph}"
                    f"&session={new_id()}&token={node.token}")
        self.ws = None
        self.got: list[dict] = []
        self.reader: asyncio.Task | None = None

    async def __aenter__(self) -> "Client":
        self.ws = await websockets.connect(self.url)
        await self.ws.recv()  # ready

        async def read() -> None:
            try:
                async for raw in self.ws:
                    self.got.append(json.loads(raw))
            except websockets.ConnectionClosed:
                pass

        self.reader = asyncio.create_task(read())
        return self

    async def __aexit__(self, *_exc) -> None:
        await self.ws.close()
        if self.reader is not None:
            self.reader.cancel()

    async def say(self, text: str) -> str:
        """Send a message as a full envelope, so its id is known for a cancel."""
        msg_id = new_id()
        await self.ws.send(json.dumps({
            "v": "1", "id": msg_id, "kind": "data", "port": "text_out", "seq": len(self.got) + 1,
            "idem": new_id(), "schema": "std/text@1", "payload": {"text": text, "final": True}}))
        return msg_id

    async def cancel(self, msg_id: str) -> None:
        await self.ws.send(json.dumps({"v": "1", "id": new_id(), "kind": "cancel", "cause_id": msg_id}))

    async def reply(self, limit: float = 20.0) -> str | None:
        end = time.monotonic() + limit
        while time.monotonic() < end:
            for env in self.got:
                if env.get("kind") == "data":
                    return (env.get("payload") or {}).get("text")
            await asyncio.sleep(0.01)
        return None


def graph_ir(graph_id: str, node: str, **edge) -> dict:
    return {
        "ir": "1", "graph_id": graph_id, "origin": {"kind": "declared"},
        "nodes": [{"ref": node, "resolve": f"cognitive.e2e.{node}"}],
        "edges": [
            dict({"from": "client.text_out", "to": f"{node}.text_in"}, **edge),
            {"from": f"{node}.text_out", "to": "client.text_in"},
        ],
    }


async def scenarios(node: Node, engine: Engine, check: Check, results: dict) -> None:
    for gid, ref, edge in (
        ("talk", "talker", {"priority": 7}),
        ("talk-deadline", "talker", {"deadline_ms": 1200}),
    ):
        code, body = node.call("POST", "/v1/graphs", graph_ir(gid, ref, **edge))
        check(f"graph {gid} registers", code in (200, 201), str(body))
    # Both Expertise handed the same message in the same session, so that one
    # can name the delivery that was made to the other. Only the thief answers
    # the client.
    code, body = node.call("POST", "/v1/graphs", {
        "ir": "1", "graph_id": "steal", "origin": {"kind": "declared"},
        "nodes": [{"ref": "talker", "resolve": "cognitive.e2e.talker"},
                  {"ref": "thief", "resolve": "cognitive.e2e.thief"}],
        "edges": [
            {"from": "client.text_out", "to": "talker.text_in", "priority": 9},
            {"from": "client.text_out", "to": "thief.text_in"},
            {"from": "thief.text_out", "to": "client.text_in"},
        ],
    })
    check("graph steal registers", code in (200, 201), str(body))

    # ------------------------------------------------------------ a request, placed
    print("\nA request through the node")
    before = engine.count()
    async with Client(node, "talk") as c:
        await c.say("hello")
        reply = await c.reply()
    check("the Expertise got its reply through the node", reply == "hello", repr(reply))
    check("the engine was called once", engine.count() == before + 1, str(engine.count() - before))
    sent = engine.last()
    check("at the engine's own chat route", sent["path"] == "/v1/chat/completions", sent["path"])
    check("the engine was sent its own key, not the node's or the Expertise's",
          sent["headers"].get("authorization") == "Bearer engine-key",
          str(sent["headers"].get("authorization")))
    check("the delivery headers stopped at the node",
          not [h for h in sent["headers"] if h.startswith("x-aura-")], str(sent["headers"]))
    check("the node asked the engine for usage",
          (sent["body"].get("stream_options") or {}).get("include_usage") is True,
          str(sent["body"].get("stream_options")))
    check("the engine was sent the edge's priority in its own convention (7 -> -7), "
          "not the 1000 the request wrote",
          sent["body"].get("priority") == -7, str(sent["body"].get("priority")))
    g = node.graph("talk")
    check("the request is counted under the graph its session runs",
          g.get("requests") == 1 and g.get("completed") == 1, str(g))
    check("with what the engine counted: 40 in, 32 of them cached, 2 out",
          (g.get("input_tokens"), g.get("cached_input_tokens"), g.get("output_tokens")) == (40, 32, 2),
          str(g))
    check("and a first-token time", (g.get("first_token") or {}).get("count") == 1, str(g.get("first_token")))

    # ------------------------------------------------------------ someone else's delivery
    print("\nA request under another Expertise's delivery")
    before = engine.count()
    refused_before = node.stats().get("refused", 0)
    async with Client(node, "steal") as c:
        await c.say("give me its priority")
        reply = await c.reply()
    check("it was refused, as someone else's delivery", bool(reply) and "403" in reply, repr(reply))
    engine.wait_requests(before + 1)
    await asyncio.sleep(0.3)
    check("and never reached the engine (the one request there is the rightful owner's own)",
          engine.count() == before + 1 and engine.last()["body"].get("priority") == -9,
          f"{engine.count() - before} request(s), last priority {engine.last()['body'].get('priority')}")
    check("and counted as refused", node.stats().get("refused", 0) == refused_before + 1,
          str(node.stats().get("refused")))

    # ------------------------------------------------------------ cancel
    print("\nA cancelled chain")
    engine.hold.set()
    seen_first = len(engine.first_token)
    seen_released = len(engine.released)
    async with Client(node, "talk") as c:
        msg = await c.say("a long answer please")
        check("the generation started", engine.wait_first_token(seen_first + 1))
        t_cancel = time.monotonic()
        await c.cancel(msg)
        released = engine.wait_released(seen_released + 1)
    check("the engine was released when the client cancelled", released is not None)
    if released is not None:
        results["cancel_to_release_ms"] = round((released - t_cancel) * 1000, 1)
        print(f"         cancel -> engine released: {results['cancel_to_release_ms']} ms")
    stopped = node.stats().get("stopped") or {}
    check("and counted as a cancelled chain", stopped.get("chain_cancelled", 0) >= 1, str(stopped))

    # ------------------------------------------------------------ deadline
    print("\nA deadline")
    seen_first = len(engine.first_token)
    seen_released = len(engine.released)
    async with Client(node, "talk-deadline") as c:
        t_sent = time.monotonic()
        await c.say("you have 1.2 seconds")
        check("the generation started", engine.wait_first_token(seen_first + 1))
        released = engine.wait_released(seen_released + 1)
    check("the engine was released when the deadline passed", released is not None)
    if released is not None:
        took = released - t_sent
        results["deadline_cut_after_ms"] = round(took * 1000, 1)
        print(f"         deadline 1200 ms -> engine released after {results['deadline_cut_after_ms']} ms")
        check("close to the deadline, not long after it", 1.0 <= took <= 2.5, f"{took:.2f}s")
    stopped = node.stats().get("stopped") or {}
    check("and counted as a deadline", stopped.get("deadline", 0) >= 1, str(stopped))
    engine.hold.clear()

    # ------------------------------------------------------------ overhead
    print("\nWhat going through the node costs")
    await asyncio.sleep(0.3)  # let the held streams from above finish closing

    def timed(url: str, headers: dict) -> tuple[float, float]:
        body = json.dumps({"model": "m", "messages": [{"role": "user", "content": "hi"}],
                           "stream": True}).encode()
        req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", **headers})
        t0 = time.perf_counter()
        first = None
        with urllib.request.urlopen(req, timeout=20) as r:
            for line in r:
                if first is None and (b'"content": "hel"' in line or b'"content":"hel"' in line):
                    first = time.perf_counter()
        return ((first or time.perf_counter()) - t0) * 1000, (time.perf_counter() - t0) * 1000

    direct_url = engine.url + "/chat/completions"
    node_url = f"http://localhost:{node.port}/v1/inference/chat/completions"
    auth = {"Authorization": f"Bearer {node.token}"}
    for _ in range(10):  # warm both paths
        timed(direct_url, {})
        timed(node_url, auth)
    direct, through = [], []
    for _ in range(150):  # alternated, so drift on the host hits both alike
        direct.append(timed(direct_url, {}))
        through.append(timed(node_url, auth))
    for name, col in (("first_token", 0), ("whole_request", 1)):
        d = statistics.median(x[col] for x in direct)
        n = statistics.median(x[col] for x in through)
        results[f"{name}_direct_ms"] = round(d, 3)
        results[f"{name}_through_node_ms"] = round(n, 3)
        print(f"         {name:14s} direct {d:7.3f} ms   through the node {n:7.3f} ms   "
              f"(+{n - d:.3f} ms, median of 150)")
    added = results["whole_request_through_node_ms"] - results["whole_request_direct_ms"]
    check("the node adds less than 20 ms to a request on loopback (a sanity bound, not a claim)",
          added < 20, f"{added:.2f} ms")
    prep = node.stats().get("prepare_micros_avg")
    results["node_prepare_micros_avg"] = prep
    print(f"         of which the node's own work before sending: {prep:.0f} µs on average")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--port", type=int, default=9398)
    ap.add_argument("--json", help="write the measurements here")
    args = ap.parse_args()

    binary = str(Path(args.binary).resolve())
    data = tempfile.mkdtemp(prefix="aura-infer-")
    work = tempfile.mkdtemp(prefix="aura-infer-expertise-")
    logs = tempfile.mkdtemp(prefix="aura-infer-logs-")
    check = Check()
    results: dict = {}
    engine = Engine()
    node = Node(binary, args.port, data, logs, engine)
    procs: list[subprocess.Popen] = []

    print(f"Inference through the node — {binary}")
    try:
        # Scoped credentials, issued before the node holds the data directory.
        tokens = {name: node.issue(f"cognitive.e2e.{name}") for name in ("talker", "thief")}
        node.start()

        recorded = os.path.join(work, "talker-delivery")
        for name in ("talker", "thief"):
            d = Path(work) / name
            d.mkdir()
            (d / "main.py").write_text(EXPERTISE, encoding="utf-8")
            (d / "expertise.yaml").write_text(manifest(name), encoding="utf-8")
            env = dict(os.environ, PYTHONPATH=str(ROOT / "sdk" / "python" / "src"),
                       PYTHONUNBUFFERED="1",
                       AURA_WS_URL=f"ws://localhost:{args.port}/ws/skill",
                       # The narrow credential: this process can register as its
                       # one capability and call inference, and nothing else.
                       AURA_TOKEN=tokens[name],
                       AURA_INFERENCE_URL="node", AURA_INFERENCE_MODEL="e2e-model")
            env.pop("OPENAI_API_KEY", None)
            if name == "talker":
                env["E2E_RECORD_DELIVERY"] = recorded
            else:
                env["E2E_STEAL_FROM"] = recorded
            procs.append(subprocess.Popen(
                [sys.executable, "main.py"], cwd=str(d), env=env,
                stdout=open(os.path.join(logs, f"{name}.log"), "w"), stderr=subprocess.STDOUT))
        for name in ("talker", "thief"):
            if not node.wait_expertise(f"cognitive.e2e.{name}"):
                raise SystemExit(f"{name} never registered; see {logs}")

        asyncio.run(scenarios(node, engine, check, results))

        # ------------------------------------------------------------ orderly stop
        print("\nA node stopped during a generation")
        engine.hold.set()
        seen_first = len(engine.first_token)
        seen_released = len(engine.released)

        async def start_one() -> Client:
            c = Client(node, "talk")
            await c.__aenter__()
            await c.say("keep talking")
            return c  # left open on purpose: the node is about to be stopped under it

        async def close_one(c: Client) -> None:
            try:
                await asyncio.wait_for(c.__aexit__(None, None, None), timeout=5)
            except Exception:  # noqa: BLE001 - the node it was talking to is gone
                pass
            if c.reader is not None:
                await asyncio.gather(c.reader, return_exceptions=True)

        loop = asyncio.new_event_loop()
        held = loop.run_until_complete(start_one())
        check("a generation is in flight", engine.wait_first_token(seen_first + 1))
        t_term = time.monotonic()
        node.proc.send_signal(signal.SIGTERM)
        released = engine.wait_released(seen_released + 1)
        try:
            code = node.proc.wait(timeout=30)
        except subprocess.TimeoutExpired:
            code = None
        t_exit = time.monotonic()
        loop.run_until_complete(close_one(held))
        loop.close()
        check("the engine was released", released is not None)
        if released is not None:
            results["sigterm_to_release_ms"] = round((released - t_term) * 1000, 1)
            print(f"         SIGTERM -> engine released: {results['sigterm_to_release_ms']} ms")
        check("the node exited cleanly", code == 0, f"exit code {code}")
        results["sigterm_to_exit_s"] = round(t_exit - t_term, 2)
        print(f"         SIGTERM -> node exited: {results['sigterm_to_exit_s']} s (drain window: 10 s)")
        check("well inside its drain window, not at the end of it",
              code is not None and t_exit - t_term < 5.0, f"{t_exit - t_term:.2f}s")
        log = (Path(logs) / "node.log").read_text(encoding="utf-8", errors="replace")
        check("and said what it cut", "cut 1 inference request(s) at the engine" in log)
        check("with its store closed in order", "stopped cleanly" in log)
        engine.hold.clear()
    finally:
        for proc in procs + ([node.proc] if node.proc else []):
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
        engine.server.shutdown()
        if args.json:
            Path(args.json).write_text(json.dumps(results, indent=2), encoding="utf-8")
        if not check.failed:
            for d in (data, work, logs):
                shutil.rmtree(d, ignore_errors=True)

    if check.failed:
        print(f"\n{len(check.failed)} check(s) failed; logs kept in {logs}")
        return 1
    print("\nall checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
