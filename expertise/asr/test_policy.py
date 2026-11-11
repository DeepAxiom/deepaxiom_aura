"""Tests for the streaming policy switch, the device config and their wiring
through main.py and engine.py — without a model.

Three things are pinned down here:

  * The default config selects the long-standing path, and that path puts on
    the wire exactly what it always did. `local_agreement` is opt-in because
    CI cannot run it against a model; a default that silently changed would
    defeat the point.
  * With `local_agreement`, the handlers drive the streaming decoder end to
    end — against a scripted stand-in for `engine.transcribe_words`, which is
    as far as "end to end" goes here.
  * `device` / `compute_type` resolve and fall back as documented, against a
    fake model factory.

    cd expertise/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import asyncio
import base64
import struct
import threading
import time
import unittest

import engine
import main
from streaming import Word

RATE = 16000
CHUNK = 1600  # samples: 100 ms


def marked_audio(seconds):
    """Loud PCM whose samples say where they are: sample n holds
    1000 + n // 160, i.e. its own time in hundredths of a second. A fake
    decoder can read the start of whatever window it is handed straight off
    the first sample."""
    n = int(seconds * RATE)
    return struct.pack(f"<{n}h", *(1000 + i // 160 for i in range(n)))


def window_of(pcm):
    """(start, end) in utterance seconds of a slice of marked_audio."""
    if not pcm:
        return 0.0, 0.0
    first = struct.unpack_from("<h", pcm)[0]
    start = (first - 1000) / 100
    return start, start + len(pcm) / 2 / RATE


TRUTH = [
    ("Please", 0.0, 0.4), ("turn", 0.5, 0.8), ("on", 0.8, 1.0),
    ("the", 1.0, 1.2), ("kitchen", 1.2, 1.8), ("light", 1.8, 2.3),
    ("and", 2.6, 2.8), ("set", 2.8, 3.1), ("the", 3.1, 3.3),
    ("thermostat", 3.3, 4.2), ("to", 4.2, 4.4), ("twenty", 4.4, 4.9),
    ("one", 4.9, 5.2), ("degrees.", 5.2, 5.9),
]
TRUTH_TEXT = " ".join(w for w, _, _ in TRUTH)


class FakeCtx:
    def __init__(self, payload, session="s1", node="n1", cancel_event=None):
        self.payload = payload
        self.session = session
        self.node = node
        self.cancel_event = cancel_event
        self.sink = None

    @property
    def cancelled(self):
        return self.cancel_event is not None and self.cancel_event.is_set()

    async def emit(self, port, data, kind=None):
        self.sink.append((port, data))

    async def error(self, port, detail):
        self.sink.append(("error", detail))

    async def status(self, port, state, detail):
        self.sink.append(("status", detail))


class Wired(unittest.TestCase):
    """Base: isolates skill.config, the utterance table and the engine's
    decode functions, and records every call to them."""

    def setUp(self):
        self._config = dict(main.skill.config)
        self._utterances = dict(engine.utterances)
        self._last_seen = dict(engine.last_seen)
        engine.utterances.clear()
        engine.last_seen.clear()
        self._transcribe = engine.transcribe
        self._transcribe_words = engine.transcribe_words
        self._prefs = dict(engine._prefs)
        self.plain_calls = []     # engine.transcribe
        self.word_calls = []      # engine.transcribe_words
        self.sink = []            # everything emitted, in order

        def fake_transcribe(pcm, sample_rate, language, beam, model_size, stop=None):
            self.plain_calls.append({"seconds": len(pcm) / 2 / sample_rate, "beam": beam})
            if stop is not None and stop.is_set():
                return ""
            return f"decoded {len(pcm) // 2} samples"

        def fake_words(pcm, sample_rate, language, beam, model_size,
                       stop=None, prompt="", info=None):
            a, b = window_of(pcm)
            self.word_calls.append({"start": a, "end": b, "beam": beam,
                                    "prompt": prompt, "language": language})
            if info is not None:
                info["language"] = "en"
            out = []
            for text, s, e in TRUTH:
                if s < a - 0.05:
                    continue
                if e <= b:
                    out.append(Word(text, s - a, e - a))
                elif s < b:
                    out.append(Word("~" + text[:2], s - a, b - a))
            return out

        engine.transcribe = fake_transcribe
        engine.transcribe_words = fake_words

    def tearDown(self):
        engine.transcribe = self._transcribe
        engine.transcribe_words = self._transcribe_words
        engine._prefs.clear()
        engine._prefs.update(self._prefs)
        main.skill.config.clear()
        main.skill.config.update(self._config)
        engine.utterances.clear()
        engine.utterances.update(self._utterances)
        engine.last_seen.clear()
        engine.last_seen.update(self._last_seen)

    def speak(self, seconds=6.0, session="s1", late=None):
        """Feed `seconds` of marked audio through handle_chunk in 100 ms
        chunks, the last one flagged final. `late` = (seq, after_seq) delivers
        one chunk out of order."""
        audio = marked_audio(seconds)
        chunks = [audio[i:i + CHUNK * 2] for i in range(0, len(audio), CHUNK * 2)]
        order = list(range(len(chunks)))
        if late:
            seq, after = late
            order.remove(seq)
            order.insert(order.index(after) + 1, seq)

        async def run():
            for n, seq in enumerate(order):
                ctx = FakeCtx({
                    "pcm_b64": base64.b64encode(chunks[seq]).decode(),
                    "sample_rate": RATE, "seq": seq,
                    "final": n == len(order) - 1}, session=session)
                ctx.sink = self.sink
                await main.handle_chunk(ctx)

        asyncio.run(run())

    def transcripts(self):
        return [d for port, d in self.sink if port == "transcript_out"]

    def texts(self):
        return [d for port, d in self.sink if port == "text_out"]


class DefaultsAreTodaysBehaviour(Wired):
    def test_declared_defaults(self):
        self.assertEqual(main.skill.config["streaming_policy"], "redecode")
        self.assertIs(main.skill.config["final_redecode"], False)
        self.assertEqual(main.skill.config["device"], "cpu")
        self.assertEqual(main.skill.config["compute_type"], "int8")

    def test_the_new_keys_are_declared_the_way_c1_wants_them(self):
        declared = {p["key"]: p for p in main.skill.manifest["config"]}
        self.assertEqual(declared["streaming_policy"]["type"], "enum")
        self.assertEqual(declared["streaming_policy"]["options"],
                         ["redecode", "local_agreement"])
        self.assertEqual(declared["device"]["options"], ["auto", "cpu", "cuda"])
        self.assertIn("auto", declared["compute_type"]["options"])
        self.assertIn("float16", declared["compute_type"]["options"])
        self.assertEqual(declared["final_redecode"]["type"], "bool")
        for key in ("streaming_policy", "final_redecode", "device", "compute_type"):
            self.assertIn(declared[key]["default"],
                          declared[key].get("options", [True, False]))
            self.assertTrue(declared[key]["description"])

    def test_default_config_never_reaches_the_streaming_decoder(self):
        self.speak(3.0)
        self.assertEqual(self.word_calls, [])
        self.assertTrue(self.plain_calls)

    def test_default_config_puts_exactly_the_old_payloads_on_the_wire(self):
        """Golden: 3 s of speech at the default 900 ms interval. Partials at
        1.0, 1.9 and 2.8 s of audio, each a from-scratch decode of the buffer
        at beam 1; then one decode of the whole utterance at beam 5, sent to
        transcript_out and — once — to text_out. No extra keys."""
        self.speak(3.0)
        utt = self.sink[0][1]["utterance"]

        self.assertEqual(self.sink, [
            ("transcript_out", {"text": "decoded 16000 samples", "final": False,
                                "replace": True, "utterance": utt}),
            ("transcript_out", {"text": "decoded 30400 samples", "final": False,
                                "replace": True, "utterance": utt}),
            ("transcript_out", {"text": "decoded 44800 samples", "final": False,
                                "replace": True, "utterance": utt}),
            ("transcript_out", {"text": "decoded 48000 samples", "final": True,
                                "replace": True, "utterance": utt}),
            ("text_out", {"text": "decoded 48000 samples", "final": True}),
        ])
        self.assertEqual([c["beam"] for c in self.plain_calls], [1, 1, 1, 5])

    def test_an_unknown_policy_value_is_treated_as_redecode(self):
        main.skill.config["streaming_policy"] = "something-newer"
        self.speak(2.0)
        self.assertEqual(self.word_calls, [])
        self.assertNotIn("stable_chars", self.transcripts()[-1])

    def test_a_policy_key_missing_altogether_is_redecode(self):
        """An older kernel's ack, or a hand-built config, may not carry it."""
        del main.skill.config["streaming_policy"]
        del main.skill.config["final_redecode"]
        del main.skill.config["device"]
        del main.skill.config["compute_type"]
        self.speak(2.0)
        self.assertEqual(self.word_calls, [])
        self.assertEqual(engine._prefs, {"device": "cpu", "compute_type": "int8"})

    def test_redecode_partials_still_see_only_the_last_ten_seconds(self):
        main.skill.config["max_utterance_s"] = 60
        self.speak(12.0)
        partial_spans = [c["seconds"] for c in self.plain_calls if c["beam"] == 1]
        self.assertEqual(max(partial_spans), 10.0)
        self.assertEqual(self.plain_calls[-1], {"seconds": 12.0, "beam": 5})


