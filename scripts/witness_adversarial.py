#!/usr/bin/env python3
"""Check that a witness can be caught rewriting its own log.

A witness exists so that parties who do not trust each other can rely on the
same anchor. That only holds if the witness is itself accountable — otherwise
the trust has simply been moved somewhere less visible. C4 v1.4 makes it
accountable by publishing the witness's own append-only Merkle log; this script
checks the claim the way it will actually be attacked, which is not by calling
an API wrongly but by *editing the database behind the running binary's back*.

    python scripts/witness_adversarial.py --witness-port 9411 --node-port 9412 \
        --data <dir> --aura-binary ./kernel/aura

Sequence:

  1. stand up a witness and a node, seal real effects, anchor them
  2. verify the monitor accepts an honest, growing witness
  3. rewrite one already-published entry directly in SQLite
  4. verify the monitor REFUSES, and exits non-zero

Step 4 is the whole point. Everything before it is setup.
"""

import argparse
import json
import os
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request

FAILURES = []


def ok(msg):
    print(f"  ok   {msg}")


def fail(msg):
    print(f"  FAIL {msg}", file=sys.stderr)
    FAILURES.append(msg)


def get(url, timeout=10):
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return r.status, json.loads(r.read().decode())


def wait_healthy(port, tries=60):
    for _ in range(tries):
        try:
            urllib.request.urlopen(f"http://localhost:{port}/healthz", timeout=2).read()
            return True
        except Exception:
            time.sleep(1)
    return False


def start_node(binary, port, data, extra=()):
    proc = subprocess.Popen(
        [binary, "up", "--port", str(port), "--no-auth", "--data", data, *extra],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if not wait_healthy(port):
        proc.kill()
        raise SystemExit(f"node on {port} never became healthy")
    return proc


def run(binary, *args):
    return subprocess.run([binary, *args], capture_output=True, text=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--witness-port", type=int, default=9411)
    ap.add_argument("--node-port", type=int, default=9412)
    ap.add_argument("--data", required=True)
    ap.add_argument("--aura-binary", required=True)
    args = ap.parse_args()

    binary = os.path.abspath(args.aura_binary)
    wit_dir = os.path.join(args.data, "wit")
    node_dir = os.path.join(args.data, "node")
    for d in (wit_dir, node_dir):
        shutil.rmtree(d, ignore_errors=True)
        os.makedirs(d, exist_ok=True)

    print("witness adversarial check:")
    procs = []
    try:
        procs.append(start_node(binary, args.witness_port, wit_dir, ["--open-witness"]))
        procs.append(start_node(binary, args.node_port, node_dir))
        ok("a witness and a node are up")

        # The witness's own log must be readable without a token: a log only its
        # operator can read cannot be audited by anyone who matters.
        code, head = get(f"http://localhost:{args.witness_port}/v1/witness/head")
        if code != 200 or "signature" not in head:
            fail(f"the witness does not publish a signed head: {code} {head}")
        else:
            ok("publishes a signed head, unauthenticated")

        # `seen` is this node's private audit baseline and must NOT be public on
        # the open surface — a stranger who could lower it could erase the very
        # record that convicts a witness. (This node runs --no-auth, so we can
        # only check the route exists and is separate; auth.go's unit tests pin
        # the exemption list itself.)
        code, _ = get(f"http://localhost:{args.node_port}/v1/witness/seen?key=x")
        if code != 200:
            fail("the local audit baseline route is missing")
        else:
            ok("the audit baseline is kept on the follower, not the witness")

        # Real effects, then anchor them.
        adversarial = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                   "ledger_adversarial.py")
        r = subprocess.run([sys.executable, adversarial, "--port", str(args.node_port),
                            "--data", node_dir, "--aura-binary", binary],
                           capture_output=True, text=True)
        if r.returncode != 0:
            fail(f"could not seal effects to anchor:\n{r.stdout}\n{r.stderr}")
            raise SystemExit(1)
        ok("sealed real effects on the node")

        r = run(binary, "witness", f"http://localhost:{args.witness_port}",
                "--port", str(args.node_port))
        if "WITNESSED" not in r.stdout:
            fail(f"anchoring failed:\n{r.stdout}\n{r.stderr}")
        else:
            ok("the node anchored at the witness")

        code, log = get(f"http://localhost:{args.witness_port}/v1/witness/log?from=1&limit=10")
        if code != 200 or not log.get("entries"):
            fail("the countersignature was not published in the witness's own log")
        else:
            ok("the countersignature appears in the witness's public log")

        # An honest witness passes the monitor, twice: the first run records a
        # baseline, the second has something to check against.
        for i in (1, 2):
            r = run(binary, "witness", "audit", f"http://localhost:{args.witness_port}",
                    "--port", str(args.node_port), "--full")
            if r.returncode != 0:
                fail(f"an honest witness was rejected by the monitor (run {i}):\n{r.stdout}\n{r.stderr}")
                break
        else:
            ok("an honest witness passes the monitor")

        # ── the actual test ──────────────────────────────────────────
        #
        # Stop the witness and rewrite an entry it has already published,
        # directly in SQLite — not through any API it exposes. This is the
        # move a witness would make to change what it had vouched for.
        for p in procs:
            p.send_signal(signal.SIGTERM)
        time.sleep(2)
        for p in procs:
            p.kill()
        procs.clear()

        db = os.path.join(wit_dir, "kernel.db")
        conn = sqlite3.connect(db)
        rows = conn.execute("SELECT seq, entry FROM witness_log ORDER BY seq").fetchall()
        if not rows:
            fail("the witness log is empty — nothing to tamper with")
            raise SystemExit(1)
        entry = json.loads(rows[0][1])
        entry["node_seq"] = entry["node_seq"] + 900
        conn.execute("UPDATE witness_log SET entry = ?, node_seq = ? WHERE seq = ?",
                     (json.dumps(entry, separators=(",", ":")), entry["node_seq"], rows[0][0]))
        conn.commit()
        conn.close()
        ok("rewrote a published witness-log entry behind the binary's back")

        procs.append(start_node(binary, args.witness_port, wit_dir, ["--open-witness"]))
        procs.append(start_node(binary, args.node_port, node_dir))

        r = run(binary, "witness", "audit", f"http://localhost:{args.witness_port}",
                "--port", str(args.node_port), "--full")
        if r.returncode == 0:
            fail("THE MONITOR ACCEPTED A WITNESS THAT REWROTE ITS OWN PUBLISHED LOG\n"
                 f"{r.stdout}\n{r.stderr}")
        else:
            ok("the monitor refuses a witness that rewrote its own log")
        if "rewrote history" not in r.stdout and "rewrote history" not in r.stderr:
            fail(f"the refusal does not say what happened:\n{r.stdout}\n{r.stderr}")
        else:
            ok("and says plainly what it detected")

    finally:
        for p in procs:
            p.kill()

    print()
    if FAILURES:
        print(f"witness adversarial check FAILED ({len(FAILURES)})", file=sys.stderr)
        sys.exit(1)
    print("witness adversarial check passed")


if __name__ == "__main__":
    main()
