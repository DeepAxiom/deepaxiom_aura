#!/usr/bin/env python3
"""Black-box proof that a tampered effect ledger fails verification.

Runs against a real, already-running node (see the "conformance" CI job,
which starts one with --no-auth): registers an actual motor skill over the
wire, drives two effects through the real gate-approval path so the kernel
seals them for real, then edits kernel.db directly — not through any API the
node exposes, which is the point — and checks that both `aura verify` and
`GET /v1/ledger/verify` say so.

    python scripts/ledger_adversarial.py --port 9080 --data <dir> --aura-binary ./kernel/aura
"""
from __future__ import annotations

import argparse
import asyncio
import json
import sqlite3
import subprocess
import sys
import urllib.request

import websockets

CAPABILITY = "motor.ledgertest.write"
GRAPH_ID = "ledger-adversarial-check"


def check(name: str, ok: bool, detail: str = "") -> None:
    print(f"  {'ok' if ok else 'FAIL':4} {name}")
    if not ok:
        if detail:
            print(f"       {detail}")
        sys.exit(1)


def http_json(method: str, url: str, body: dict | None = None) -> tuple[int, dict]:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                  headers={"Content-Type": "application/json"} if data else {})
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


async def seal_two_effects(port: int) -> None:
    """Register a real motor skill, wire a graph to it, and drive TWO
    messages through the human-approval gate, confirming each delivered
    envelope carries a receipt — i.e. the kernel actually sealed them, not
    just logged them.

    Two, not one: tampering a lone entry with nothing sealed after it and no
    checkpoint yet is — correctly — undetectable, the same way editing the
    last line of an as-yet-unwitnessed ledger book would be. The interior
    hash-chain check (entry N+1 disagreeing with entry N) is what this script
    exercises; sealing a second effect is what gives it something to disagree
    with. See kernel/internal/ledger's own test suite for the checkpoint-only
    case, where the *last* entry is the one tampered.
    """
    manifest = {
        "id": "acme/motor/ledger-test", "version": "1.0.0", "protocol": "1",
        "name": "Ledger adversarial test writer",
        "description": "exists only to give the adversarial check a real motor effect to seal",
        "capability": CAPABILITY, "type": "motor", "format": "source",
        "ports": {
            "ingress": [{"name": "text_in", "schema": "std/text@1"}],
            "egress": [{"name": "text_out", "schema": "std/text@1"}],
        },
    }

    async with websockets.connect(f"ws://localhost:{port}/ws/skill") as skill:
        await skill.send(json.dumps({"v": "1", "id": "reg-1", "kind": "register", "payload": manifest}))
        ack = json.loads(await skill.recv())
        check("the test skill registered", ack.get("kind") == "status", str(ack))

        graph = {
            "ir": "1", "graph_id": GRAPH_ID, "origin": {"kind": "declared"},
            "nodes": [{"ref": "w", "resolve": CAPABILITY}],
            "edges": [{"from": "client.text_out", "to": "w.text_in"}],
        }
        status, _ = http_json("POST", f"http://localhost:{port}/v1/graphs", graph)
        check("the test graph registered", status == 201, f"status {status}")

        async with websockets.connect(f"ws://localhost:{port}/v1/stream?graph={GRAPH_ID}") as client:
            hello = json.loads(await client.recv())
            check("the session opened", hello.get("payload", {}).get("state") == "ready", str(hello))

            for i in (1, 2):
                await client.send(json.dumps({"text": f"seal effect {i}"}))

                # The default policy gates motor.* with no matching rule, so
                # this has to go through approval — exercising the exact path
                # Session.resolveGate / sealDenial-or-forward runs in production.
                gate = json.loads(await client.recv())
                check(f"effect {i}: a human-approval gate was requested",
                      gate.get("kind") == "confirm_request", str(gate))

                await client.send(json.dumps({
                    "v": "1", "id": f"resp-{i}", "cause_id": gate["id"],
                    "kind": "confirm_response", "payload": {"approve": True},
                }))

                delivered = json.loads(await skill.recv())
                check(f"effect {i}: the approved effect reached the skill",
                      delivered.get("kind") == "data", str(delivered))
                check(f"effect {i}: the delivered envelope carries a ledger receipt",
                      bool(delivered.get("receipt")), str(delivered))


def run_aura_verify(binary: str, data_dir: str) -> subprocess.CompletedProcess:
    return subprocess.run([binary, "verify", "--data", data_dir],
                           capture_output=True, text=True)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9080)
    ap.add_argument("--data", required=True, help="the running node's --data directory")
    ap.add_argument("--aura-binary", required=True)
    args = ap.parse_args()

    print("ledger adversarial check:")
    asyncio.run(seal_two_effects(args.port))

    before = run_aura_verify(args.aura_binary, args.data)
    check("aura verify is sound before tampering", before.returncode == 0,
          before.stdout + before.stderr)

    status, body = http_json("GET", f"http://localhost:{args.port}/v1/ledger/verify")
    check("GET /v1/ledger/verify reports sound before tampering",
          status == 200 and body.get("sound") is True, f"status {status}: {body}")

    # Tamper directly against the SQLite file — bypassing every API the node
    # exposes, which is exactly the threat model this whole feature exists for.
    db = sqlite3.connect(f"{args.data}/kernel.db")
    db.execute(
        "UPDATE ledger_entries SET entry = REPLACE(entry, ?, 'motor.payments.send') WHERE seq = 1",
        (CAPABILITY,),
    )
    db.commit()
    db.close()

    after = run_aura_verify(args.aura_binary, args.data)
    check("aura verify fails after tampering", after.returncode != 0,
          "expected a nonzero exit code")
    check("the report says NOT SOUND", "NOT SOUND" in after.stdout, after.stdout)

    status, body = http_json("GET", f"http://localhost:{args.port}/v1/ledger/verify")
    check("GET /v1/ledger/verify reports unsound after tampering",
          status == 409 and body.get("sound") is False, f"status {status}: {body}")

    print("ledger adversarial check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
