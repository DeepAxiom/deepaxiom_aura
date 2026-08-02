"""
Conformance suite for the C1/C2/C3 contracts.

Black box: speaks the RAW protocol (WS + HTTP) against a live kernel,
without using any SDK. Any kernel that passes this suite is conformant.

Usage:
    # with a kernel running (clean data or not — ids are unique per run):
    python runner.py --port 9080

Dependencies: websockets (required), jsonschema (optional, validates vectors).
"""
from __future__ import annotations

import argparse
import asyncio
import json
import secrets
import sys
import urllib.request
from pathlib import Path

import websockets

# Windows consoles on cp1252: never crash over a decorative character.
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = Path(__file__).parent
SCHEMAS = HERE.parent / "schemas"
VECTORS = HERE / "vectors"

PASS, FAIL = [], []


def check(name: str, ok: bool, detail: str = "") -> None:
    (PASS if ok else FAIL).append(name)
    mark = "PASS" if ok else "FAIL"
    line = f"  [{mark}] {name}"
    if detail and not ok:
        line += f" — {detail}"
    print(line)


def new_id() -> str:
    return secrets.token_hex(13).upper()


def http(base: str, method: str, path: str, body: dict | None = None) -> tuple[int, dict]:
    req = urllib.request.Request(base + path, method=method)
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, data=data, timeout=10) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:  # noqa: BLE001
            return e.code, {}


def manifest(cap_suffix: str, *, ingress_schema="std/text@1",
             egress_schema="std/text@1", protocol="1") -> dict:
    return {
        "id": f"conformance/logical/{cap_suffix.replace('_', '-')}",
        "version": "1.0.0",
        "protocol": protocol,
        "name": f"Conformance {cap_suffix}",
        "description": "Synthetic skill from the conformance suite.",
        "capability": f"logical.{cap_suffix}",
        "type": "logical",
        "format": "source",
        "ports": {
            "ingress": [{"name": "text_in", "schema": ingress_schema}],
            "egress": [{"name": "text_out", "schema": egress_schema}],
        },
    }


class FakeSkill:
    """Synthetic skill speaking raw C3, echoing with a transformation."""

    def __init__(self, ws_url: str, mani: dict, behavior: str = "echo") -> None:
        self.ws_url, self.mani, self.behavior = ws_url, mani, behavior
        self.received: list[dict] = []
        self.cancels: list[dict] = []
        self.ack: dict | None = None
        self._task: asyncio.Task | None = None
        self._registered = asyncio.Event()

    async def __aenter__(self):
        self._task = asyncio.create_task(self._run())
        await asyncio.wait_for(self._registered.wait(), 10)
        return self

    async def __aexit__(self, *_):
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass

    async def _run(self):
        async with websockets.connect(self.ws_url) as ws:
            await ws.send(json.dumps({
                "v": "1", "id": new_id(), "kind": "register", "payload": self.mani}))
            self.ack = json.loads(await ws.recv())
            self._registered.set()
            seq = 0
            async for raw in ws:
                env = json.loads(raw)
                if env.get("kind") == "cancel":
                    # Recorded but deliberately NOT acted on: C3 lets a skill
                    # ignore cancel, and the kernel's suppression must hold
                    # anyway.
                    self.cancels.append(env)
                    continue
                if env.get("kind") != "data":
                    continue
                self.received.append(env)
                text = (env.get("payload") or {}).get("text", "")
                if self.behavior == "slow":
                    await asyncio.sleep(0.7)  # long enough for a cancel to land
                seq += 1
                reply = {
                    "v": "1", "id": new_id(), "cause_id": env["id"],
                    "session": env["session"], "node": env["node"],
                    "port": "text_out", "seq": seq,
                    "idem": f"{env['idem']}:{env['node']}:text_out:{seq}",
                    "schema": "std/text@1", "kind": "data",
                    "payload": {"text": f"eco:{text}", "final": True},
                }
                if self.behavior == "duplicate":
                    await ws.send(json.dumps(reply))
                    await ws.send(json.dumps(reply))  # same idem → must dedup
                else:
                    await ws.send(json.dumps(reply))