class LocalAgreementWiring(Wired):
    def setUp(self):
        super().setUp()
        main.skill.config["streaming_policy"] = "local_agreement"

    def test_the_final_is_committed_words_plus_one_short_tail_decode(self):
        self.speak(6.0)

        final = self.transcripts()[-1]
        self.assertEqual(final["text"], TRUTH_TEXT)
        self.assertIs(final["final"], True)
        # No whole-utterance decode anywhere, and the final decode (beam 5)
        # covered a fraction of the six seconds.
        self.assertEqual(self.plain_calls, [])
        finals = [c for c in self.word_calls if c["beam"] == 5]
        self.assertEqual(len(finals), 1)
        self.assertLess(finals[0]["end"] - finals[0]["start"], 3.0)
        self.assertAlmostEqual(finals[0]["end"], 6.0)

    def test_text_out_gets_the_final_once_and_nothing_else(self):
        self.speak(6.0)
        self.assertEqual(self.texts(), [{"text": TRUTH_TEXT, "final": True}])
        self.assertEqual(self.sink[-1][0], "text_out")

    def test_partials_keep_the_replace_contract_and_add_stable_chars(self):
        self.speak(6.0)
        *partials, final = self.transcripts()

        self.assertGreaterEqual(len(partials), 4)
        utt = final["utterance"]
        for p in partials:
            self.assertEqual(set(p), {"text", "final", "replace", "utterance", "stable_chars"})
            self.assertIs(p["final"], False)
            self.assertIs(p["replace"], True)
            self.assertEqual(p["utterance"], utt)
            self.assertIsInstance(p["stable_chars"], int)
            self.assertLessEqual(p["stable_chars"], len(p["text"]))
        self.assertEqual(final["stable_chars"], len(final["text"]))
        self.assertIs(final["replace"], True)

    def test_what_a_partial_calls_stable_stays_put(self):
        self.speak(6.0)
        shown = self.transcripts()
        self.assertTrue(any(0 < p["stable_chars"] < len(p["text"]) for p in shown))
        for i, p in enumerate(shown):
            stable = p["text"][:p["stable_chars"]]
            for later in shown[i:]:
                self.assertTrue(later["text"].startswith(stable), (stable, later["text"]))

    def test_each_partial_shows_the_whole_hypothesis_not_a_delta(self):
        """An existing UI replaces what it shows with `text`. If `text` were
        only the new words it would show a sliding fragment."""
        self.speak(6.0)
        partials = self.transcripts()[:-1]
        self.assertTrue(all(p["text"].startswith("Please") for p in partials))
        lengths = [len(p["text"]) for p in partials]
        self.assertGreater(lengths[-1], lengths[0])

    def test_partial_decodes_stop_growing_with_the_utterance(self):
        self.speak(6.0)
        spans = [c["end"] - c["start"] for c in self.word_calls if c["beam"] == 1]
        self.assertGreaterEqual(len(spans), 4)
        self.assertLess(max(spans), 4.0)          # redecode: up to 5.5
        self.assertGreater(self.word_calls[-2]["start"], 2.0)

    def test_decodes_after_the_first_commit_carry_a_prompt(self):
        self.speak(6.0)
        self.assertEqual(self.word_calls[0]["prompt"], "")
        prompted = [c for c in self.word_calls if c["prompt"]]
        self.assertTrue(prompted)
        for c in prompted:
            self.assertTrue(TRUTH_TEXT.startswith(c["prompt"]) or c["prompt"] in TRUTH_TEXT)
            self.assertGreater(c["start"], 0.0)

    def test_beam_widths_come_from_config(self):
        main.skill.config["beam_size_partial"] = 2
        main.skill.config["beam_size_final"] = 7
        self.speak(6.0)
        self.assertEqual({c["beam"] for c in self.word_calls[:-1]}, {2})
        self.assertEqual(self.word_calls[-1]["beam"], 7)

    def test_final_redecode_decodes_the_whole_utterance_again(self):
        main.skill.config["final_redecode"] = True
        self.speak(6.0)

        self.assertEqual(self.plain_calls, [{"seconds": 6.0, "beam": 5}])
        self.assertTrue(all(c["beam"] == 1 for c in self.word_calls))
        final = self.transcripts()[-1]
        self.assertEqual(final["text"], "decoded 96000 samples")
        self.assertEqual(final["stable_chars"], len(final["text"]))
        self.assertEqual(self.texts(), [{"text": final["text"], "final": True}])

    def test_a_short_utterance_with_no_partial_is_one_plain_decode(self):
        """Under a second of audio never gets a partial; there is no stream
        state, so the final is the same single decode redecode would do."""
        self.speak(0.8)
        self.assertEqual(self.word_calls, [])
        self.assertEqual(self.plain_calls, [{"seconds": 0.8, "beam": 5}])
        self.assertEqual(len(self.transcripts()), 1)

    def test_partials_disabled_still_yields_the_final(self):
        main.skill.config["partials_enabled"] = False
        self.speak(6.0)
        self.assertEqual(len(self.transcripts()), 1)
        self.assertEqual(len(self.texts()), 1)

    def test_a_late_chunk_in_committed_audio_forces_a_full_final(self):
        """Chunk 3 (0.3-0.4 s) arrives after 3.5 s of audio, long after
        "Please" was committed. Those samples were never decoded; the final
        must not pretend otherwise."""
        self.speak(6.0, late=(3, 35))

        self.assertEqual(self.plain_calls, [{"seconds": 6.0, "beam": 5}])
        self.assertEqual(self.transcripts()[-1]["text"], "decoded 96000 samples")
        self.assertEqual(len(self.texts()), 1)

    def test_a_late_chunk_in_uncommitted_audio_does_not(self):
        self.speak(6.0, late=(55, 57))
        self.assertEqual(self.plain_calls, [])
        self.assertEqual(self.transcripts()[-1]["text"], TRUTH_TEXT)

    def test_a_dropped_chunk_is_reported_and_the_stream_goes_on(self):
        audio = marked_audio(3.0)
        chunks = [audio[i:i + CHUNK * 2] for i in range(0, len(audio), CHUNK * 2)]

        async def run():
            for seq, chunk in enumerate(chunks):
                if seq == 12:
                    continue                      # lost in transit
                ctx = FakeCtx({"pcm_b64": base64.b64encode(chunk).decode(),
                               "sample_rate": RATE, "seq": seq,
                               "final": seq == len(chunks) - 1})
                ctx.sink = self.sink
                await main.handle_chunk(ctx)

        asyncio.run(run())
        self.assertIn(("status", "1 audio chunk(s) were dropped in transit"), self.sink)
        self.assertIs(self.transcripts()[-1]["final"], True)
        self.assertEqual(len(self.texts()), 1)

    def test_two_sessions_do_not_share_stream_state(self):
        async def run():
            audio = marked_audio(6.0)
            chunks = [audio[i:i + CHUNK * 2] for i in range(0, len(audio), CHUNK * 2)]
            for seq, chunk in enumerate(chunks):
                for session in ("a", "b"):
                    ctx = FakeCtx({"pcm_b64": base64.b64encode(chunk).decode(),
                                   "sample_rate": RATE, "seq": seq,
                                   "final": seq == len(chunks) - 1}, session=session)
                    ctx.sink = self.sink
                    await main.handle_chunk(ctx)

        asyncio.run(run())
        finals = [d for d in self.transcripts() if d["final"]]
        self.assertEqual([d["text"] for d in finals], [TRUTH_TEXT, TRUTH_TEXT])
        self.assertEqual(len({d["utterance"] for d in finals}), 2)

    def test_an_unchanged_hypothesis_is_not_sent_twice(self):
        engine.transcribe_words = lambda *a, **k: [Word("hello", 0.0, 0.5)]
        self.speak(5.0)
        partials = self.transcripts()[:-1]
        self.assertEqual(len(partials), len({(p["text"], p["stable_chars"]) for p in partials}))


