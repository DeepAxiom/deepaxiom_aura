"""Tests for the skill.config -> engine wiring, without loading a model.

`main.py` reads `preload_on_boot`, `beam_size_final` and `beam_size_partial`
from `skill.config` and hands them to `engine.load_model`/`engine.transcribe`
as plain arguments. These tests monkeypatch `engine.load_model` and
`engine.transcribe` so nothing here ever touches faster-whisper — same spirit
as test_streaming.py, just covering main.py's side of the split instead of
engine.py's pure pieces.

    cd skills/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import asyncio
import base64
import io
import unittest
import wave
from unittest.mock import Mock

import engine
import main


def _wav_b64(samples=(0, 1, -1, 2), sample_rate=16000) -> str:
    import struct

    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(struct.pack(f"<{len(samples)}h", *samples))
    return base64.b64encode(buf.getvalue()).decode()


class FakeCtx:
    """Stand-in for aura.Context: enough surface for the handlers under test,
    none of the real websocket plumbing."""

    def __init__(self, payload, session="s1", node="n1"):
        self.payload = payload
        self.session = session
        self.node = node
        self.cancel_event = None
        self.cancelled = False
        self.emitted = []
        self.errors = []
        self.statuses = []

    async def emit(self, port, data, kind=None):
        self.emitted.append((port, data))

    async def error(self, port, detail):
        self.errors.append((port, detail))

    async def status(self, port, state, detail):
        self.statuses.append((port, state, detail))


class ConfigDefaults(unittest.TestCase):
    """The three new knobs must be declared in skill.yaml with the defaults
    documented there — this is what makes them show up in the UI's config
    panel with the right starting values."""

    def test_preload_on_boot_defaults_true(self):
        self.assertIs(main.skill.config["preload_on_boot"], True)

    def test_beam_size_final_defaults_to_five(self):
        self.assertEqual(main.skill.config["beam_size_final"], 5)

    def test_beam_size_partial_defaults_to_one(self):
        self.assertEqual(main.skill.config["beam_size_partial"], 1)


class PreloadOnBoot(unittest.TestCase):
    def setUp(self):
        self._preload = main.skill.config["preload_on_boot"]
        self._model_size = main.skill.config["model_size"]

    def tearDown(self):
        main.skill.config["preload_on_boot"] = self._preload
        main.skill.config["model_size"] = self._model_size

    def test_enabled_loads_the_configured_model_size(self):
        main.skill.config["preload_on_boot"] = True
        main.skill.config["model_size"] = "small"
        calls = []
        original_load_model = engine.load_model
        engine.load_model = lambda size: calls.append(size)
        original_thread = main.threading.Thread

        class SyncThread:
            """Runs the target immediately instead of on a real thread — the
            test only cares which model size reaches engine.load_model."""

            def __init__(self, target, args=(), daemon=None):
                self._target, self._args = target, args

            def start(self):
                self._target(*self._args)

        main.threading.Thread = SyncThread
        try:
            main._preload_if_configured()
        finally:
            engine.load_model = original_load_model
            main.threading.Thread = original_thread

        self.assertEqual(calls, ["small"])

    def test_disabled_never_touches_the_model(self):
        main.skill.config["preload_on_boot"] = False
        original_load_model = engine.load_model
        mock = Mock()
        engine.load_model = mock
        try:
            main._preload_if_configured()
        finally:
            engine.load_model = original_load_model
        mock.assert_not_called()


class BeamSizeWiring(unittest.TestCase):
    """`_transcribe`'s beam width used to be hardcoded (5 for the final
    transcript, 1 for partials); now it must come from skill.config on every
    call site that decodes audio."""

    def setUp(self):
        self._beam_final = main.skill.config["beam_size_final"]
        self._beam_partial = main.skill.config["beam_size_partial"]
        self._model_size = main.skill.config["model_size"]
        self._language = main.skill.config["language"]
        self._original_transcribe = engine.transcribe
        self.calls = []

        def fake_transcribe(pcm, sample_rate, language, beam, model_size, stop=None):
            self.calls.append({
                "beam": beam, "model_size": model_size, "language": language,
            })
            return "hola mundo"

        engine.transcribe = fake_transcribe
        main.skill.config["beam_size_final"] = 7
        main.skill.config["beam_size_partial"] = 2
        main.skill.config["model_size"] = "tiny"

    def tearDown(self):
        engine.transcribe = self._original_transcribe
        main.skill.config["beam_size_final"] = self._beam_final
        main.skill.config["beam_size_partial"] = self._beam_partial
        main.skill.config["model_size"] = self._model_size
        main.skill.config["language"] = self._language

    def test_handle_document_uses_beam_size_final(self):
        ctx = FakeCtx({"bytes_b64": _wav_b64()})
        asyncio.run(main.handle_document(ctx))

        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.calls[0]["beam"], 7)
        self.assertEqual(self.calls[0]["model_size"], "tiny")
        self.assertEqual(ctx.emitted[0], ("text_out", {"text": "hola mundo", "final": True}))

    def test_finalize_uses_beam_size_final(self):
        utt = engine.Utterance(id="u")
        utt.add(b"\x00\x00" * 8000, seq=0)  # 0.5s of silence at 16kHz
        ctx = FakeCtx({})

        asyncio.run(main._finalize(ctx, utt, reason="client"))

        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.calls[0]["beam"], 7)

    def test_maybe_partial_uses_beam_size_partial(self):
        utt = engine.Utterance(id="u")
        # Enough audio to clear MIN_PARTIAL_SECONDS and the default partial
        # interval (900ms) so _maybe_partial actually decodes.
        utt.add(b"\x00\x00" * 16000 * 2, seq=0)
        ctx = FakeCtx({})

        asyncio.run(main._maybe_partial(ctx, utt))

        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.calls[0]["beam"], 2)


if __name__ == "__main__":
    unittest.main()