async def register_expect_error(ws_url: str, payload, raw_kind="register") -> dict:
    async with websockets.connect(ws_url) as ws:
        await ws.send(json.dumps({"v": "1", "id": new_id(), "kind": raw_kind,
                                  "payload": payload}))
        return json.loads(await asyncio.wait_for(ws.recv(), 10))


async def client_recv_until(ws, kinds: set[str], timeout=15) -> dict:
    while True:
        env = json.loads(await asyncio.wait_for(ws.recv(), timeout))
        if env.get("kind") in kinds:
            return env


def graph(gid: str, cap: str, gate: str | None = None) -> dict:
    edge = {"from": "eco.text_out", "to": "client.text_in"}
    if gate:
        edge["gate"] = gate
    return {
        "ir": "1", "graph_id": gid, "origin": {"kind": "declared"},
        "nodes": [{"ref": "eco", "resolve": cap}],
        "edges": [{"from": "client.text_out", "to": "eco.text_in"}, edge],
    }


def chain_graph(gid: str, first: str, second: str) -> dict:
    """client -> a -> b -> client: the shape that distinguishes a cancel that
    walks the chain from one that stops at the first hop."""
    return {
        "ir": "1", "graph_id": gid, "origin": {"kind": "declared"},
        "nodes": [{"ref": "a", "resolve": first}, {"ref": "b", "resolve": second}],
        "edges": [
            {"from": "client.text_out", "to": "a.text_in"},
            {"from": "a.text_out", "to": "b.text_in"},
            {"from": "b.text_out", "to": "client.text_in"},
        ],
    }


# ─────────────────────────── sections ────────────────────────────

def section_vectors():
    print("\n■ Vectors against JSON Schemas (C1/C2/C3)")
    try:
        import jsonschema
    except ImportError:
        print("  [SKIP] jsonschema not installed (pip install jsonschema)")
        return
    schema_for = {
        "manifest": json.loads((SCHEMAS / "manifest.schema.json").read_text(encoding="utf-8")),
        "graph-ir": json.loads((SCHEMAS / "graph-ir.schema.json").read_text(encoding="utf-8")),
        "envelope": json.loads((SCHEMAS / "envelope.schema.json").read_text(encoding="utf-8")),
    }
    for kind, schema in schema_for.items():
        for vec in sorted((VECTORS / kind).glob("*.json")):
            doc = json.loads(vec.read_text(encoding="utf-8"))
            errors = list(jsonschema.Draft202012Validator(schema).iter_errors(doc))
            # valid-* and semantic-invalid-* are well-formed per the schema;
            # semantic-* must be rejected by implementations (section C2).
            expect_valid = vec.name.startswith(("valid", "semantic"))
            ok = (not errors) if expect_valid else bool(errors)
            check(f"vector {kind}/{vec.name}", ok,
                  errors[0].message if errors and expect_valid else "accepted an invalid document")

    section_std_vectors(jsonschema)


def section_std_vectors(jsonschema):
    """The `std` payload namespace every port declares a schema from.

    Until these existed, `schema: "std/text@1"` was a string the kernel
    checked the *format* of and nothing more — the shapes lived only in prose,
    so no implementation could actually be held to them.
    """
    print("\n■ Vectors against the std payload schemas (C1)")
    std_dir = SCHEMAS / "std"
    if not std_dir.is_dir():
        check("std schemas exist", False, "spec/schemas/std/ is missing")
        return

    schemas = {p.name.removesuffix(".schema.json"): json.loads(p.read_text(encoding="utf-8"))
               for p in sorted(std_dir.glob("*.schema.json"))}

    for vec in sorted((VECTORS / "std").glob("*.json")):
        stem = vec.name.removesuffix(".json")
        body = stem.removeprefix("valid-").removeprefix("invalid-")
        # A vector is named <valid|invalid>-<schema>[-detail]; the longest
        # matching schema name wins, so "audio-chunk-final" resolves to
        # "audio-chunk" and not to a schema that does not exist.
        name = max((s for s in schemas if body == s or body.startswith(s + "-")),
                   key=len, default=None)
        if name is None:
            check(f"vector std/{vec.name}", False, "names no known std schema")
            continue

        doc = json.loads(vec.read_text(encoding="utf-8"))
        errors = list(jsonschema.Draft202012Validator(schemas[name]).iter_errors(doc))
        expect_valid = stem.startswith("valid")
        ok = (not errors) if expect_valid else bool(errors)
        check(f"vector std/{vec.name}", ok,
              errors[0].message if errors and expect_valid else "accepted an invalid payload")


