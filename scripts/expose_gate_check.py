#!/usr/bin/env python3
"""Check the integration promise a full-stack app is sold.

An app announces two functions with `aura.expose()`. One is marked
`write: true`. The claim is that from that single flag, the kernel gates the
call and seals the effect — with no gate-handling code in the app, and even in a
graph whose author never asked for one.

This registers exactly such a graph (no gate on the edge), drives the write,
and requires the kernel to hold it. A run where the refund happens without a
`confirm_request` is the failure this exists to catch, because it would mean the
promise holds only for graphs that remember to ask.

    python3 scripts/expose_gate_check.py --port 9181
"""
from __future__ import annotations

import argparse
import asyncio
import json
import sys
import time
import urllib.error
import urllib.request

CAPABILITY = "motor.api.shop.refund_order"


def fail(msg: str) -> None:
    print(f"  FAIL {msg}")
    sys.exit(1)


def register_graph(port: int, graph_id: str) -> None:
    graph = {
        "ir": "1",
        "graph_id": graph_id,
        "origin": {"kind": "declared"},
        "nodes": [{"ref": "r", "resolve": CAPABILITY}],
        # No gate declared anywhere. That is the point.
        "edges": [
            {"from": "client.request_out", "to": "r.request_in"},
            {"from": "r.response_out", "to": "client.response_in"},
        ],
    }
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}/v1/graphs",
        data=json.dumps(graph).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            if resp.status != 201:
                fail(f"graph registration returned {resp.status}")
    except urllib.error.HTTPError as e:
        fail(f"graph registration returned {e.code}: {e.read().decode()[:200]}")


async def drive(port: int, graph_id: str) -> None:
    import websockets

    url = f"ws://127.0.0.1:{port}/v1/stream?graph={graph_id}"
    gated = False
    async with websockets.connect(url, open_timeout=15) as ws:
        session = ""
        seq = 0
        deadline = time.time() + 30
        while time.time() < deadline:
            env = json.loads(await asyncio.wait_for(ws.recv(), timeout=20))
            payload = env.get("payload") or {}
            kind = env.get("kind")

            if kind == "status" and payload.get("state") == "ready":
                session = payload["session"]
                seq += 1
                await ws.send(json.dumps({
                    "v": "1", "id": f"req-{seq}", "session": session,
                    "node": "client", "port": "request_out", "seq": seq,
                    "idem": f"{session}:{seq}",
                    "schema": "std/api-request@1", "kind": "data",
                    "payload": {"op": "refund-order", "params": {"id": "1002"}},
                }))
                continue

            if kind == "confirm_request":
                gated = True
                print("  ok   the kernel held a write on an edge that declared no gate")
                await ws.send(json.dumps({
                    "v": "1", "id": f"ack-{seq}", "cause_id": env["id"],
                    "session": session, "node": "client", "port": "confirm",
                    "kind": "confirm_response", "payload": {"approve": True},
                }))
                continue

            if kind == "data" and env.get("node") == "client":
                if not gated:
                    fail("the refund completed with no gate — `write: true` bought nothing")
                body = payload.get("body") or {}
                if body.get("refunded") != "1002":
                    fail(f"the app's own function did not run: {payload}")
                print("  ok   the app's real function ran, only after approval")
                return

            if kind == "error":
                fail(f"kernel error: {payload}")

    fail("timed out before the effect completed")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    args = ap.parse_args()

    graph_id = f"expose-gate-{int(time.time())}"
    register_graph(args.port, graph_id)
    asyncio.run(drive(args.port, graph_id))
    print("expose gate check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
