"""Tests for main.handle's command parsing — catalog/storage/download are
mocked out (patched on main, since main does `from x import y`), so this
never touches a real filesystem or the network.

    cd skills/model-manager && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import unittest
from unittest.mock import patch

import main


class FakeContext:
    """Duck-types just the aura.Context surface handle() uses, recording
    what would have gone out over the websocket instead of sending it."""

    def __init__(self, payload):
        self.payload = payload
        self.emitted: list[tuple[str, object]] = []
        self.statuses: list[tuple[str, str, str]] = []
        self.errors: list[tuple[str, str]] = []

    async def emit(self, port, payload):
        self.emitted.append((port, payload))

    async def status(self, port, state, detail=""):
        self.statuses.append((port, state, detail))

    async def error(self, port, detail):
        self.errors.append((port, detail))


class TestList(unittest.IsolatedAsyncioTestCase):
    async def test_delegates_to_storage_and_emits_models(self):
        models = [{"name": "a.gguf", "size_mb": 1.0}]
        with patch.object(main, "list_models", return_value=models) as mocked:
            ctx = FakeContext({"action": "list"})
            await main.handle(ctx)
        mocked.assert_called_once()
        self.assertEqual(ctx.emitted, [("result_out", {"action": "list", "models": models})])
        self.assertEqual(ctx.errors, [])


class TestCatalog(unittest.IsolatedAsyncioTestCase):
    async def test_passes_model_type_filter_through(self):
        entries = [{"model_id": "x"}]
        with patch.object(main, "filter_catalog", return_value=entries) as mocked:
            ctx = FakeContext({"action": "catalog", "model_type": "llm"})
            await main.handle(ctx)
        mocked.assert_called_once_with("llm")
        self.assertEqual(ctx.emitted, [("result_out", {"action": "catalog", "catalog": entries})])

    async def test_no_model_type_passes_none(self):
        with patch.object(main, "filter_catalog", return_value=[]) as mocked:
            ctx = FakeContext({"action": "catalog"})
            await main.handle(ctx)
        mocked.assert_called_once_with(None)


class TestSearch(unittest.IsolatedAsyncioTestCase):
    async def test_requires_a_query(self):
        ctx = FakeContext({"action": "search"})
        await main.handle(ctx)
        self.assertEqual(ctx.emitted, [])
        self.assertEqual(len(ctx.errors), 1)
        self.assertIn("search requires a query", ctx.errors[0][1])

    async def test_runs_query_and_emits_results(self):
        results = [{"hf_repo": "org/model", "downloads": 5, "likes": 1}]
        with patch.object(main, "search_models", return_value=results) as mocked:
            ctx = FakeContext({"action": "search", "query": "qwen"})
            await main.handle(ctx)
        mocked.assert_called_once_with("qwen")
        self.assertEqual(ctx.emitted, [("result_out", {"action": "search", "results": results})])
        self.assertEqual(ctx.statuses[0][1], "working")


class TestDownload(unittest.IsolatedAsyncioTestCase):
    async def test_requires_repo_and_filename_or_catalog_id(self):
        ctx = FakeContext({"action": "download"})
        await main.handle(ctx)
        self.assertEqual(ctx.emitted, [])
        self.assertEqual(len(ctx.errors), 1)

    async def test_resolves_catalog_model_id_to_repo_and_filename(self):
        entry = {"model_id": "whisper-base", "model_type": "asr", "tier": "mvp",
                  "hf_repo": "Systran/faster-whisper-base",
                  "hf_filename": "(faster-whisper auto)"}
        with patch.object(main, "find_model", return_value=entry) as mocked_find, \
             patch.object(main, "download_model", return_value="/models/whisper-base") as mocked_dl:
            ctx = FakeContext({"action": "download", "model_id": "whisper-base"})
            await main.handle(ctx)
        mocked_find.assert_called_once_with("whisper-base")
        mocked_dl.assert_called_once()
        self.assertEqual(mocked_dl.call_args.args[1:], (entry["hf_repo"], entry["hf_filename"]))
        self.assertEqual(ctx.emitted, [("result_out", {"action": "download", "path": "/models/whisper-base"})])

    async def test_unknown_catalog_model_id_errors(self):
        with patch.object(main, "find_model", return_value=None):
            ctx = FakeContext({"action": "download", "model_id": "nope"})
            await main.handle(ctx)
        self.assertEqual(ctx.emitted, [])
        self.assertIn("unknown catalog model_id", ctx.errors[0][1])

    async def test_explicit_repo_and_filename_skip_catalog_lookup(self):
        with patch.object(main, "find_model") as mocked_find, \
             patch.object(main, "download_model", return_value="/models/x.gguf") as mocked_dl:
            ctx = FakeContext({"action": "download", "hf_repo": "org/x", "hf_filename": "x.gguf"})
            await main.handle(ctx)
        mocked_find.assert_not_called()
        mocked_dl.assert_called_once()


class TestDelete(unittest.IsolatedAsyncioTestCase):
    async def test_delegates_to_storage(self):
        with patch.object(main, "delete_model") as mocked:
            ctx = FakeContext({"action": "delete", "model_id": "whisper-base/model.bin"})
            await main.handle(ctx)
        mocked.assert_called_once()
        self.assertEqual(mocked.call_args.args[1], "whisper-base/model.bin")
        self.assertEqual(ctx.emitted, [("result_out", {"action": "delete", "deleted": "whisper-base/model.bin"})])

    async def test_propagates_storage_errors_eg_path_traversal(self):
        with patch.object(main, "delete_model",
                           side_effect=ValueError("path escapes the models directory")):
            ctx = FakeContext({"action": "delete", "model_id": "../evil"})
            await main.handle(ctx)
        self.assertEqual(ctx.emitted, [])
        self.assertIn("path escapes the models directory", ctx.errors[0][1])


class TestUnknownOrMissingAction(unittest.IsolatedAsyncioTestCase):
    async def test_unknown_action_errors_without_raising(self):
        ctx = FakeContext({"action": "reboot"})
        await main.handle(ctx)
        self.assertEqual(ctx.emitted, [])
        self.assertIn("unknown action", ctx.errors[0][1])

    async def test_missing_action_errors(self):
        ctx = FakeContext({})
        await main.handle(ctx)
        self.assertEqual(len(ctx.errors), 1)


if __name__ == "__main__":
    unittest.main()