async def section_kernel(base: str, ws_skill: str, ws_client_base: str):
    run = secrets.token_hex(3)  # unique ids per run

    print("\n■ Health and negotiation")
    code, health = http(base, "GET", "/healthz")
    check("healthz responds", code == 200 and health.get("ok") is True)
    check("healthz declares protocol=1 and ir=1",
          health.get("protocol") == "1" and health.get("ir") == "1")

    print("\n■ C1 — manifest registration")
    bad = manifest(f"bad_{run}")
    del bad["description"]
    resp = await register_expect_error(ws_skill, bad)
    check("manifest without description → error", resp.get("kind") == "error")

    resp = await register_expect_error(ws_skill, manifest(f"proto_{run}", protocol="99"))
    check("unknown protocol major → error", resp.get("kind") == "error")

    resp = await register_expect_error(
        ws_skill, {**manifest(f"noschema_{run}"),
                   "ports": {"ingress": [{"name": "text_in"}], "egress": []}})
    check("port without schema → error", resp.get("kind") == "error")

    async with FakeSkill(ws_skill, manifest(f"ok_{run}")) as sk:
        check("valid manifest → status registered",
              sk.ack and sk.ack.get("kind") == "status")
        _, skills = http(base, "GET", "/v1/skills")
        check("skill appears in /v1/skills",
              any(s["id"] == sk.mani["id"] for s in (skills or [])))

    print("\n■ C2 — IR validation")
    for name in ("semantic-invalid-duplicate-ref", "semantic-invalid-unknown-edge-ref", "invalid-bad-major"):
        doc = json.loads((VECTORS / "graph-ir" / f"{name}.json").read_text(encoding="utf-8"))
        doc["graph_id"] += f"-{run}"
        code, _ = http(base, "POST", "/v1/graphs", doc)
        check(f"IR {name} → 4xx", 400 <= code < 500, f"got {code}")
    code, _ = http(base, "POST", "/v1/graphs", graph(f"conf-{run}", f"logical.ok_{run}"))
    check("valid IR → 201", code == 201, f"got {code}")
    _, graphs = http(base, "GET", "/v1/graphs")
    check("graph appears in /v1/graphs", f"conf-{run}" in (graphs.get("graphs") or []))

    print("\n■ C3 — routing, causality, ordering, dedup")
    async with FakeSkill(ws_skill, manifest(f"ok_{run}")) as sk:
        async with websockets.connect(f"{ws_client_base}?graph=conf-{run}") as ws:
            hello = await client_recv_until(ws, {"status", "error"})
            check("client receives status ready", hello["kind"] == "status")
            session = (hello.get("payload") or {}).get("session", "")

            await ws.send(json.dumps({"text": "hola"}))
            reply = await client_recv_until(ws, {"data", "error"})
            check("round-trip returns data", reply["kind"] == "data")
            check("payload transformed by the skill",
                  (reply.get("payload") or {}).get("text") == "eco:hola")
            check("cause_id present in the reply", bool(reply.get("cause_id")))
            check("correct session in the reply", reply.get("session") == session)
            check("client destination labeled node=client, port=text_in",
                  reply.get("node") == "client" and reply.get("port") == "text_in")

            delivered = sk.received[0]
            check("skill received a complete envelope (idem, seq, schema, session)",
                  all(delivered.get(k) for k in ("idem", "schema", "session"))
                  and delivered.get("seq", 0) >= 1)
            check("skill received its ref and ingress port",
                  delivered.get("node") == "eco" and delivered.get("port") == "text_in")

            # FIFO: 5 mensajes en orden
            for i in range(5):
                await ws.send(json.dumps({"text": f"m{i}"}))
            got = [await client_recv_until(ws, {"data"}) for _ in range(5)]
            texts = [(g.get("payload") or {}).get("text") for g in got]
            check("FIFO: 5 replies in order", texts == [f"eco:m{i}" for i in range(5)],
                  str(texts))

            # log causal
            code, log = http(base, "GET", f"/v1/sessions/{session}/events")
            events = log.get("events") or []
            ids = {e.get("id") for e in events}
            causes_ok = all((not e.get("cause_id")) or (e["cause_id"] in ids)
                            for e in events)
            check("event log persists the session", code == 200 and len(events) >= 4)
            check("causal closure: every cause_id exists in the log", causes_ok)

    print("\n■ C3 — at-least-once dedup")
    dup_cap = f"dup_{run}"
    code, _ = http(base, "POST", "/v1/graphs", graph(f"dup-{run}", f"logical.{dup_cap}"))
    async with FakeSkill(ws_skill, manifest(dup_cap), behavior="duplicate"):
        async with websockets.connect(f"{ws_client_base}?graph=dup-{run}") as ws:
            await client_recv_until(ws, {"status"})
            await ws.send(json.dumps({"text": "x"}))
            first = await client_recv_until(ws, {"data"})
            try:
                second = await asyncio.wait_for(ws.recv(), 2)
                got_dup = json.loads(second).get("kind") == "data"
            except asyncio.TimeoutError:
                got_dup = False
            check("duplicate emission (same idem) delivered ONCE",
                  first["kind"] == "data" and not got_dup)

    print("\n■ Resolution and schema compatibility")
    code, _ = http(base, "POST", "/v1/graphs", graph(f"missing-{run}", "logical.no_existe"))
    async with websockets.connect(f"{ws_client_base}?graph=missing-{run}") as ws:
        env = await client_recv_until(ws, {"error", "status"})
        check("unconnected capability → explained error on connect",
              env["kind"] == "error")

    # mismatch: egress std/text@1 → ingress otro/formato@1
    a_cap, b_cap = f"mma_{run}", f"mmb_{run}"
    b_mani = manifest(b_cap, ingress_schema="otro/formato@1")
    mismatch_graph = {
        "ir": "1", "graph_id": f"mm-{run}", "origin": {"kind": "declared"},
        "nodes": [{"ref": "a", "resolve": f"logical.{a_cap}"},
                  {"ref": "b", "resolve": f"logical.{b_cap}"}],
        "edges": [{"from": "client.text_out", "to": "a.text_in"},
                  {"from": "a.text_out", "to": "b.text_in"}],
    }
    http(base, "POST", "/v1/graphs", mismatch_graph)
    async with FakeSkill(ws_skill, manifest(a_cap)), FakeSkill(ws_skill, b_mani):
        async with websockets.connect(f"{ws_client_base}?graph=mm-{run}") as ws:
            env = await client_recv_until(ws, {"error", "status"})
            check("incompatible schemas on an edge → session rejected",
                  env["kind"] == "error")

    print("\n■ C2 — human-approval gate")
    g_cap = f"gate_{run}"
    http(base, "POST", "/v1/graphs", graph(f"gate-{run}", f"logical.{g_cap}",
                                           gate="human-approval"))
    async with FakeSkill(ws_skill, manifest(g_cap)):
        # approve
        async with websockets.connect(f"{ws_client_base}?graph=gate-{run}") as ws:
            await client_recv_until(ws, {"status"})
            await ws.send(json.dumps({"text": "aprueba esto"}))
            req = await client_recv_until(ws, {"confirm_request"})
            check("gate emits confirm_request", req["kind"] == "confirm_request")
            await ws.send(json.dumps({"v": "1", "id": new_id(), "cause_id": req["id"],
                                      "kind": "confirm_response",
                                      "payload": {"approve": True}}))
            env = await client_recv_until(ws, {"data", "error"})
            check("approve → the held message is delivered", env["kind"] == "data")
        # deny
        async with websockets.connect(f"{ws_client_base}?graph=gate-{run}") as ws:
            await client_recv_until(ws, {"status"})
            await ws.send(json.dumps({"text": "deniega esto"}))
            req = await client_recv_until(ws, {"confirm_request"})
            await ws.send(json.dumps({"v": "1", "id": new_id(), "cause_id": req["id"],
                                      "kind": "confirm_response",
                                      "payload": {"approve": False}}))
            env = await client_recv_until(ws, {"data", "error"})
            check("deny → explained error, no delivery", env["kind"] == "error")


