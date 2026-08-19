"""Model resolution, and the join with model-manager.

The last test is the one that matters: it writes an active record with
`model-manager`'s own writer and reads it with this skill's reader. They are
separate processes with separate copies of the file's shape, so nothing at
runtime would catch one changing it — which is exactly how "download a model"
and "use a model" came to be unrelated acts in the first place.
"""
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import models


def _model_manager_active():
    """The sibling skill's writer, loaded straight from its file."""
    path = Path(__file__).resolve().parent.parent / "model-manager" / "active.py"
    spec = importlib.util.spec_from_file_location("mm_active", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Resolution(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.env = mock.patch.dict(os.environ, {"AURA_MODELS_DIR": str(self.dir)}, clear=False)
        self.env.start()
        for var in ("AURA_MODEL_PATH", "AURA_MODEL_REPO", "AURA_MODEL_FILE"):
            os.environ.pop(var, None)

    def tearDown(self):
        self.env.stop()
        self.tmp.cleanup()

    def _touch(self, name: str) -> Path:
        p = self.dir / name
        p.write_bytes(b"gguf")
        return p

    def test_default_when_nothing_is_installed(self):
        got = models.resolve()
        self.assertEqual(got["source"], "built-in default")
        self.assertEqual(got["repo"], models.DEFAULT_REPO)

    def test_env_path_wins_over_everything(self):
        self._touch("a.gguf")
        _model_manager_active().set_active(self.dir, "a.gguf")
        with mock.patch.dict(os.environ, {"AURA_MODEL_PATH": "/somewhere/explicit.gguf"}):
            got = models.resolve("a.gguf")
        self.assertEqual(got["path"], "/somewhere/explicit.gguf")
        self.assertEqual(got["source"], "AURA_MODEL_PATH")

    def test_config_model_is_used_when_the_file_exists(self):
        p = self._touch("chosen.gguf")
        got = models.resolve("chosen.gguf")
        self.assertEqual(got["path"], str(p))
        self.assertEqual(got["source"], "skill config `model`")

    def test_config_naming_a_missing_file_falls_through(self):
        """A stale config must not leave the node without a chat skill."""
        got = models.resolve("deleted.gguf")
        self.assertEqual(got["source"], "built-in default")

    def test_active_record_is_used_when_config_is_empty(self):
        p = self._touch("active-one.gguf")
        _model_manager_active().set_active(self.dir, "active-one.gguf")
        got = models.resolve("")
        self.assertEqual(got["path"], str(p))
        self.assertEqual(got["source"], "model-manager active.json")

    def test_config_beats_the_active_record(self):
        self._touch("active-one.gguf")
        chosen = self._touch("config-one.gguf")
        _model_manager_active().set_active(self.dir, "active-one.gguf")
        got = models.resolve("config-one.gguf")
        self.assertEqual(got["path"], str(chosen))

    def test_active_pointing_at_a_deleted_file_falls_through(self):
        self._touch("gone.gguf")
        _model_manager_active().set_active(self.dir, "gone.gguf")
        (self.dir / "gone.gguf").unlink()
        self.assertEqual(models.resolve("")["source"], "built-in default")

    def test_corrupt_active_json_is_not_fatal(self):
        (self.dir / "active.json").write_text("{not json", encoding="utf-8")
        self.assertEqual(models.resolve("")["source"], "built-in default")

    def test_installed_lists_models_but_not_the_pointer(self):
        self._touch("one.gguf")
        self._touch("two.gguf")
        _model_manager_active().set_active(self.dir, "one.gguf")
        self.assertEqual(models.installed(self.dir), ["one.gguf", "two.gguf"])


class JoinWithModelManager(unittest.TestCase):
    """What one skill writes, the other must read."""

    def test_the_record_written_there_is_understood_here(self):
        mm = _model_manager_active()
        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "m.gguf").write_bytes(b"gguf")
            written = mm.set_active(d, "m.gguf", model_id="gemma-3-4b-it")

            on_disk = json.loads((d / "active.json").read_text(encoding="utf-8"))
            self.assertEqual(on_disk["model"], "m.gguf")
            self.assertEqual(on_disk["model_id"], "gemma-3-4b-it")
            self.assertEqual(written["model"], on_disk["model"])

            with mock.patch.dict(os.environ, {"AURA_MODELS_DIR": str(d)}, clear=False):
                os.environ.pop("AURA_MODEL_PATH", None)
                got = models.resolve("")
            self.assertEqual(got["path"], str(d / "m.gguf"))
            self.assertEqual(got["source"], "model-manager active.json")

    def test_set_active_refuses_a_file_that_is_not_there(self):
        mm = _model_manager_active()
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ValueError):
                mm.set_active(Path(tmp), "never-downloaded.gguf")

    def test_set_active_refuses_a_path_that_escapes(self):
        mm = _model_manager_active()
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ValueError):
                mm.set_active(Path(tmp), "../../etc/passwd")


if __name__ == "__main__":
    unittest.main()
