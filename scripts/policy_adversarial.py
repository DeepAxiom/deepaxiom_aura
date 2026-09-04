#!/usr/bin/env python3
"""Check that a graph cannot talk its way past a policy that denies it.

`gate: none` on an edge is a graph author saying "this effect needs no human".
The node's policy is its operator saying which effects are not to happen at all.
When those disagree the operator wins, and the claim this script exercises is
that the disagreement is settled *before the effect reaches the skill that would
perform it* — not by an error message after the fact.

    python scripts/policy_adversarial.py --port 9141 --data <dir> --aura-binary ./kernel/aura

The load-bearing part is that a real motor skill is connected. Without one, a
node refuses the same graph with "no connected skill provides that capability" —
which is what a node with NO policy would answer too, so a check that accepts
that refusal proves nothing about the policy. That is exactly what
`adversarial.sh` used to do here, over a socket curl cannot read.

Sequence:

  1. start a node whose policy denies `motor.payments.*`
  2. connect a real skill that provides `motor.payments.send`
  3. register a graph that waives the gate on it — a well-formed document
  4. open a session and send input
  5. require: the client is told the POLICY refused it, and the skill is
     handed nothing at all

Step 5 is the whole point. Everything before it is setup.
"""

import argparse
import asyncio
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

import websockets

FAILURES = []

CAPABILITY = "motor.payments.send"

MANIFEST = {
    "id": "probe/motor/payments",
    "version": "1.0.0",
    "protocol": "1",
    "name": "Payments probe",
    "description": "A motor skill that exists so a denied effect has somewhere to be delivered, and must never receive one.",
    "capability": CAPABILITY,
    "type": "motor",
    "format": "source",
    "runtime": {"language": "python", "version": ">=3.11"},
    "ports": {
        "ingress": [{"name": "text_in", "schema": "std/text@1"}],
        "egress": [{"name": "text_out", "schema": "std/text@1"}],
    },
}

GRAPH = {
    "ir": "1",
    "graph_id": "waiver",
    "origin": {"kind": "declared"},
    "nodes": [{"ref": "w", "resolve": CAPABILITY}],
    "edges": [{"from": "client.text_out", "to": "w.text_in", "gate": "none"}],
}

POLICY = """policy: 1
default_effect: gate
rules:
  - match: "motor.payments.*"
    decision: deny
    reason: "no automated payments on this node"
"""


def ok(msg):
    print(f"  ok   {msg}")


def fail(msg):
    print(f"  FAIL {msg}", file=sys.stderr)
    FAILURES.append(msg)


def wait_healthy(port, tries=60):
    for _ in range(tries):
        try:
            urllib.request.urlopen(f"http://localhost:{port}/healthz", timeout=2).read()
            return True
        except Exception:
            time.sleep(1)
    return False


def post_graph(port, token):
    req = urllib.request.Request(
        f"http://localhost:{port}/v1/graphs",
        method="POST",
        data=json.dumps(GRAPH).encode(),
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()


async def exercise(port, token):
    """Connect the skill, run the session, and report what each side saw."""
    auth = {"Authorization": f"Bearer {token}"}
    delivered = []

    skill = await websockets.connect(f"ws://localhost:{port}/ws/skill", additional_headers=auth)
    try:
        await skill.send(json.dumps({"v": "1", "id": "reg1", "kind": "register", "payload": MANIFEST}))
        ack = json.loads(await asyncio.wait_for(skill.recv(), timeout=10))
        if ack.get("kind") != "status" or ack.get("payload", {}).get("state") != "registered":
            fail(f"the probe skill could not register: {ack}")
            return None, delivered
        ok("a real motor skill provides the denied capability")

        async def drain():
            try:
                while True:
                    delivered.append(await skill.recv())
            except Exception:
                pass

        drainer = asyncio.create_task(drain())

        # A graph that waives the gate is still a well-formed document, so it
        # registers. Registration is not the enforcement point.
        code, body = post_graph(port, token)
        if code != 201:
            fail(f"registering a well-formed graph answered {code}: {body}")
            drainer.cancel()
            return None, delivered
        ok("the node accepts a gate:none graph as a document")

        refusal = None
        try:
            async with websockets.connect(
                f"ws://localhost:{port}/v1/stream?graph=waiver", additional_headers=auth
            ) as client:
                await client.send(json.dumps({
                    "v": "1", "id": "in1", "kind": "data", "session": "s1",
                    "node": "client", "port": "text_out", "seq": 1,
                    "idem": "s1:client:text_out:1", "schema": "std/text@1",
                    "payload": {"text": "pay 500"},
                }))
                while True:
                    frame = json.loads(await asyncio.wait_for(client.recv(), timeout=8))
                    if frame.get("kind") == "error":
                        refusal = frame
                        break
        except asyncio.TimeoutError:
            fail("the session neither refused nor answered within 8s")
        except websockets.exceptions.WebSocketException as exc:
            # A closed socket with no envelope is a refusal too, but a silent
            # one: nothing downstream can tell it from a network fault.
            if refusal is None:
                fail(f"the session closed without saying why: {exc}")

        # Give anything wrongly routed time to arrive before declaring it did not.
        await asyncio.sleep(1)
        drainer.cancel()
        return refusal, delivered
    finally:
        await skill.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9141)
    ap.add_argument("--data", required=True)
    ap.add_argument("--aura-binary", required=True)
    args = ap.parse_args()

    binary = os.path.abspath(args.aura_binary)
    data = os.path.abspath(args.data)
    shutil.rmtree(data, ignore_errors=True)
    os.makedirs(data, exist_ok=True)
    policy_path = os.path.join(data, "aura.policy.yaml")
    with open(policy_path, "w", encoding="utf-8") as fh:
        fh.write(POLICY)

    print("policy adversarial check:")
    node = subprocess.Popen(
        [binary, "up", "--port", str(args.port), "--data", os.path.join(data, "node"),
         "--policy", policy_path],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not wait_healthy(args.port):
            raise SystemExit("the node never became healthy")
        with open(os.path.join(data, "node", "node.token"), encoding="utf-8") as fh:
            token = fh.read().strip()

        refusal, delivered = asyncio.run(exercise(args.port, token))

        if refusal is None:
            fail("the client was never told the effect was refused")
        else:
            detail = refusal.get("payload", {}).get("detail", "")
            # The refusal must name the policy. "no connected skill provides
            # that capability" is the answer a node with no policy at all gives,
            # and accepting it here would make this whole script vacuous.
            if "policy denies" not in detail:
                fail(f"the refusal does not cite the policy: {detail!r}")
            else:
                ok(f"the client is told the policy refused it: {detail}")
            if "no automated payments" not in detail:
                fail("the refusal does not carry the operator's own reason")
            else:
                ok("and carries the reason the operator wrote")

        # The one that matters. An error envelope proves the client was told;
        # only this proves the effect did not happen.
        if delivered:
            fail(f"the denied effect reached the motor skill: {delivered!r}")
        else:
            ok("nothing at all reached the motor skill")
    finally:
        node.terminate()
        try:
            node.wait(timeout=15)
        except subprocess.TimeoutExpired:
            node.kill()

    if FAILURES:
        print(f"\n{len(FAILURES)} failure(s).", file=sys.stderr)
        return 1
    print("\npolicy adversarial: a deny is not negotiable.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