async def section_cancel(base: str, ws_skill: str, ws_client_base: str):
    """C3 v1.2 cancel: whole-chain, addressed per skill, kernel-suppressed."""
    print("\n■ C3 — cancel abandons the whole chain")
    run = new_id()[:6].lower()
    a_cap, b_cap = f"ca-{run}", f"cb-{run}"

    async with FakeSkill(ws_skill, manifest(a_cap)) as a, \
            FakeSkill(ws_skill, manifest(b_cap), behavior="slow") as b:
        code, _ = http(base, "POST", "/v1/graphs",
                       chain_graph(f"cancel-{run}", f"logical.{a_cap}", f"logical.{b_cap}"))
        check("chain graph registered", code == 201, f"got {code}")

        async with websockets.connect(f"{ws_client_base}?graph=cancel-{run}") as ws:
            await client_recv_until(ws, {"status"})
            msg_id = new_id()
            await ws.send(json.dumps({
                "v": "1", "id": msg_id, "node": "client", "port": "text_out",
                "seq": 1, "idem": f"cancel-{run}:1", "schema": "std/text@1",
                "kind": "data", "payload": {"text": "long answer", "final": True}}))

            # Wait until the SECOND skill is actually working, so the cancel has
            # somewhere past the first hop to reach.
            for _ in range(50):
                if b.received:
                    break
                await asyncio.sleep(0.05)
            check("chain reached the second skill", bool(b.received))

            await ws.send(json.dumps({
                "v": "1", "id": new_id(), "cause_id": msg_id, "kind": "cancel"}))

            # Propagation: the second skill must be told, in its own terms.
            for _ in range(60):
                if b.cancels:
                    break
                await asyncio.sleep(0.05)
            check("cancel reaches a skill past the first hop", bool(b.cancels),
                  "only the first hop was told — cancel did not walk the chain")
            if b.cancels and b.received:
                check("cancel carries the cause_id that skill recognises",
                      b.cancels[0].get("cause_id") == b.received[0].get("cause_id"),
                      "the skill would ignore a cancel naming an id it never saw")

            # Suppression: the skill ignores cancel and replies anyway.
            got_data = False
            try:
                env = await asyncio.wait_for(
                    client_recv_until(ws, {"data", "error"}), timeout=2.5)
                got_data = env.get("kind") == "data"
            except (asyncio.TimeoutError, Exception):  # noqa: BLE001
                pass
            check("nothing from a cancelled chain reaches the client", not got_data,
                  "the kernel forwarded output produced after the cancel")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9080)
    ap.add_argument("--host", default="localhost")
    args = ap.parse_args()
    base = f"http://{args.host}:{args.port}"
    ws_skill = f"ws://{args.host}:{args.port}/ws/skill"
    ws_client = f"ws://{args.host}:{args.port}/v1/stream"

    print(f"Conformance suite — kernel {base}")
    section_vectors()
    asyncio.run(section_kernel(base, ws_skill, ws_client))
    asyncio.run(section_cancel(base, ws_skill, ws_client))

    print(f"\n{'─' * 50}\n  {len(PASS)} PASS · {len(FAIL)} FAIL")
    if FAIL:
        print("  Failures:", *[f"\n    · {f}" for f in FAIL])
        return 1
    print("  Kernel is CONFORMANT with C1/C2/C3 v1")
    return 0


if __name__ == "__main__":
    sys.exit(main())
