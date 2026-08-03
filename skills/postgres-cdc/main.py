"""
postgres-cdc — sensorial.postgres.cdc (Postgres logical replication -> causal
events, Phase 3, ROADMAP.md).

One `watch_in` message starts the stream; the handler keeps running and
emitting `change_out` envelopes — one per row change, not batched per
transaction, the same reason the effect ledger seals one effect at a time
rather than a whole transaction's worth (spec/c4-ledger.md) — until the
session cancels it or the process stops.

Uses the `test_decoding` output plugin, built into Postgres core since 9.4:
no extension install needed on the target database, unlike wal2json (which
this skill deliberately does NOT depend on — verified by hand against a real
`debezium/postgres:16` image that it is not even present there). The
trade-off is a text format to parse instead of clean JSON — see
`parse_test_decoding_line`, which is the one function in this file that
matters and is tested on its own in test_parser.py against real captured
output, not invented examples.

Known limitation, honestly stated rather than silently accepted: a `cancel`
is only noticed the next time the replication connection has something to
tell this skill (a change, or Postgres's own keepalive) — the SDK's own
`Context.cancelled` docs already call cancel "best-effort... a skill decides
when to notice" for exactly this kind of reason, so this is consistent with
that contract, not an exception to it.

Setup, once, on the source database:

    ALTER SYSTEM SET wal_level = logical;   -- then restart Postgres
    -- no extension needed: test_decoding ships with core Postgres

Run:

    PG_CDC_DSN="host=... dbname=... user=... password=..." \\
        PYTHONPATH=../../sdk/python/src python main.py
"""
from __future__ import annotations

import asyncio
import logging
import os
import queue
import re
import threading

import psycopg2
import psycopg2.extras

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("postgres-cdc")

skill = Skill()

# --- parsing test_decoding's text output ------------------------------------
#
# Real captured shapes (probed by hand against a live Postgres — see the
# design note above and test_parser.py):
#
#   table public.t: INSERT: id[integer]:2 name[text]:'world'
#   table public.t: UPDATE: id[integer]:2 name[text]:'wo''rld  updated' amount[numeric]:12.50 note[text]:null
#   table public.t: DELETE: id[integer]:2
#
# DELETE (and UPDATE on a table that is not REPLICA IDENTITY FULL) only
# carries the replica-identity columns — the primary key, by default. That
# is a property of the source database, not something this parser lost.

_LINE_RE = re.compile(r"^table (?P<table>\S+): (?P<op>INSERT|UPDATE|DELETE): (?P<cols>.*)$")
_COL_RE = re.compile(r"(?P<name>\w+)\[(?P<type>[^\]]+)\]:(?P<value>'(?:[^']|'')*'|\S+)")


def _parse_value(raw: str):
    if raw == "null":
        return None
    if len(raw) >= 2 and raw.startswith("'") and raw.endswith("'"):
        return raw[1:-1].replace("''", "'")
    if re.fullmatch(r"-?\d+", raw):
        return int(raw)
    try:
        return float(raw)
    except ValueError:
        return raw  # booleans (t/f), dates, and anything else: pass through as text


def parse_test_decoding_line(line: str) -> dict | None:
    """One `test_decoding` output line -> {"table", "op", "columns"}, or None
    for a transaction boundary (BEGIN/COMMIT), a DDL statement (which
    test_decoding does not emit a data line for at all), or anything else
    that does not match the row-change shape.

    A column token that does not match `_COL_RE` is skipped, not raised on —
    a CDC feed staying up despite one unparseable column matters more than a
    clean exception on it, and this function is pure specifically so a
    caller can log the original line before deciding what to do about it.
    """
    line = line.strip()
    if not line:
        return None
    m = _LINE_RE.match(line)
    if not m:
        return None
    columns = {}
    for col in _COL_RE.finditer(m.group("cols")):
        columns[col.group("name")] = _parse_value(col.group("value"))
    return {"table": m.group("table"), "op": m.group("op").lower(), "columns": columns}


def _format_lsn(lsn: int) -> str:
    """Postgres's own LSN notation (e.g. "16/B374D848"), not the bare
    integer psycopg2 hands back — this is what an operator resuming from it
    by hand, or comparing against `pg_current_wal_lsn()`, expects to see."""
    return f"{lsn >> 32:X}/{lsn & 0xFFFFFFFF:08X}"


# --- the replication worker (a background thread, not the asyncio loop) ----
#
# psycopg2's consume_stream blocks the calling thread until StopReplication
# is raised — the same reason skills/asr runs model inference off the event
# loop rather than inside an async handler.


def _replication_worker(dsn: str, slot: str, out_queue: "queue.Queue[dict | None]",
                         stop_event: threading.Event) -> None:
    conn = psycopg2.connect(dsn, connection_factory=psycopg2.extras.LogicalReplicationConnection)
    cur = conn.cursor()
    try:
        cur.create_replication_slot(slot, output_plugin="test_decoding")
        log.info("created replication slot %s", slot)
    except psycopg2.errors.DuplicateObject:
        conn.rollback()
        log.info("reusing existing replication slot %s", slot)

    cur.start_replication(slot_name=slot, decode=True)

    def consume(msg) -> None:
        change = parse_test_decoding_line(msg.payload)
        if change is not None:
            change["lsn"] = _format_lsn(msg.data_start)
            out_queue.put(change)
        msg.cursor.send_feedback(flush_lsn=msg.data_start)
        if stop_event.is_set():
            raise psycopg2.extras.StopReplication()

    try:
        cur.consume_stream(consume)
    except psycopg2.extras.StopReplication:
        pass
    except Exception:  # noqa: BLE001
        log.exception("replication stream for slot %s failed", slot)
    finally:
        cur.close()
        conn.close()
        out_queue.put(None)  # sentinel: the worker has stopped, one way or another


@skill.on("watch_in")
async def handle(ctx: Context) -> None:
    dsn = os.environ.get("PG_CDC_DSN")
    if not dsn:
        await ctx.error("change_out", "PG_CDC_DSN is not set — nothing to watch")
        return

    slot = skill.config["slot"]
    allowed_tables = {t.strip() for t in skill.config["tables"].split(",") if t.strip()}

    out_queue: "queue.Queue[dict | None]" = queue.Queue()
    stop_event = ctx.cancel_event
    worker = threading.Thread(
        target=_replication_worker, args=(dsn, slot, out_queue, stop_event), daemon=True,
    )
    worker.start()
    await ctx.status("change_out", "working", f"watching slot {slot!r}")

    try:
        while True:
            change = await asyncio.to_thread(out_queue.get)
            if change is None:  # the worker stopped — cancelled, or a fatal error already logged
                break
            if allowed_tables and change["table"] not in allowed_tables:
                continue
            await ctx.emit("change_out", change)
    finally:
        stop_event.set()
        await ctx.done("change_out")


if __name__ == "__main__":
    skill.run()