class SilentTail(Wired):
    """Words timed over silence at the end of a hypothesis are inventions.
    The fixture is a real decode: "...de compras." followed by quiet came
    back with "No, no, no." stamped on the quiet."""

    LOUD = struct.pack("<h", 3000)
    QUIET = struct.pack("<h", 120)

    def _audio(self, loud_s, quiet_s):
        return self.LOUD * int(loud_s * RATE) + self.QUIET * int(quiet_s * RATE)

    HALLUCINATED = [
        Word("de", 0.86, 1.04), Word("compras.", 1.04, 1.50),
        Word("No,", 1.50, 2.36), Word("no,", 2.36, 2.36), Word("no.", 2.36, 2.36)]

    def test_trailing_words_over_silence_are_dropped(self):
        pcm = self._audio(1.5, 0.9)
        kept = engine.drop_silent_tail(self.HALLUCINATED, pcm, RATE, 500)
        self.assertEqual([w.text for w in kept], ["de", "compras."])

    def test_a_threshold_of_zero_disables_it(self):
        pcm = self._audio(1.5, 0.9)
        self.assertEqual(engine.drop_silent_tail(self.HALLUCINATED, pcm, RATE, 0),
                         self.HALLUCINATED)

    def test_a_quiet_word_in_the_middle_is_kept(self):
        """Only a trailing run goes: mid-sentence, a soft syllable or a
        timestamp a little off is likelier than an invention."""
        pcm = self.LOUD * 8000 + self.QUIET * 8000 + self.LOUD * 8000
        said = [Word("a", 0.0, 0.5), Word("soft", 0.5, 1.0), Word("b", 1.0, 1.5)]
        self.assertEqual(engine.drop_silent_tail(said, pcm, RATE, 500), said)

    def test_a_word_that_is_partly_loud_is_kept(self):
        pcm = self._audio(1.0, 1.0)
        tail = [Word("end", 0.8, 1.4)]
        self.assertEqual(engine.drop_silent_tail(tail, pcm, RATE, 500), tail)

    def test_all_silence_leaves_nothing_and_nothing_breaks(self):
        pcm = self._audio(0.0, 2.0)
        self.assertEqual(engine.drop_silent_tail(self.HALLUCINATED, pcm, RATE, 500), [])
        self.assertEqual(engine.drop_silent_tail([], pcm, RATE, 500), [])
        self.assertEqual(engine.drop_silent_tail(self.HALLUCINATED, b"", RATE, 500), [])

    def test_the_final_uses_the_configured_silence_threshold(self):
        main.skill.config["streaming_policy"] = "local_agreement"
        engine.transcribe_words = lambda *a, **k: list(self.HALLUCINATED)

        def final_text(threshold):
            main.skill.config["silence_rms"] = threshold
            utt = engine.Utterance(id="u")
            utt.add(self._audio(1.5, 0.9), seq=0)
            engine.stream_partial(utt, "", 1, "base", None, threshold)
            ctx = FakeCtx({})
            ctx.sink = self.sink = []
            asyncio.run(main._finalize(ctx, utt, reason="silence"))
            return self.texts()[0]["text"]

        self.assertEqual(final_text(500), "de compras.")
        self.assertEqual(final_text(0), "de compras. No, no, no.")

    def test_partials_use_it_too(self):
        main.skill.config["streaming_policy"] = "local_agreement"
        engine.transcribe_words = lambda *a, **k: list(self.HALLUCINATED)
        utt = engine.Utterance(id="u")
        utt.add(self._audio(1.5, 0.9), seq=0)
        ctx = FakeCtx({})
        ctx.sink = self.sink

        asyncio.run(main._maybe_partial(ctx, utt))

        self.assertEqual([d["text"] for d in self.transcripts()], ["de compras."])


