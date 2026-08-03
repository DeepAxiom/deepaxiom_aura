"""Integration test against a REAL Postgres — not mocked, the same
discipline `kernel/internal/wasmrt`'s tests hold themselves to by compiling
and running a real wasm guest. Skips cleanly (not a failure) when Docker
isn't reachable, the same spirit as that package's guest-build skip.

    cd skills/postgres-cdc && PYTHONPATH=../../sdk/python/src python -m unittest test_integration -v
"""
from __future__ import annotations

import shutil
import subprocess
import time
import unittest

import psycopg2
import psycopg2.extras

import main


def _docker_available() -> bool:
    if shutil.which("docker") is None:
        return False
    try:
        subprocess.run(["docker", "info"], capture_output=True, timeout=10, check=True)
        return True
    except Exception:  # noqa: BLE001
        return False


_HAS_DOCKER = _docker_available()


@unittest.skipUnless(_HAS_DOCKER, "Docker is not reachable in this environment")
class TestAgainstRealPostgres(unittest.TestCase):
    """One container for the whole class — starting Postgres is the slow
    part, and nothing here mutates state a later test depends on not seeing
    (each test uses its own table and its own replication slot)."""

    container_id: str
    dsn: str

    @classmethod
    def setUpClass(cls) -> None:
        # `-c wal_level=logical`: a stock `postgres:16` does not default to
        # this (confirmed by hand — `debezium/postgres` does, plain
        # `postgres` does not), and test_decoding needs it.
        run = subprocess.run(
            ["docker", "run", "-d", "--rm",
             "-e", "POSTGRES_PASSWORD=test",
             "-p", "127.0.0.1::5432",
             "postgres:16", "-c", "wal_level=logical"],
            capture_output=True, text=True, timeout=30, check=True,
        )
        cls.container_id = run.stdout.strip()

        port_out = subprocess.run(
            ["docker", "port", cls.container_id, "5432/tcp"],
            capture_output=True, text=True, timeout=10, check=True,
        ).stdout.strip()
        # e.g. "127.0.0.1:55433\n0.0.0.0:55433" — take the first host:port.
        host_port = port_out.splitlines()[0].rsplit(":", 1)[1]
        cls.dsn = f"host=127.0.0.1 port={host_port} dbname=postgres user=postgres password=test"

        cls._wait_ready(timeout=30)

    @classmethod
    def tearDownClass(cls) -> None:
        subprocess.run(["docker", "rm", "-f", cls.container_id],
                        capture_output=True, timeout=15)

    @classmethod
    def _wait_ready(cls, timeout: float) -> None:
        deadline = time.time() + timeout
        last_err: Exception | None = None
        while time.time() < deadline:
            try:
                conn = psycopg2.connect(cls.dsn, connect_timeout=3)
                conn.close()
                return
            except Exception as exc:  # noqa: BLE001
                last_err = exc
                time.sleep(1)
        raise RuntimeError(f"postgres never became ready: {last_err}")

    # --- helpers -------------------------------------------------------------

    def _plain_cursor(self):
        conn = psycopg2.connect(self.dsn)
        conn.autocommit = True
        return conn, conn.cursor()

    def _replication_cursor(self):
        conn = psycopg2.connect(self.dsn, connection_factory=psycopg2.extras.LogicalReplicationConnection)
        return conn, conn.cursor()

    def _create_slot(self, name: str):
        conn, cur = self._replication_cursor()
        try:
            cur.create_replication_slot(name, output_plugin="test_decoding")
        except psycopg2.errors.DuplicateObject:
            conn.rollback()
        self.addCleanup(conn.close)
        return conn, cur

    def _drain_changes(self, slot: str) -> list[dict]:
        """Reads whatever the slot has queued via pg_logical_slot_get_changes
        — the same textual output `_replication_worker`'s consume_stream
        callback receives, just pulled with a plain query instead of a live
        streaming connection, since this test only needs to prove the real
        server and `parse_test_decoding_line` agree, not exercise the
        threading/asyncio bridge (kernel/internal/gateway's wasm tests are
        the precedent for "prove the wiring separately from the core logic").
        """
        conn, cur = self._plain_cursor()
        try:
            cur.execute(
                "SELECT data FROM pg_logical_slot_get_changes(%s, NULL, NULL)", (slot,)
            )
            lines = [row[0] for row in cur.fetchall()]
        finally:
            cur.close()
            conn.close()
        changes = []
        for line in lines:
            change = main.parse_test_decoding_line(line)
            if change is not None:
                changes.append(change)
        return changes

    # --- tests -----------------------------------------------------------------

    def test_insert_update_delete_round_trip(self):
        conn, cur = self._plain_cursor()
        cur.execute("CREATE TABLE cdc_orders(id serial primary key, name text, amount numeric)")
        cur.close()
        conn.close()

        self._create_slot("aura_cdc_roundtrip")

        conn, cur = self._plain_cursor()
        cur.execute("INSERT INTO cdc_orders(name, amount) VALUES ('alice', 100.50)")
        cur.execute("UPDATE cdc_orders SET amount = 200.75 WHERE name = 'alice'")
        cur.execute("DELETE FROM cdc_orders WHERE name = 'alice'")
        cur.close()
        conn.close()

        changes = self._drain_changes("aura_cdc_roundtrip")

        self.assertEqual(len(changes), 3, changes)
        insert, update, delete = changes

        self.assertEqual(insert["op"], "insert")
        self.assertEqual(insert["table"], "public.cdc_orders")
        self.assertEqual(insert["columns"]["name"], "alice")
        self.assertIsInstance(insert["columns"]["amount"], float)
        self.assertEqual(insert["columns"]["amount"], 100.50)

        self.assertEqual(update["op"], "update")
        self.assertEqual(update["columns"]["amount"], 200.75)

        self.assertEqual(delete["op"], "delete")
        # DELETE with default REPLICA IDENTITY only carries the primary key —
        # a real Postgres property, asserted here so a future change to this
        # parser cannot silently start expecting more than the server sends.
        self.assertEqual(set(delete["columns"]), {"id"})

    def test_null_round_trips_to_python_none(self):
        conn, cur = self._plain_cursor()
        cur.execute("CREATE TABLE cdc_notes(id serial primary key, note text)")
        cur.close()
        conn.close()

        self._create_slot("aura_cdc_null")

        conn, cur = self._plain_cursor()
        cur.execute("INSERT INTO cdc_notes(id, note) VALUES (1, NULL)")
        cur.close()
        conn.close()

        changes = self._drain_changes("aura_cdc_null")
        self.assertEqual(len(changes), 1, changes)
        self.assertIsNone(changes[0]["columns"]["note"])

    def test_tables_filter_matches_main_handlers_logic(self):
        conn, cur = self._plain_cursor()
        cur.execute("CREATE TABLE cdc_a(id serial primary key)")
        cur.execute("CREATE TABLE cdc_b(id serial primary key)")
        cur.close()
        conn.close()

        self._create_slot("aura_cdc_filter")

        conn, cur = self._plain_cursor()
        cur.execute("INSERT INTO cdc_a DEFAULT VALUES")
        cur.execute("INSERT INTO cdc_b DEFAULT VALUES")
        cur.close()
        conn.close()

        changes = self._drain_changes("aura_cdc_filter")
        self.assertEqual({c["table"] for c in changes}, {"public.cdc_a", "public.cdc_b"})

        # The same filtering `handle()` applies in main.py, given only
        # public.cdc_a is allowed.
        allowed_tables = {"public.cdc_a"}
        filtered = [c for c in changes if not allowed_tables or c["table"] in allowed_tables]
        self.assertEqual([c["table"] for c in filtered], ["public.cdc_a"])


if __name__ == "__main__":
    unittest.main()
