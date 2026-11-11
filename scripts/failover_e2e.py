#!/usr/bin/env python3
"""Kill a live node and require its standby to take over.

Two real binaries on one data directory: a leader (`aura up`) and a standby
(`aura up --standby`). A real Expertise is connected and a session is in
progress. The leader is SIGKILLed — no clean shutdown, no lease released —
and this checks what an operator would:

  - the standby answers on the same port, and how long the gap was;
  - it is the same node (same identity, same ledger), not a fresh one;
  - the Expertise reconnects on its own;
  - the *same session* continues, and its causal log holds what happened on
    both sides of the kill;
  - the ledger still verifies offline afterwards.

It is a script rather than a Go test because every one of those is a property
of processes: a unit test cannot kill the process it is running in.

    python scripts/failover_e2e.py ./kernel/aura [--port 9396] [--max-outage-ms 3000]

Needs: websockets, pyyaml (for the echo Expertise).
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
import time
import urllib.request
from pathlib import Path

import websockets

ROOT = Path(__file__).resolve().parent.parent


def expertise_dir() -> Path:
    for name in ("expertise", "skills"):
        candidate = ROOT / name / "echo"
        if candidate.is_dir():
            return candidate
    raise SystemExit("no echo Expertise found under expertise/ (or skills/ in an older checkout)")


class Check:
    def __init__(self) -> None:
        self.failed: list[str] = []

    def __call__(self, name: str, ok: bool, detail: str = "") -> None:
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail and not ok else ""))
        if not ok:
            self.failed.append(name)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--port", type=int, default=9396)
    ap.add_argument("--max-outage-ms", type=int, default=3000,
                    help="fail if the standby takes longer than this to answer")
    args = ap.parse_args()

    binary = str(Path(args.binary).resolve())
    base = f"http://localhost:{args.port}"
    data = tempfile.mkdtemp(prefix="aura-failover-")
    logs = tempfile.mkdtemp(prefix="aura-failover-logs-")
    check = Check()

    def get(path: str, timeout: float = 1.0):
        try:
            with urllib.request.urlopen(base + path, timeout=timeout) as r:
                return r.status, r.read()
        except Exception:  # noqa: BLE001 - a node that is down is the point
            return 0, b""

    def node(extra: list[str], name: str) -> subprocess.Popen:
        return subprocess.Popen(
            [binary, "up", "--port", str(args.port), "--no-auth", "--data", data, *extra],
            stdout=open(os.path.join(logs, name), "w"), stderr=subprocess.STDOUT)

    def wait_healthy(limit: float = 30.0) -> dict:
        start = time.perf_counter()
        while time.perf_counter() - start < limit:
            code, body = get("/healthz", 0.5)
            if code == 200:
                return json.loads(body)
            time.sleep(0.005)
        raise SystemExit("the node never became healthy")

    def wait_expertise(limit: float = 30.0) -> bool:
        start = time.perf_counter()
        while time.perf_counter() - start < limit:
            code, body = get("/v1/skills", 1.0)
            if code == 200 and b"logical.echo" in body:
                return True
            time.sleep(0.05)
        return False

    async def ask(session: str, text: str) -> str:
        url = f"ws://localhost:{args.port}/v1/stream?graph=echo&session={session}"
        async with websockets.connect(url) as ws:
            await ws.recv()  # ready
            await ws.send(json.dumps({"text": text}))
            while True:
                env = json.loads(await asyncio.wait_for(ws.recv(), 20))
                if env.get("kind") == "data":
                    return (env.get("payload") or {}).get("text", "")
                if env.get("kind") == "error":
                    return "ERROR " + json.dumps(env.get("payload"))

    def events(session: str) -> list:
        code, body = get(f"/v1/sessions/{session}/events", 5.0)
        return json.loads(body).get("events", []) if code == 200 else []

    print(f"Failover — {binary}")
    leader = node([], "leader.log")
    first = wait_healthy()
    standby = node(["--standby"], "standby.log")
    env = dict(os.environ, PYTHONPATH=str(ROOT / "sdk" / "python" / "src"),
               AURA_WS_URL=f"ws://localhost:{args.port}/ws/skill", PYTHONUNBUFFERED="1")
    expertise = subprocess.Popen([sys.executable, "main.py"], cwd=str(expertise_dir()), env=env,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        check("an Expertise connects to the leader", wait_expertise())
        session = "sess-failover"
        before = asyncio.run(ask(session, "before the kill"))
        check("the session answers on the leader", "before the kill" in before, before)
        logged_before = len(events(session))
        time.sleep(1.0)  # let the standby see who it is standing by for

        leader.kill()  # SIGKILL: nothing is released, nothing is flushed on purpose
        killed = time.perf_counter()
        leader.wait()
        health = wait_healthy()
        outage_ms = (time.perf_counter() - killed) * 1000
        print(f"\n  outage, from the kill to /healthz answering again: {outage_ms:.0f} ms\n")
        check(f"the standby answers within {args.max_outage_ms} ms", outage_ms <= args.max_outage_ms,
              f"{outage_ms:.0f} ms")
        check("it is the same node, not a new one", health.get("node") == first.get("node"),
              f"{first.get('node')} -> {health.get('node')}")

        check("the Expertise reconnects on its own", wait_expertise())
        after = asyncio.run(ask(session, "after the takeover"))
        check("the same session continues on the standby", "after the takeover" in after, after)

        logged_after = len(events(session))
        check("the causal log holds both sides of the kill",
              logged_before > 0 and logged_after >= 2 * logged_before,
              f"{logged_before} events before, {logged_after} after")

        standby_log = Path(logs, "standby.log").read_text(encoding="utf-8", errors="replace")
        check("the standby says why it took over", "TOOK OVER" in standby_log)
        for line in standby_log.splitlines():
            if "TOOK OVER" in line or "the writer is" in line:
                print("    " + line.strip())
    finally:
        for proc in (expertise, standby, leader):
            try:
                proc.kill()
                proc.wait(timeout=5)
            except Exception:  # noqa: BLE001
                pass

    verify = subprocess.run([binary, "verify", "--data", data], capture_output=True, text=True)
    check("the ledger verifies offline after the takeover", verify.returncode == 0,
          verify.stdout[-300:] + verify.stderr[-300:])

    shutil.rmtree(data, ignore_errors=True)
    shutil.rmtree(logs, ignore_errors=True)
    print(f"\n  {'FAILED: ' + ', '.join(check.failed) if check.failed else 'failover holds'}")
    return 1 if check.failed else 0


if __name__ == "__main__":
    sys.exit(main())
