"""
Traffic for the captures in docs/media: registers the two test graphs and
opens client sessions against them.

    python docs/media/src/drive.py register
    python docs/media/src/drive.py run --graph notice --sessions 1 --turns 1 --approve yes
    python docs/media/src/drive.py run --graph draft-only --sessions 12 --turns 4 --hold 20

Graphs (both resolve the test Expertise in demo_expertise.py):

  draft-only   client -> draft -> client                       (a token stream)
  notice       client -> draft -> client                       (the same stream)
                         draft.full_out -> redact -> notify    (gated motor effect)
                         notify -> client

    python docs/media/src/drive.py mcp        (a gate raised for a non-human client:
                                               shows up in the approvals inbox)

`--approve yes|no|ignore` answers the human-approval gate the kernel raises
before delivering into `motor.demo.notify`. `ignore` leaves it pending.
`--trace` sends a W3C traceparent on the first envelope of every turn.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import secrets
import time
import urllib.request

import websockets

PORT = int(os.getenv("AURA_PORT", "9480"))
BASE = f"http://localhost:{PORT}"
WS = f"ws://localhost:{PORT}"

GRAPHS = [
    {
        "ir": "1", "graph_id": "draft-only", "origin": {"kind": "declared"},
        "nodes": [{"ref": "draft", "resolve": "cognitive.demo.draft"}],
        "edges": [
            {"from": "client.text_out", "to": "draft.text_in"},
            {"from": "draft.text_out", "to": "client.text_in", "qos": "realtime"},
        ],
    },
    {
        "ir": "1", "graph_id": "notice", "origin": {"kind": "declared"},
        "nodes": [
            {"ref": "draft", "resolve": "cognitive.demo.draft"},
            {"ref": "redact", "resolve": "logical.demo.redact"},
            {"ref": "notify", "resolve": "motor.demo.notify"},
        ],
        "edges": [
            {"from": "client.text_out", "to": "draft.text_in"},
            {"from": "draft.text_out", "to": "client.text_in", "qos": "realtime"},
            {"from": "draft.full_out", "to": "redact.text_in"},
            {"from": "redact.text_out", "to": "notify.text_in", "gate": "human-approval"},
            {"from": "notify.text_out", "to": "client.text_in"},
        ],
    },
]

PROMPTS = [
    "What happened with order 48213?",
    "Summarise last night's sync.",
    "Anything on the cameras worth a handover note?",
]


def register() -> None:
    for g in GRAPHS:
        req = urllib.request.Request(
            f"{BASE}/v1/graphs", data=json.dumps(g).encode(),
            headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req) as res:
            print(g["graph_id"], res.status)


def mcp_call(tool: str, text: str) -> None:
    """Call an Expertise as an MCP tool. A motor one is held by the kernel and
    appears in GET /v1/approvals until someone answers it (or it expires)."""
    body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": tool, "arguments": {"text": text}}}
    req = urllib.request.Request(
        f"{BASE}/mcp", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=300) as res:
        print(res.read().decode()[:400])


def ulid() -> str:
    return (format(int(time.time() * 1000), "X") + secrets.token_hex(8)).upper()


async def session(graph: str, turns: int, approve: str, hold: float, trace: bool, idx: int) -> None:
    async with websockets.connect(f"{WS}/v1/stream?graph={graph}", max_size=None) as ws:
        seq = 0
        ready = False
        pending_final = 0
        want_finals = 2 if graph == "notice" and approve == "yes" else 1
        turn = 0

        async def send_turn() -> None:
            nonlocal seq, pending_final
            seq += 1
            env = {
                "v": "1", "id": ulid(), "node": "client", "port": "text_out", "seq": seq,
                "idem": f"drive:{idx}:{seq}:{secrets.token_hex(4)}", "schema": "std/text@1",
                "kind": "data", "payload": {"text": PROMPTS[(idx + turn) % len(PROMPTS)], "final": True},
            }
            if trace:
                env["trace"] = f"00-{secrets.token_hex(16)}-{secrets.token_hex(8)}-01"
            pending_final = want_finals
            await ws.send(json.dumps(env))

        async for raw in ws:
            env = json.loads(raw)
            payload = env.get("payload") or {}
            if env["kind"] == "status" and payload.get("state") == "ready" and not ready:
                ready = True
                await send_turn()
            elif env["kind"] == "confirm_request":
                if approve == "ignore":
                    continue
                await ws.send(json.dumps({
                    "v": "1", "id": ulid(), "cause_id": env["id"], "kind": "confirm_response",
                    "payload": {"approve": approve == "yes"},
                }))
                if approve == "no":
                    pending_final -= 0
            elif env["kind"] == "error":
                pending_final = 0
            elif env["kind"] == "data" and payload.get("final") is True:
                pending_final -= 1
            if ready and pending_final <= 0 and env["kind"] in ("data", "error"):
                turn += 1
                if turn >= turns:
                    break
                await send_turn()
        if hold > 0:
            await asyncio.sleep(hold)


async def run(args: argparse.Namespace) -> None:
    async def one(i: int) -> None:
        if args.ramp > 0:
            await asyncio.sleep(args.ramp * i / max(1, args.sessions))
        try:
            await asyncio.wait_for(
                session(args.graph, args.turns, args.approve, args.hold, args.trace, i),
                timeout=args.timeout)
        except (asyncio.TimeoutError, websockets.ConnectionClosed) as exc:
            print(f"session {i}: {type(exc).__name__}")

    await asyncio.gather(*(one(i) for i in range(args.sessions)))


def main() -> None:
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("register")
    r = sub.add_parser("run")
    r.add_argument("--graph", default="draft-only")
    r.add_argument("--sessions", type=int, default=1)
    r.add_argument("--turns", type=int, default=1)
    r.add_argument("--approve", choices=["yes", "no", "ignore"], default="yes")
    r.add_argument("--hold", type=float, default=0, help="keep the session open this many seconds after the last turn")
    r.add_argument("--ramp", type=float, default=0, help="stagger session starts across this many seconds")
    r.add_argument("--timeout", type=float, default=120)
    r.add_argument("--trace", action="store_true")
    m = sub.add_parser("mcp", help="call an Expertise through /mcp; blocks until the gate is answered")
    m.add_argument("--tool", default="motor_demo_notify")
    m.add_argument("--text", default="Refund notice for order 48213, held for approval.")
    args = ap.parse_args()
    if args.cmd == "register":
        register()
    elif args.cmd == "mcp":
        mcp_call(args.tool, args.text)
    else:
        asyncio.run(run(args))


if __name__ == "__main__":
    main()
