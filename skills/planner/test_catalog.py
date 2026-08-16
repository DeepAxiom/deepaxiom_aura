"""Tests for catalog.py — prompt formatting and the fetch-error handling
that fixes the real bug this refactor exists for (fetch_catalog used to run
outside the handler's try/except, so an unreachable kernel raised an
uncaught exception). No live kernel needed: urllib.request.urlopen is
mocked.

    cd skills/planner && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import json
import socket
import unittest
import urllib.error
from unittest.mock import patch

import catalog


class _FakeResponse:
    """Minimal stand-in for the context-managed object urlopen() returns —
    json.load() only needs .read()."""

    def __init__(self, data) -> None:
        self._data = data if isinstance(data, (bytes, bytearray)) else json.dumps(data).encode()

    def read(self) -> bytes:
        return self._data


class CatalogLines(unittest.TestCase):
    def test_formats_capability_description_and_ingress_schema(self):
        got = catalog.catalog_lines([
            {"capability": "motor.tts.speak", "description": "Speaks text aloud",
             "ports": {"ingress": [{"name": "text_in", "schema": "std/text@1"}]}},
        ])
        self.assertEqual(got, "- motor.tts.speak :: Speaks text aloud :: std/text@1")

    def test_multiple_entries_are_newline_joined(self):
        skills = [
            {"capability": "a.b.c", "description": "d1",
             "ports": {"ingress": [{"name": "in", "schema": "std/text@1"}]}},
            {"capability": "x.y.z", "description": "d2",
             "ports": {"ingress": [{"name": "in", "schema": "std/document@1"}]}},
        ]
        got = catalog.catalog_lines(skills)
        self.assertEqual(got.count("\n"), 1)
        self.assertIn("a.b.c :: d1 :: std/text@1", got)
        self.assertIn("x.y.z :: d2 :: std/document@1", got)

    def test_empty_catalog_is_empty_string(self):
        self.assertEqual(catalog.catalog_lines([]), "")

    def test_only_the_first_ingress_port_is_used(self):
        got = catalog.catalog_lines([
            {"capability": "a.b.c", "description": "d",
             "ports": {"ingress": [
                 {"name": "first", "schema": "std/text@1"},
                 {"name": "second", "schema": "std/audio-chunk@1"},
             ]}},
        ])
        self.assertIn("std/text@1", got)
        self.assertNotIn("std/audio-chunk@1", got)


class FetchCatalogFilters(unittest.TestCase):
    def test_excludes_the_planner_itself_and_ports_without_ingress(self):
        skills = [
            {"capability": "cognitive.planner", "ports": {"ingress": [{"name": "goal_in"}]}},
            {"capability": "motor.tts.speak", "ports": {"ingress": [{"name": "text_in"}]}},
            {"capability": "logical.no_ingress", "ports": {"ingress": []}},
        ]
        with patch("urllib.request.urlopen") as mock_urlopen:
            mock_urlopen.return_value.__enter__.return_value = _FakeResponse(skills)
            got = catalog.fetch_catalog()

        self.assertEqual([s["capability"] for s in got], ["motor.tts.speak"])


class FetchCatalogErrorHandling(unittest.TestCase):
    """The bug this refactor fixes: a kernel that is down must not blow up
    with a raw urllib exception — it must surface as CatalogUnavailable so
    main.py's handler can turn it into a clean ctx.error(...)."""

    def test_connection_refused_raises_catalog_unavailable_not_urlerror(self):
        with patch("urllib.request.urlopen",
                   side_effect=urllib.error.URLError(ConnectionRefusedError())):
            with self.assertRaises(catalog.CatalogUnavailable):
                catalog.fetch_catalog()

    def test_timeout_raises_catalog_unavailable(self):
        with patch("urllib.request.urlopen", side_effect=socket.timeout("timed out")):
            with self.assertRaises(catalog.CatalogUnavailable):
                catalog.fetch_catalog()

    def test_malformed_json_body_raises_catalog_unavailable(self):
        with patch("urllib.request.urlopen") as mock_urlopen:
            mock_urlopen.return_value.__enter__.return_value = _FakeResponse(b"not json")
            with self.assertRaises(catalog.CatalogUnavailable):
                catalog.fetch_catalog()

    def test_no_other_exception_type_escapes(self):
        # Belt and suspenders: whatever the underlying error, callers should
        # only ever have to handle CatalogUnavailable.
        with patch("urllib.request.urlopen", side_effect=OSError("network unreachable")):
            try:
                catalog.fetch_catalog()
                self.fail("expected CatalogUnavailable")
            except catalog.CatalogUnavailable:
                pass


if __name__ == "__main__":
    unittest.main()
