#!/usr/bin/env python3
"""Deadlines and response targets against a real node, over real sockets.

The executor's unit tests prove the rules. This proves the part they cannot:
that the rules hold on the path a production Expertise actually takes — a
WebSocket registration, an emission loop, a mailbox — where the kernel has to
work out *who is speaking* from the connection rather than be told.

One real binary, three Expertise:

  relay   a raw-socket Expertise whose timing this script controls
  sink    a raw-socket Expertise that records what it is handed
  clock   a real SDK Expertise that reports what its handler was shown

and it checks what C2 rules 7 and 9 promise:

  - a deadline declared on the first edge still binds the delivery two hops
    later, though the Expertise in between emitted no deadline at all;
  - an Expertise that writes itself a later deadline is not believed;
  - data emitted after the deadline is dropped, the stream's `done` is not,
    and the client is told once;
  - a late first output under `respond_ms` is recorded and nothing is dropped;
  - an emission naming a delivery made to someone else is counted as foreign;
  - a handler built on the SDK can read its deadline and the time left.

    python scripts/deadline_e2e.py ./kernel/aura [--port 9397]

Needs: websockets, pyyaml (for the SDK Expertise).
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import time
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


def now_ms() -> int:
    return int(time.time() * 1000)


class Check:
    def __init__(self) -> None:
        self.failed: list[str] = []

    def __call__(self, name: str, ok: bool, detail: str = "") -> None:
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail and not ok else ""))
        if not ok:
            self.failed.append(name)


def manifest(name: str) -> dict:
    return {
        "id": f"e2e/logical/{name}", "version": "1.0.0", "protocol": "1",
        "name": name, "description": "deadline e2e", "capability": f"logical.e2e.{name}",
        "type": "logical", "format": "source",
        "ports": {"ingress": [{"name": "text_in", "schema": "std/text@1"}],
                  "egress": [{"name": "text_out", "schema": "std/text@1"}]},
    }


class RawExpertise:
    """An Expertise speaking C3 by hand, so the script decides exactly what
    goes on the wire and when."""

    def __init__(self, url: str, name: str) -> None:
        self.url, self.name = url, name
        self.got: list[dict] = []
        self.ws = None
        self._seq = 0
        self._reader: asyncio.Task | None = None

    async def start(self) -> None:
        self.ws = await websockets.connect(self.url)
        await self.ws.send(json.dumps({
            "v": "1", "id": new_id(), "kind": "register", "payload": manifest(self.name)}))
        ack = json.loads(await asyncio.wait_for(self.ws.recv(), 10))
        if (ack.get("payload") or {}).get("state") != "registered":
            raise SystemExit(f"{self.name} was not registered: {ack}")
        self._reader = asyncio.create_task(self._read())

    async def _read(self) -> None:
        try:
            async for raw in self.ws:
                self.got.append(json.loads(raw))
        except websockets.ConnectionClosed:
            pass

    def deliveries(self, kind: str = "data") -> list[dict]:
        return [e for e in self.got if e.get("kind") == kind and e.get("port") == "text_in"]

    async def wait_for(self, n: int, kind: str = "data", limit: float = 10.0) -> list[dict]:
        end = time.monotonic() + limit
        while time.monotonic() < end:
            if len(self.deliveries(kind)) >= n:
                return self.deliveries(kind)
            await asyncio.sleep(0.01)
        return self.deliveries(kind)

    async def emit(self, cause: dict, text: str, *, kind: str = "data", **extra) -> None:
        self._seq += 1
        env = {
            "v": "1", "id": new_id(), "kind": kind, "cause_id": cause["id"],
            "session": cause["session"], "node": cause["node"], "port": "text_out",
            "seq": self._seq, "idem": new_id(), "schema": "std/text@1",
        }
        if kind == "data":
            env["payload"] = {"text": text, "final": False}
        env.update(extra)
        await self.ws.send(json.dumps(env))

    async def stop(self) -> None:
        if self.ws is not None:
            await self.ws.close()
        if self._reader is not None:
            self._reader.cancel()


CLOCK_EXPERTISE = textwrap.dedent('''
    """A real SDK Expertise: answers with what its handler was shown."""
    from aura import Context, Skill

    skill = Skill()


    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        import json
        seen = {"id": ctx.id, "deadline": ctx.deadline, "remaining_ms": ctx.remaining_ms(),
                "respond_by": ctx.respond_by, "priority": ctx.priority, "expired": ctx.expired}
        await ctx.emit("text_out", {"text": json.dumps(seen), "final": True})


    if __name__ == "__main__":
        skill.run()
''')

CLOCK_MANIFEST = textwrap.dedent('''
    id: "e2e/logical/clock"
    version: "1.0.0"
    protocol: "1"
    name: "Clock"
    description: "Reports what its handler was shown about time."
    capability: "logical.e2e.clock"
    type: logical
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


def graph(graph_id: str, edges: list[dict], nodes: list[str]) -> dict:
    return {
        "ir": "1", "graph_id": graph_id, "origin": {"kind": "declared"},
        "nodes": [{"ref": n, "resolve": f"logical.e2e.{n}"} for n in nodes],
        "edges": edges,
    }


async def run(args, check: Check) -> None:
    base = f"http://localhost:{args.port}"
    ws_base = f"ws://localhost:{args.port}"

    def call(method: str, path: str, body: dict | None = None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=5) as r:
                return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            return e.code, {"error": e.read().decode(errors="replace")}

    def stats() -> dict:
        code, body = call("GET", "/v1/stats")
        return body if code == 200 else {}

    relay = RawExpertise(f"{ws_base}/ws/skill", "relay")
    sink = RawExpertise(f"{ws_base}/ws/skill", "sink")
    await relay.start()
    await sink.start()

    async def session(graph_id: str, text: str, *, deadline: int | None = None):
        """Open a session, send one message, and keep reading what comes back."""
        ws = await websockets.connect(f"{ws_base}/v1/stream?graph={graph_id}&session={new_id()}")
        await ws.recv()  # ready
        got: list[dict] = []

        async def read() -> None:
            try:
                async for raw in ws:
                    got.append(json.loads(raw))
            except websockets.ConnectionClosed:
                pass

        reader = asyncio.create_task(read())
        await ws.send(json.dumps({"text": text}))
        return ws, got, reader

    # ---------------------------------------------------------------- rule 7
    print("\nA deadline across an Expertise (C2 rule 7)")
    code, body = call("POST", "/v1/graphs", graph("e2e-deadline", [
        {"from": "client.text_out", "to": "relay.text_in", "deadline_ms": 1500},
        {"from": "relay.text_out", "to": "sink.text_in"},
        {"from": "sink.text_out", "to": "client.text_in"},
    ], ["relay", "sink"]))
    check("the graph registers", code in (200, 201), str(body))

    relay.got.clear()
    sink.got.clear()
    ws, client_got, reader = await session("e2e-deadline", "go")
    to_relay = (await relay.wait_for(1))
    check("the first delivery carries the edge's deadline",
          bool(to_relay) and to_relay[0].get("deadline", 0) > now_ms(), str(to_relay[:1]))
    if not to_relay:
        return
    first = to_relay[0]

    await relay.emit(first, "in time 1")                          # no deadline on the wire
    await relay.emit(first, "in time 2", deadline=first["deadline"] + 3_600_000)  # "one more hour"
    to_sink = await sink.wait_for(2)
    check("the delivery after the Expertise is still under the chain's deadline",
          len(to_sink) == 2 and to_sink[0].get("deadline") == first["deadline"],
          f"sink saw deadline {to_sink[0].get('deadline') if to_sink else None}, want {first['deadline']}")
    check("an Expertise that wrote itself a later deadline was not believed",
          len(to_sink) == 2 and to_sink[1].get("deadline") == first["deadline"],
          f"sink saw {to_sink[1].get('deadline') if len(to_sink) > 1 else None}")

    await asyncio.sleep(max(0.0, (first["deadline"] - now_ms()) / 1000) + 0.25)
    await relay.emit(first, "too late 1")
    await relay.emit(first, "too late 2")
    await relay.emit(first, "", kind="done")
    done = await sink.wait_for(1, kind="done")
    check("the stream's `done` still reaches the next Expertise", len(done) == 1)
    check("data emitted after the deadline was dropped",
          len(sink.deliveries()) == 2, f"sink holds {len(sink.deliveries())} data deliveries")
    await asyncio.sleep(0.2)
    told = [e for e in client_got if e.get("kind") == "status"
            and (e.get("payload") or {}).get("event") == "deadline_exceeded"]
    check("the client was told once that late output is being dropped",
          len(told) == 1, f"told {len(told)} time(s)")
    dl = stats().get("deadlines") or {}
    check("the node counted two dropped deliveries", dl.get("dropped") == 2, str(dl))
    await ws.close()
    reader.cancel()

    # ---------------------------------------------------------------- rule 9
    print("\nA response target (C2 rule 9)")
    code, body = call("POST", "/v1/graphs", graph("e2e-respond", [
        {"from": "client.text_out", "to": "relay.text_in", "respond_ms": 300},
        {"from": "relay.text_out", "to": "sink.text_in"},
        {"from": "sink.text_out", "to": "client.text_in"},
    ], ["relay", "sink"]))
    check("the graph registers", code in (200, 201), str(body))

    relay.got.clear()
    sink.got.clear()
    ws, client_got, reader = await session("e2e-respond", "go")
    to_relay = await relay.wait_for(1)
    first = to_relay[0] if to_relay else {}
    check("the delivery carries respond_by and no deadline",
          first.get("respond_by", 0) > now_ms() and "deadline" not in first, str(first))
    await asyncio.sleep(0.6)  # late to start
    for text in ("sorry", "for", "the", "wait"):
        await relay.emit(first, text)
    to_sink = await sink.wait_for(4)
    check("a reply that started late was delivered in full", len(to_sink) == 4,
          f"sink holds {len(to_sink)} of 4")
    after = stats().get("deadlines") or {}
    check("the lateness was recorded once", after.get("late_responses") == 1, str(after))
    check("nothing was dropped for a response target", after.get("dropped") == dl.get("dropped"),
          str(after))
    await ws.close()
    reader.cancel()

    # ------------------------------------------------------------ cause check
    print("\nThe cause of an emission is checked")
    code, body = call("POST", "/v1/graphs", graph("e2e-cause", [
        {"from": "client.text_out", "to": "relay.text_in", "deadline_ms": 60_000},
        {"from": "client.text_out", "to": "sink.text_in"},
        {"from": "sink.text_out", "to": "client.text_in"},
    ], ["relay", "sink"]))
    check("the graph registers", code in (200, 201), str(body))
    relay.got.clear()
    sink.got.clear()
    ws, client_got, reader = await session("e2e-cause", "go")
    to_relay = await relay.wait_for(1)
    to_sink = await sink.wait_for(1)
    before = (stats().get("lineage") or {}).get("foreign", 0)
    # sink answers, but names the delivery that was made to relay.
    stolen = dict(to_relay[0], node=to_sink[0]["node"])
    await sink.emit(stolen, "under someone else's delivery")
    await asyncio.sleep(0.3)
    lineage = stats().get("lineage") or {}
    check("an emission under a delivery made to another Expertise is counted as foreign",
          lineage.get("foreign", 0) == before + 1, str(lineage))
    out = [e for e in client_got if e.get("kind") == "data"]
    check("and it inherited no deadline from the delivery it claimed",
          len(out) == 1 and "deadline" not in out[0], str(out[:1]))
    await ws.close()
    reader.cancel()

    # ------------------------------------------------------------------- SDK
    print("\nA handler built on the SDK")
    code, body = call("POST", "/v1/graphs", graph("e2e-clock", [
        {"from": "client.text_out", "to": "clock.text_in", "deadline_ms": 30_000, "priority": 7},
        {"from": "clock.text_out", "to": "client.text_in"},
    ], ["clock"]))
    check("the graph registers", code in (200, 201), str(body))
    ws, client_got, reader = await session("e2e-clock", "what time is it")
    end = time.monotonic() + 15
    while time.monotonic() < end and not any(e.get("kind") == "data" for e in client_got):
        await asyncio.sleep(0.02)
    replies = [e for e in client_got if e.get("kind") == "data"]
    seen = json.loads(replies[0]["payload"]["text"]) if replies else {}
    check("the handler was shown its deadline",
          isinstance(seen.get("deadline"), int) and seen["deadline"] > now_ms(), str(seen))
    check("and the time left", isinstance(seen.get("remaining_ms"), int)
          and 0 < seen["remaining_ms"] <= 30_000, str(seen))
    check("and the edge's priority", seen.get("priority") == 7, str(seen))
    check("and the id of the delivery it was answering", bool(seen.get("id")), str(seen))
    await ws.close()
    reader.cancel()

    await relay.stop()
    await sink.stop()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--port", type=int, default=9397)
    args = ap.parse_args()

    binary = str(Path(args.binary).resolve())
    data = tempfile.mkdtemp(prefix="aura-deadline-")
    work = tempfile.mkdtemp(prefix="aura-deadline-clock-")
    logs = tempfile.mkdtemp(prefix="aura-deadline-logs-")
    check = Check()

    (Path(work) / "main.py").write_text(CLOCK_EXPERTISE, encoding="utf-8")
    (Path(work) / "expertise.yaml").write_text(CLOCK_MANIFEST, encoding="utf-8")

    print(f"Deadlines — {binary}")
    node = subprocess.Popen(
        [binary, "up", "--port", str(args.port), "--no-auth", "--data", data],
        stdout=open(os.path.join(logs, "node.log"), "w"), stderr=subprocess.STDOUT)
    clock = None
    try:
        start = time.monotonic()
        while True:
            try:
                with urllib.request.urlopen(f"http://localhost:{args.port}/healthz", timeout=0.5) as r:
                    if r.status == 200:
                        break
            except Exception:  # noqa: BLE001 - the node is still starting
                pass
            if time.monotonic() - start > 30:
                raise SystemExit("the node never became healthy; see " + logs)
            time.sleep(0.05)

        env = dict(os.environ, PYTHONPATH=str(ROOT / "sdk" / "python" / "src"),
                   AURA_WS_URL=f"ws://localhost:{args.port}/ws/skill", PYTHONUNBUFFERED="1")
        clock = subprocess.Popen([sys.executable, "main.py"], cwd=work, env=env,
                                 stdout=open(os.path.join(logs, "clock.log"), "w"),
                                 stderr=subprocess.STDOUT)
        start = time.monotonic()
        while time.monotonic() - start < 30:
            try:
                with urllib.request.urlopen(f"http://localhost:{args.port}/v1/skills", timeout=1) as r:
                    if b"logical.e2e.clock" in r.read():
                        break
            except Exception:  # noqa: BLE001
                pass
            time.sleep(0.05)
        else:
            raise SystemExit("the SDK Expertise never registered; see " + logs)

        asyncio.run(run(args, check))
    finally:
        for proc in (clock, node):
            if proc is not None:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
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
