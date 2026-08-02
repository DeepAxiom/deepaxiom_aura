"""Tests for the SQLite persistence layer.

Pure `sqlite3` (stdlib) — no `aura` import, no `ChatBackend`, no model. Runs
on a temporary database file per test so nothing touches a real
`~/.aura/memory` install.

    cd skills/memory-context && PYTHONPATH=../../sdk/python/src python -m unittest test_store -v
"""
import tempfile
import unittest
from pathlib import Path

import store


class StoreTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.db = store.connect(str(Path(self._tmp.name) / "context.sqlite"))

    def tearDown(self) -> None:
        self.db.close()
        self._tmp.cleanup()


class Connect(StoreTestCase):
    def test_creates_parent_directories(self):
        nested = Path(self._tmp.name) / "a" / "b" / "context.sqlite"
        db = store.connect(str(nested))
        self.assertTrue(nested.exists())
        db.close()

    def test_reopening_the_same_path_preserves_data(self):
        store.remember(self.db, "s1", "user", "hola", max_turns=200)
        self.db.close()

        db2 = store.connect(str(Path(self._tmp.name) / "context.sqlite"))
        self.assertEqual(store.load(db2, "s1"), [(0, "user", "hola")])
        db2.close()


class RememberAndLoad(StoreTestCase):
    def test_roundtrip(self):
        store.remember(self.db, "s1", "user", "hello", max_turns=200)
        store.remember(self.db, "s1", "assistant", "hi there", max_turns=200)

        turns = store.load(self.db, "s1")

        self.assertEqual(turns, [(0, "user", "hello"), (1, "assistant", "hi there")])

    def test_sequence_numbers_are_per_session(self):
        store.remember(self.db, "s1", "user", "a", max_turns=200)
        store.remember(self.db, "s2", "user", "b", max_turns=200)
        store.remember(self.db, "s1", "user", "c", max_turns=200)

        self.assertEqual([seq for seq, _, _ in store.load(self.db, "s1")], [0, 1])
        self.assertEqual([seq for seq, _, _ in store.load(self.db, "s2")], [0])

    def test_max_turns_drops_the_oldest(self):
        for i in range(5):
            store.remember(self.db, "s1", "user", f"turn {i}", max_turns=3)

        turns = store.load(self.db, "s1")

        self.assertEqual([content for _, _, content in turns], ["turn 2", "turn 3", "turn 4"])

    def test_next_seq_starts_at_zero(self):
        self.assertEqual(store.next_seq(self.db, "s1"), 0)
        store.remember(self.db, "s1", "user", "x", max_turns=200)
        self.assertEqual(store.next_seq(self.db, "s1"), 1)


class Clear(StoreTestCase):
    def test_wipes_only_the_given_session(self):
        store.remember(self.db, "s1", "user", "a", max_turns=200)
        store.remember(self.db, "s2", "user", "b", max_turns=200)

        store.clear(self.db, "s1")

        self.assertEqual(store.load(self.db, "s1"), [])
        self.assertEqual(store.load(self.db, "s2"), [(0, "user", "b")])

    def test_clearing_an_empty_session_does_not_raise(self):
        store.clear(self.db, "never-existed")  # should not raise


class ApproxTokens(unittest.TestCase):
    def test_at_least_one_for_any_nonempty_string(self):
        self.assertEqual(store.approx_tokens("x"), 1)

    def test_roughly_four_chars_per_token(self):
        self.assertEqual(store.approx_tokens("a" * 40), 10)


class Recall(StoreTestCase):
    def test_empty_session_returns_nothing_and_nothing_dropped(self):
        result = store.recall(self.db, "s1", budget=100)
        self.assertEqual(result.kept, "")
        self.assertEqual(result.dropped, [])

    def test_everything_fits_under_budget(self):
        store.remember(self.db, "s1", "user", "hi", max_turns=200)
        store.remember(self.db, "s1", "assistant", "hello", max_turns=200)

        result = store.recall(self.db, "s1", budget=1000)

        self.assertEqual(result.kept, "user: hi\nassistant: hello")
        self.assertEqual(result.dropped, [])

    def test_a_tight_budget_keeps_only_the_newest_and_drops_the_rest(self):
        # Each ~40-char turn costs ~10 approx-tokens (len // 4).
        for i in range(5):
            store.remember(self.db, "s1", "user", f"turn number {i} " + "x" * 20, max_turns=200)

        result = store.recall(self.db, "s1", budget=10)

        self.assertIn("turn number 4", result.kept)
        self.assertNotIn("turn number 0", result.kept)
        self.assertEqual(len(result.dropped), 4)
        # dropped is oldest-first, kept is newest.
        self.assertEqual(result.dropped[0][2].startswith("turn number 0"), True)

    def test_at_least_the_newest_turn_is_always_kept_even_over_budget(self):
        """A single turn larger than the whole budget must still come back —
        otherwise 'recall' after one huge turn returns nothing at all."""
        store.remember(self.db, "s1", "user", "x" * 4000, max_turns=200)

        result = store.recall(self.db, "s1", budget=1)

        self.assertNotEqual(result.kept, "")
        self.assertEqual(result.dropped, [])


if __name__ == "__main__":
    unittest.main()