try:
    import numpy  # noqa: F401
    HAVE_NUMPY = True
except ImportError:  # CI installs only pyyaml + websockets
    HAVE_NUMPY = False


@unittest.skipUnless(HAVE_NUMPY, "numpy is not installed")
class ModelCalls(unittest.TestCase):
    """What actually reaches faster-whisper, against a stand-in model. Needs
    numpy (the PCM is converted before the call), so it skips in CI."""

    class Segment:
        def __init__(self, text, words=()):
            self.text = text
            self.words = list(words)

    class RawWord:
        def __init__(self, word, start, end):
            self.word, self.start, self.end = word, start, end

    class Info:
        language = "es"

    def setUp(self):
        self._load = engine.load_model
        self.calls = []
        test = self

        class Model:
            def transcribe(self, audio, **kwargs):
                test.calls.append((len(audio), kwargs))
                W = ModelCalls.RawWord
                return iter([
                    ModelCalls.Segment(" Hola mundo.", [W(" Hola", 0.0, 0.4), W(" mundo.", 0.4, 0.9)]),
                    ModelCalls.Segment(" 你好", [W("你", 1.0, 1.2), W("好", 1.2, 1.4)]),
                ]), ModelCalls.Info()

        engine.load_model = lambda size: Model()

    def tearDown(self):
        engine.load_model = self._load

    def test_redecode_calls_the_model_exactly_as_before(self):
        """The default path's decode arguments, pinned: no word timestamps,
        no prompt, faster-whisper's own temperature fallback."""
        text = engine.transcribe(b"\x00\x01" * 16000, 16000, "", 5, "base")

        self.assertEqual(text, "Hola mundo. 你好")
        self.assertEqual(self.calls, [(16000, {
            "beam_size": 5, "language": None, "condition_on_previous_text": False})])

    def test_streaming_decodes_ask_for_words_a_prompt_and_no_fallback(self):
        info = {}
        words = engine.transcribe_words(
            b"\x00\x01" * 16000, 16000, "es", 2, "base", None, "antes dije", info)

        self.assertEqual(self.calls[0][1], {
            "beam_size": 2, "language": "es", "condition_on_previous_text": False,
            "word_timestamps": True, "initial_prompt": "antes dije", "temperature": 0.0})
        self.assertEqual(info, {"language": "es"})
        self.assertEqual(words, [
            Word("Hola", 0.0, 0.4, space=True), Word("mundo.", 0.4, 0.9, space=True),
            Word("你", 1.0, 1.2, space=False), Word("好", 1.2, 1.4, space=False)])

    def test_an_empty_prompt_is_no_prompt(self):
        engine.transcribe_words(b"\x00\x01" * 1600, 16000, "", 1, "base")
        self.assertIsNone(self.calls[0][1]["initial_prompt"])
        self.assertIsNone(self.calls[0][1]["language"])

    def test_stop_between_segments_ends_the_decode_early(self):
        stop = threading.Event()
        real_segment = ModelCalls.Segment

        class StoppingSegment(real_segment):
            @property
            def words(self):
                stop.set()                      # cancelled while decoding this one
                return self._words

            @words.setter
            def words(self, value):
                self._words = value

        ModelCalls.Segment = StoppingSegment
        try:
            words = engine.transcribe_words(b"\x00\x01" * 16000, 16000, "", 1, "base", stop)
        finally:
            ModelCalls.Segment = real_segment
        self.assertEqual([w.text for w in words], ["Hola", "mundo."])

    def test_nothing_is_decoded_once_cancelled_or_without_audio(self):
        stop = threading.Event()
        stop.set()
        self.assertEqual(engine.transcribe_words(b"\x00\x01" * 100, 16000, "", 1, "base", stop), [])
        self.assertEqual(engine.transcribe_words(b"", 16000, "", 1, "base"), [])
        self.assertEqual(engine.transcribe(b"", 16000, "", 1, "base"), "")
        self.assertEqual(self.calls, [])


