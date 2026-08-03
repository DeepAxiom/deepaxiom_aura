"""Tests for parse_test_decoding_line — pure, no database.

Every "real output" case here is text captured by hand from a live Postgres
16 container (test_decoding plugin, no extension installed), not invented —
see main.py's module docstring for how.

    cd skills/postgres-cdc && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import unittest

import main


class TestParseTestDecodingLine(unittest.TestCase):
    def test_insert(self):
        got = main.parse_test_decoding_line(
            "table public.t: INSERT: id[integer]:2 name[text]:'world'"
        )
        self.assertEqual(got, {
            "table": "public.t", "op": "insert",
            "columns": {"id": 2, "name": "world"},
        })

    def test_update_with_escaped_quote_null_and_numeric(self):
        got = main.parse_test_decoding_line(
            "table public.t: UPDATE: id[integer]:2 name[text]:'wo''rld  updated' "
            "amount[numeric]:12.50 note[text]:null"
        )
        self.assertEqual(got, {
            "table": "public.t", "op": "update",
            "columns": {
                "id": 2,
                "name": "wo'rld  updated",  # '' unescaped to a single '
                "amount": 12.50,
                "note": None,
            },
        })

    def test_delete_carries_only_replica_identity_columns(self):
        got = main.parse_test_decoding_line("table public.t: DELETE: id[integer]:2")
        self.assertEqual(got, {"table": "public.t", "op": "delete", "columns": {"id": 2}})

    def test_schema_qualified_table_name_is_preserved(self):
        got = main.parse_test_decoding_line(
            "table analytics.events: INSERT: id[bigint]:9001"
        )
        self.assertEqual(got["table"], "analytics.events")

    def test_negative_and_float_values(self):
        got = main.parse_test_decoding_line(
            "table public.t: INSERT: balance[numeric]:-42.5 count[integer]:-3"
        )
        self.assertEqual(got["columns"], {"balance": -42.5, "count": -3})

    # --- transaction boundaries and non-data lines --------------------------

    def test_begin_is_none(self):
        self.assertIsNone(main.parse_test_decoding_line("BEGIN 733"))

    def test_commit_is_none(self):
        self.assertIsNone(main.parse_test_decoding_line("COMMIT 733"))

    def test_empty_line_is_none(self):
        self.assertIsNone(main.parse_test_decoding_line(""))
        self.assertIsNone(main.parse_test_decoding_line("   "))

    # --- adversarial: malformed input must never raise ----------------------

    def test_complete_garbage_is_none_not_an_exception(self):
        self.assertIsNone(main.parse_test_decoding_line("complete garbage, no structure at all"))

    def test_unknown_operation_is_none(self):
        # TRUNCATE and other statements test_decoding might one day describe
        # differently must not be mistaken for INSERT/UPDATE/DELETE.
        self.assertIsNone(main.parse_test_decoding_line("table public.t: TRUNCATE: "))

    def test_a_malformed_column_token_is_skipped_not_fatal(self):
        # "garbage_no_brackets" matches neither the quoted-string nor the
        # bracketed name[type]:value shape — the well-formed column next to
        # it must still come through, proving one bad token does not sink
        # the whole line.
        got = main.parse_test_decoding_line(
            "table public.t: INSERT: id[integer]:1 garbage_no_brackets"
        )
        self.assertEqual(got, {"table": "public.t", "op": "insert", "columns": {"id": 1}})

    def test_value_containing_bracket_like_text_does_not_confuse_the_parser(self):
        got = main.parse_test_decoding_line(
            "table public.t: INSERT: id[integer]:1 note[text]:'looks[like]:this'"
        )
        self.assertEqual(got["columns"]["note"], "looks[like]:this")


class TestFormatLSN(unittest.TestCase):
    def test_round_trips_through_postgres_notation(self):
        # 22410856 was a real msg.data_start value captured against a live
        # replication stream.
        s = main._format_lsn(22410856)
        hi, lo = s.split("/")
        self.assertEqual((int(hi, 16) << 32) | int(lo, 16), 22410856)

    def test_matches_a_known_value(self):
        self.assertEqual(main._format_lsn(22410856), "0/0155F668")


if __name__ == "__main__":
    unittest.main()
