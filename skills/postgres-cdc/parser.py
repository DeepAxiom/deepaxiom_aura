"""Parsing for the `test_decoding` output plugin's text format.

Deliberately free of any `psycopg2` import — psycopg2 requires libpq and is
a heavy install, but this module is pure text parsing and does not need it.
Keeping it psycopg2-free lets its tests (test_parser.py) run in CI's light
lane, without the replication dependencies main.py needs.

Real captured shapes (probed by hand against a live Postgres — see
main.py's module docstring for how):

    table public.t: INSERT: id[integer]:2 name[text]:'world'
    table public.t: UPDATE: id[integer]:2 name[text]:'wo''rld  updated' amount[numeric]:12.50 note[text]:null
    table public.t: DELETE: id[integer]:2

DELETE (and UPDATE on a table that is not REPLICA IDENTITY FULL) only
carries the replica-identity columns — the primary key, by default. That is
a property of the source database, not something this parser lost.
"""
from __future__ import annotations

import re

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