class LanguageLock(Wired):
    def setUp(self):
        super().setUp()
        main.skill.config["streaming_policy"] = "local_agreement"

    def test_autodetect_runs_on_the_whole_window_then_sticks(self):
        """Once the window is cut, the tail is too short to detect a language
        from — and a flip mid-sentence would be committed."""
        self.speak(6.0)
        from_start = [c for c in self.word_calls if c["start"] == 0.0]
        cut = [c for c in self.word_calls if c["start"] > 0.0]
        self.assertTrue(from_start and cut)
        self.assertTrue(all(c["language"] is None for c in from_start))
        self.assertTrue(all(c["language"] == "en" for c in cut))

    def test_a_configured_language_is_always_used(self):
        main.skill.config["language"] = "es"
        self.speak(6.0)
        self.assertEqual({c["language"] for c in self.word_calls}, {"es"})


class Cancellation(Wired):
    def test_a_partial_cancelled_mid_decode_changes_nothing(self):
        stop = threading.Event()
        utt = engine.Utterance(id="u")
        utt.add(marked_audio(2.0), seq=0)
        engine.stream_partial(utt, "", 1, "base", None)
        before = (utt.stream.decodes, utt.stream.volatile, utt.stream.window_start)

        inner = engine.transcribe_words

        def cancelled_halfway(*args, **kwargs):
            words = inner(*args, **kwargs)
            stop.set()
            return words[:1]                    # a truncated hypothesis

        engine.transcribe_words = cancelled_halfway
        with self.assertRaises(engine.DecodeSkipped):
            engine.stream_partial(utt, "", 1, "base", stop)
        self.assertEqual(
            (utt.stream.decodes, utt.stream.volatile, utt.stream.window_start), before)

    def test_a_cancelled_partial_emits_nothing_and_frees_the_utterance(self):
        main.skill.config["streaming_policy"] = "local_agreement"
        stop = threading.Event()
        stop.set()
        utt = engine.Utterance(id="u")
        utt.add(marked_audio(2.0), seq=0)
        ctx = FakeCtx({}, cancel_event=stop)
        ctx.sink = self.sink

        asyncio.run(main._maybe_partial(ctx, utt))

        self.assertEqual(self.sink, [])
        self.assertEqual(self.word_calls, [])
        self.assertFalse(utt.decoding)
        self.assertFalse(engine.gate.busy())

    def test_a_cancelled_redecode_partial_emits_nothing_either(self):
        stop = threading.Event()
        stop.set()
        utt = engine.Utterance(id="u")
        utt.add(marked_audio(2.0), seq=0)
        ctx = FakeCtx({}, cancel_event=stop)
        ctx.sink = self.sink

        asyncio.run(main._maybe_partial(ctx, utt))

        self.assertEqual(self.sink, [])
        self.assertFalse(utt.decoding)
        self.assertFalse(engine.gate.busy())

    def test_a_cancelled_final_still_emits_as_it_always_did(self):
        """Today a cancelled final decodes to "" and is still emitted; the
        gate must not turn that into silence."""
        stop = threading.Event()
        stop.set()
        utt = engine.Utterance(id="u")
        utt.add(marked_audio(1.0), seq=0)
        ctx = FakeCtx({}, cancel_event=stop)
        ctx.sink = self.sink

        asyncio.run(main._finalize(ctx, utt, reason="client"))

        self.assertEqual(self.sink, [
            ("transcript_out", {"text": "", "final": True, "replace": True, "utterance": "u"}),
            ("text_out", {"text": "", "final": True}),
        ])


