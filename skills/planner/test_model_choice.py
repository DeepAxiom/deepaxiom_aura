"""Which model the planner plans with.

The order has to match `skills/llm-chat/models.py` exactly. Two skills that
resolve a model differently are two things a person has to learn, and the
question this answers — "what is Operate using, and how do I change it?" — is
only answerable if the answer is the same shape in both places.
"""
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import backends


class ModelChoice(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.env = mock.patch.dict(os.environ, {"AURA_MODELS_DIR": str(self.dir)}, clear=False)
        self.env.start()
        os.environ.pop("AURA_MODEL_PATH", None)

    def tearDown(self):
        self.env.stop()
        self.tmp.cleanup()

    def _touch(self, name: str) -> Path:
        p = self.dir / name
        p.write_bytes(b"gguf")
        return p

    def _set_active(self, name: str) -> None:
        (self.dir / "active.json").write_text(f'{{"model": "{name}"}}', encoding="utf-8")

    def test_env_path_wins(self):
        with mock.patch.dict(os.environ, {"AURA_MODEL_PATH": "/explicit.gguf"}):
            self.assertEqual(backends.resolve_model("anything.gguf"), "/explicit.gguf")

    def test_this_skills_config_is_its_own_choice(self):
        """The planner planning with one model while chat uses another."""
        chosen = self._touch("planner-model.gguf")
        self._touch("chat-model.gguf")
        self._set_active("chat-model.gguf")
        self.assertEqual(backends.resolve_model("planner-model.gguf"), str(chosen))

    def test_empty_config_follows_the_node_wide_active_model(self):
        """Leaving it blank is how you say 'the same as everything else'."""
        active = self._touch("shared.gguf")
        self._set_active("shared.gguf")
        self.assertEqual(backends.resolve_model(""), str(active))

    def test_config_naming_a_missing_file_falls_through(self):
        active = self._touch("shared.gguf")
        self._set_active("shared.gguf")
        self.assertEqual(backends.resolve_model("deleted.gguf"), str(active))

    def test_active_naming_a_missing_file_falls_through_to_the_default(self):
        self._set_active("gone.gguf")
        self.assertTrue(backends.resolve_model("").endswith(backends.DEFAULT_FILE))

    def test_nothing_configured_uses_the_built_in_default(self):
        self.assertTrue(backends.resolve_model("").endswith(backends.DEFAULT_FILE))


class ParityWithLlmChat(unittest.TestCase):
    """The two skills must resolve in the same order, or the docs lie."""

    def test_both_report_the_same_sources_in_the_same_order(self):
        import importlib.util

        path = Path(__file__).resolve().parent.parent / "llm-chat" / "models.py"
        spec = importlib.util.spec_from_file_location("chat_models", path)
        chat = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(chat)

        with tempfile.TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "a.gguf").write_bytes(b"gguf")
            (d / "b.gguf").write_bytes(b"gguf")
            (d / "active.json").write_text('{"model": "b.gguf"}', encoding="utf-8")
            with mock.patch.dict(os.environ, {"AURA_MODELS_DIR": str(d)}, clear=False):
                os.environ.pop("AURA_MODEL_PATH", None)
                # Own config wins over the node-wide active model, in both.
                self.assertEqual(backends.resolve_model("a.gguf"), str(d / "a.gguf"))
                self.assertEqual(chat.resolve("a.gguf")["path"], str(d / "a.gguf"))
                # Empty config follows active, in both.
                self.assertEqual(backends.resolve_model(""), str(d / "b.gguf"))
                self.assertEqual(chat.resolve("")["path"], str(d / "b.gguf"))


if __name__ == "__main__":
    unittest.main()
