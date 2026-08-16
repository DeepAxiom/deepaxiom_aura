"""Tests for local filesystem storage — especially the path-traversal guard
in delete_model, which must reject anything that would resolve outside
models_dir exactly as it did before this module was split out of main.py.

    cd skills/model-manager && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import tempfile
import unittest
from pathlib import Path

import storage


class TestListModels(unittest.TestCase):
    def test_creates_models_dir_if_missing_and_returns_empty(self):
        with tempfile.TemporaryDirectory() as base:
            models_dir = Path(base) / "models"
            self.assertFalse(models_dir.exists())
            got = storage.list_models(models_dir)
            self.assertEqual(got, [])
            self.assertTrue(models_dir.is_dir())

    def test_lists_files_with_size_and_relative_name_sorted(self):
        with tempfile.TemporaryDirectory() as models_dir:
            models_dir = Path(models_dir)
            (models_dir / "b.gguf").write_bytes(b"x" * 2_000_000)
            (models_dir / "a.gguf").write_bytes(b"x" * 1_000_000)
            got = storage.list_models(models_dir)
            self.assertEqual([m["name"] for m in got], ["a.gguf", "b.gguf"])
            self.assertEqual(got[0]["size_mb"], 1.0)
            self.assertEqual(got[1]["size_mb"], 2.0)

    def test_includes_nested_files_and_excludes_dotfiles(self):
        with tempfile.TemporaryDirectory() as models_dir:
            models_dir = Path(models_dir)
            sub = models_dir / "whisper-base"
            sub.mkdir()
            (sub / "model.bin").write_bytes(b"x")
            (models_dir / ".DS_Store").write_bytes(b"x")
            names = [m["name"] for m in storage.list_models(models_dir)]
            self.assertIn(str(Path("whisper-base") / "model.bin"), names)
            self.assertNotIn(".DS_Store", names)


class TestDeleteModelPathTraversal(unittest.TestCase):
    """Every case here must raise ValueError WITHOUT touching the file
    outside models_dir — that's the actual security property, not just the
    exception type."""

    def test_rejects_dotdot_traversal(self):
        with tempfile.TemporaryDirectory() as base:
            base = Path(base)
            models_dir = base / "models"
            models_dir.mkdir()
            secret = base / "secret.txt"
            secret.write_text("nope")
            with self.assertRaises(ValueError):
                storage.delete_model(models_dir, "../secret.txt")
            self.assertTrue(secret.exists())

    def test_rejects_nested_dotdot_traversal(self):
        with tempfile.TemporaryDirectory() as base:
            base = Path(base)
            models_dir = base / "models"
            models_dir.mkdir()
            secret = base / "secret.txt"
            secret.write_text("nope")
            with self.assertRaises(ValueError):
                storage.delete_model(models_dir, "sub/../../secret.txt")
            self.assertTrue(secret.exists())

    def test_rejects_absolute_path_outside_models_dir(self):
        with tempfile.TemporaryDirectory() as models_dir, \
             tempfile.TemporaryDirectory() as outside:
            models_dir = Path(models_dir)
            secret = Path(outside) / "secret.txt"
            secret.write_text("nope")
            with self.assertRaises(ValueError):
                storage.delete_model(models_dir, str(secret))
            self.assertTrue(secret.exists())


class TestDeleteModel(unittest.TestCase):
    def test_removes_a_real_file(self):
        with tempfile.TemporaryDirectory() as models_dir:
            models_dir = Path(models_dir)
            target = models_dir / "model.gguf"
            target.write_bytes(b"x" * 10)
            storage.delete_model(models_dir, "model.gguf")
            self.assertFalse(target.exists())

    def test_allows_a_nested_path_within_models_dir(self):
        with tempfile.TemporaryDirectory() as models_dir:
            models_dir = Path(models_dir)
            sub = models_dir / "whisper-base"
            sub.mkdir()
            target = sub / "model.bin"
            target.write_bytes(b"x")
            storage.delete_model(models_dir, str(Path("whisper-base") / "model.bin"))
            self.assertFalse(target.exists())

    def test_missing_file_raises_file_not_found(self):
        with tempfile.TemporaryDirectory() as models_dir:
            with self.assertRaises(FileNotFoundError):
                storage.delete_model(Path(models_dir), "nope.gguf")

    def test_rejects_a_directory_target(self):
        with tempfile.TemporaryDirectory() as models_dir:
            models_dir = Path(models_dir)
            (models_dir / "whisper-base").mkdir()
            with self.assertRaises(FileNotFoundError):
                storage.delete_model(models_dir, "whisper-base")


if __name__ == "__main__":
    unittest.main()
