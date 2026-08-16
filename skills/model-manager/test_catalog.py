"""Tests for catalog filtering/lookup — pure, no network.

    cd skills/model-manager && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import unittest

import catalog


class TestFilterCatalog(unittest.TestCase):
    def test_no_filter_returns_the_whole_catalog(self):
        self.assertEqual(catalog.filter_catalog(None), catalog.CATALOG)
        self.assertEqual(catalog.filter_catalog(""), catalog.CATALOG)

    def test_filters_by_model_type(self):
        got = catalog.filter_catalog("llm")
        self.assertTrue(got)
        self.assertTrue(all(e["model_type"] == "llm" for e in got))

    def test_unknown_model_type_returns_empty(self):
        self.assertEqual(catalog.filter_catalog("nonexistent-type"), [])

    def test_returns_a_copy_not_the_live_list(self):
        # Mutating the result must not corrupt the module-level CATALOG that
        # every other request reads from.
        got = catalog.filter_catalog(None)
        got.append({"model_id": "fake"})
        self.assertNotIn({"model_id": "fake"}, catalog.CATALOG)


class TestFindModel(unittest.TestCase):
    def test_finds_a_known_model(self):
        entry = catalog.find_model("whisper-base")
        self.assertIsNotNone(entry)
        self.assertEqual(entry["hf_repo"], "Systran/faster-whisper-base")

    def test_unknown_model_id_returns_none(self):
        self.assertIsNone(catalog.find_model("does-not-exist"))

    def test_every_entry_has_the_required_fields(self):
        required = {"model_id", "model_type", "tier", "hf_repo", "hf_filename"}
        for entry in catalog.CATALOG:
            self.assertTrue(required.issubset(entry.keys()), entry)

    def test_model_ids_are_unique(self):
        ids = [e["model_id"] for e in catalog.CATALOG]
        self.assertEqual(len(ids), len(set(ids)))


if __name__ == "__main__":
    unittest.main()
