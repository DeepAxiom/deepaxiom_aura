"""
postgres-cdc — sensorial.postgres.cdc (Postgres logical replication -> causal
events).

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

Reconnection: earlier versions of this skill connected once and let any
failure (network blip, Postgres restart, replication slot momentarily busy)
end the whole `watch_in` stream — simplest possible code, but it meant a
transient failure needed a human to notice the stream died and restart it
by hand. `_replication_worker` now retries in a loop bounded by
`stop_event`, sleeping `reconnect_delay_s` between attempts, because a CDC
feed silently going dark is worse than a few seconds of extra log noise,
and every failure mode here (the ones worth retrying, at least) is the kind
that clears up on its own.

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
import threading

import psycopg2
import psycopg2.extras

from aura import Context, Skill

from parser import _format_lsn, parse_test_decoding_line

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("postgres-cdc")

skill = Skill()

# --- the replication worker (a background thread, not the asyncio loop) ----
#
# psycopg2's consume_stream blocks the calling thread until StopReplication
# is raised — the same reason skills/asr runs model inference off the event
# loop rather than inside an async handler.


def _replication_worker(dsn: str, slot: str, out_queue: "queue.Queue[dict | None]",
                         stop_event: threading.Event, connect_timeout_s: int,
                         reconnect_delay_s: int) -> None:
    # The sentinel must reach out_queue exactly once, no matter how the loop
    # below exits — retrying, cancelling, or connecting cleanly — so it lives
    # in this outer finally, not the per-attempt one.
    try:
        while not stop_event.is_set():
            conn = None
            cur = None
            try:
                conn = psycopg2.connect(
                    dsn, connect_timeout=connect_timeout_s,
                    connection_factory=psycopg2.extras.LogicalReplicationConnection,
                )
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
                return  # cancelled cleanly, or the stream ended on its own — no retry
            except Exception:  # noqa: BLE001
                log.exception(
                    "replication stream for slot %s failed; retrying in %ds",
                    slot, reconnect_delay_s,
                )
                if stop_event.wait(timeout=reconnect_delay_s):
                    return  # cancelled while waiting to retry
            finally:
                if cur is not None:
                    cur.close()
                if conn is not None:
                    conn.close()
        # loop exited because stop_event was already set before an attempt
        # even started (checked at the top of the `while`) — nothing to close.
    finally:
        out_queue.put(None)  # sentinel: the worker has stopped, one way or another


@skill.on("watch_in")
async def handle(ctx: Context) -> None:
    dsn = os.environ.get("PG_CDC_DSN")
    if not dsn:
        await ctx.error("change_out", "PG_CDC_DSN is not set — nothing to watch")
        return

    slot = skill.config["slot"]
    allowed_tables = {t.strip() for t in skill.config["tables"].split(",") if t.strip()}
    connect_timeout_s = skill.config["connect_timeout_s"]
    reconnect_delay_s = skill.config["reconnect_delay_s"]

    out_queue: "queue.Queue[dict | None]" = queue.Queue()
    stop_event = ctx.cancel_event
    worker = threading.Thread(
        target=_replication_worker,
        args=(dsn, slot, out_queue, stop_event, connect_timeout_s, reconnect_delay_s),
        daemon=True,
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