class GateWiring(Wired):
    """main.py routes every decode through engine.gate."""

    def _hold_gate(self):
        release, started = threading.Event(), threading.Event()

        def hold():
            started.set()
            release.wait(5)

        t = threading.Thread(target=engine.gate.run, args=(hold,), daemon=True)
        t.start()
        self.assertTrue(started.wait(5))
        return release, t

    async def _until(self, predicate, timeout=5.0):
        deadline = time.monotonic() + timeout
        while not predicate():
            self.assertLess(time.monotonic(), deadline, "timed out")
            await asyncio.sleep(0.002)

    def _final_beats_queued_partial(self, policy):
        main.skill.config["streaming_policy"] = policy
        release, holder = self._hold_gate()
        utt = engine.Utterance(id="u")
        utt.add(marked_audio(2.0), seq=0)
        ctx = FakeCtx({})
        ctx.sink = self.sink

        async def run():
            before = engine.gate.queued
            partial = asyncio.create_task(main._maybe_partial(ctx, utt))
            await self._until(lambda: engine.gate.queued > before)
            final = asyncio.create_task(main._finalize(ctx, utt, reason="client"))
            # The partial is dropped while the model is still busy with
            # someone else — it does not wait for its turn to find out.
            await asyncio.wait_for(partial, 5)
            self.assertTrue(engine.gate.busy())
            release.set()
            await asyncio.wait_for(final, 5)

        asyncio.run(run())
        holder.join(5)
        return utt

    def test_redecode_final_supersedes_its_own_queued_partial(self):
        utt = self._final_beats_queued_partial("redecode")
        self.assertEqual(self.plain_calls, [{"seconds": 2.0, "beam": 5}])
        self.assertEqual([d["final"] for d in self.transcripts()], [True])
        self.assertFalse(utt.decoding)

    def test_local_agreement_final_supersedes_its_own_queued_partial(self):
        self._final_beats_queued_partial("local_agreement")
        self.assertEqual(self.word_calls, [])           # the partial never ran
        self.assertEqual(self.plain_calls, [{"seconds": 2.0, "beam": 5}])
        self.assertEqual([d["final"] for d in self.transcripts()], [True])

    def test_a_document_goes_through_the_gate_too(self):
        import io
        import wave

        buf = io.BytesIO()
        with wave.open(buf, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(RATE)
            w.writeframes(marked_audio(0.5))
        ctx = FakeCtx({"bytes_b64": base64.b64encode(buf.getvalue()).decode()})
        ctx.sink = self.sink
        ran = engine.gate.ran
        ready = engine.model_ready.is_set()
        engine.model_ready.set()      # otherwise a 'loading the model' status leads
        try:
            asyncio.run(main.handle_document(ctx))
        finally:
            if not ready:
                engine.model_ready.clear()

        self.assertEqual(engine.gate.ran, ran + 1)
        self.assertEqual(self.sink, [
            ("text_out", {"text": "decoded 8000 samples", "final": True}),
            ("transcript_out", {"text": "decoded 8000 samples", "final": True,
                                "replace": True}),
        ])

    def test_a_failing_decode_releases_the_gate(self):
        def boom(*args, **kwargs):
            raise RuntimeError("model fell over")

        engine.transcribe = boom
        utt = engine.Utterance(id="u")
        utt.add(marked_audio(1.0), seq=0)
        ctx = FakeCtx({})
        ctx.sink = self.sink

        asyncio.run(main._finalize(ctx, utt, reason="client"))

        self.assertEqual(self.sink, [("error", "transcription failed: model fell over")])
        self.assertFalse(engine.gate.busy())
        self.assertEqual(engine.gate.waiting(), 0)


class Device(unittest.TestCase):
    """device / compute_type, against a fake model factory."""

    def setUp(self):
        self._saved = (engine._model, engine._model_key, engine.loaded_on,
                       dict(engine._prefs), engine._make_model,
                       engine._cuda_available, engine.model_ready.is_set())
        engine._model = engine._model_key = engine.loaded_on = None
        engine._prefs.update(device="cpu", compute_type="int8")
        self.made = []
        self.fail_on = set()
        self.cuda = False

        def make(model_size, device, compute_type):
            self.made.append((model_size, device, compute_type))
            if device in self.fail_on:
                raise RuntimeError(f"cannot load on {device}")
            return object()

        engine._make_model = make
        engine._cuda_available = lambda: self.cuda

    def tearDown(self):
        (engine._model, engine._model_key, engine.loaded_on, prefs,
         engine._make_model, engine._cuda_available, ready) = self._saved
        engine._prefs.clear()
        engine._prefs.update(prefs)
        if ready:
            engine.model_ready.set()
        else:
            engine.model_ready.clear()

    def test_resolution_table(self):
        r = engine.resolve_device
        self.assertEqual(r("cpu", "int8"), ("cpu", "int8"))
        self.assertEqual(r("auto", "auto", cuda=True), ("cuda", "float16"))
        self.assertEqual(r("auto", "auto", cuda=False), ("cpu", "int8"))
        self.assertEqual(r("cuda", "auto"), ("cuda", "float16"))
        self.assertEqual(r("cpu", "auto"), ("cpu", "int8"))
        self.assertEqual(r("auto", "int8_float16", cuda=True), ("cuda", "int8_float16"))
        self.assertEqual(r("cpu", "float32"), ("cpu", "float32"))   # passed through

    def test_out_of_the_box_the_model_loads_on_cpu_int8(self):
        """What the hard-coded call used to do."""
        engine.load_model("base")
        self.assertEqual(self.made, [("base", "cpu", "int8")])
        self.assertEqual(engine.loaded_on, ("cpu", "int8"))
        self.assertTrue(engine.model_ready.is_set())

    def test_the_skill_defaults_configure_cpu_int8(self):
        main._configure_engine()
        engine.load_model("base")
        self.assertEqual(self.made, [("base", "cpu", "int8")])

    def test_the_same_request_is_served_from_the_cache(self):
        first = engine.load_model("base")
        self.assertIs(engine.load_model("base"), first)
        self.assertEqual(len(self.made), 1)

    def test_auto_picks_the_gpu_when_there_is_one(self):
        self.cuda = True
        engine.configure(device="auto", compute_type="auto")
        engine.load_model("small")
        self.assertEqual(self.made, [("small", "cuda", "float16")])
        self.assertEqual(engine.loaded_on, ("cuda", "float16"))

    def test_auto_without_a_gpu_is_cpu_int8(self):
        engine.configure(device="auto", compute_type="auto")
        engine.load_model("base")
        self.assertEqual(self.made, [("base", "cpu", "int8")])

    def test_auto_falls_back_to_cpu_when_the_gpu_load_fails_and_says_why(self):
        self.cuda = True
        self.fail_on = {"cuda"}
        engine.configure(device="auto", compute_type="auto")
        with self.assertLogs("asr.engine", level="WARNING") as logs:
            model = engine.load_model("base")

        self.assertIsNotNone(model)
        self.assertEqual(self.made, [("base", "cuda", "float16"), ("base", "cpu", "int8")])
        self.assertEqual(engine.loaded_on, ("cpu", "int8"))
        self.assertIn("cannot load on cuda", "\n".join(logs.output))
        self.assertIn("falling back to cpu/int8", "\n".join(logs.output))
        # The fallback is cached: the GPU is not retried on every decode.
        engine.load_model("base")
        self.assertEqual(len(self.made), 2)

    def test_auto_fallback_ignores_an_explicit_gpu_compute_type(self):
        self.cuda = True
        self.fail_on = {"cuda"}
        engine.configure(device="auto", compute_type="float16")
        with self.assertLogs("asr.engine", level="WARNING"):
            engine.load_model("base")
        self.assertEqual(self.made[-1], ("base", "cpu", "int8"))

    def test_explicit_cuda_fails_loudly_instead_of_falling_back(self):
        self.fail_on = {"cuda"}
        engine.configure(device="cuda", compute_type="float16")
        with self.assertRaises(RuntimeError):
            engine.load_model("base")
        self.assertEqual(self.made, [("base", "cuda", "float16")])
        self.assertIsNone(engine._model)

    def test_a_cpu_failure_is_not_retried_on_cpu(self):
        self.fail_on = {"cpu"}
        engine.configure(device="auto", compute_type="auto")
        with self.assertRaises(RuntimeError):
            engine.load_model("base")
        self.assertEqual(len(self.made), 1)

    def test_a_changed_config_loads_again(self):
        """The boot preload runs on manifest defaults, before the kernel has
        sent the effective config. What it loaded must not pin the device."""
        engine.load_model("base")                      # the preload
        self.cuda = True
        engine.configure(device="auto", compute_type="auto")
        engine.load_model("base")                      # first decode
        engine.load_model("small")
        self.assertEqual(self.made, [
            ("base", "cpu", "int8"), ("base", "cuda", "float16"),
            ("small", "cuda", "float16")])

    def test_configure_ignores_empty_values(self):
        engine.configure(device="cuda", compute_type="float16")
        engine.configure(device=None, compute_type="")
        self.assertEqual(engine._prefs, {"device": "cuda", "compute_type": "float16"})

    def test_cuda_probe_without_ctranslate2_is_simply_no(self):
        _, _, _, _, _, real_probe, _ = self._saved
        try:
            import ctranslate2  # noqa: F401
        except ImportError:
            self.assertIs(real_probe(), False)
        else:
            self.assertIn(real_probe(), (True, False))


if __name__ == "__main__":
    unittest.main()
