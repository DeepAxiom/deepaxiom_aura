"""Tests for the parts of llm-chat that only show up under load or over a long
conversation: token usage on the attestation, coalesced deltas, a history
whose prefix survives trimming, and two sessions sharing one local context.

No llama-cpp-python and no network — the backend is faked.

    cd expertise/llm-chat && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import threading
import time
import unittest
from unittest.mock import patch

from aura.llm import ChatBackend

import main


class _Ctx:
    def __init__(self, session: str, text: str) -> None:
        self.payload = {"text": text}
        self.session = session
        self.cancelled = False
        self.emitted: list[tuple[str, dict, object]] = []

    async def status(self, port, state, detail=""):
        pass

    async def emit(self, port, payload, attest=None, **_kwargs):
        self.emitted.append((port, payload, attest))

    async def error(self, port, detail):
        self.emitted.append((port, {"error": detail}, None))


def _text(ctx: _Ctx) -> str:
    return "".join(p["text"] for port, p, _ in ctx.emitted if port == "text_out")


class _ConfigIsolation(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        main._history.clear()
        main._current.clear()
        self._saved = dict(main.skill.config)

    async def asyncTearDown(self):
        main.skill.config.clear()
        main.skill.config.update(self._saved)


class Coalescing(_ConfigIsolation):
    async def test_a_fast_stream_is_fewer_envelopes_and_the_same_text(self):
        tokens = [f"t{i} " for i in range(200)]
        main.skill.config["coalesce_ms"] = 25
        ctx = _Ctx("s-fast", "hello")
        with patch.object(main.backend, "stream_chat", return_value=iter(tokens)):
            await main.handle(ctx)

        deltas = [p for port, p, _ in ctx.emitted if port == "text_out" and not p["final"]]
        self.assertEqual(_text(ctx), "".join(tokens))
        self.assertLess(len(deltas), len(tokens) // 4,
                        "200 tokens produced back to back should not be 200 envelopes")
        # The final marker is still its own, empty, envelope — consumers key
        # on it and it carries the attestation.
        self.assertEqual(ctx.emitted[-1][1], {"text": "", "final": True})

    async def test_zero_restores_one_envelope_per_token(self):
        tokens = ["a", "b", "c", "d"]
        main.skill.config["coalesce_ms"] = 0
        ctx = _Ctx("s-off", "hello")
        with patch.object(main.backend, "stream_chat", return_value=iter(tokens)):
            await main.handle(ctx)
        deltas = [p["text"] for port, p, _ in ctx.emitted
                  if port == "text_out" and not p["final"]]
        self.assertEqual(deltas, tokens)

    async def test_history_records_exactly_what_was_sent(self):
        main.skill.config["coalesce_ms"] = 25
        ctx = _Ctx("s-hist", "hello")
        with patch.object(main.backend, "stream_chat",
                          return_value=iter(["one ", "two ", "three"])):
            await main.handle(ctx)
        self.assertEqual(main._history["s-hist"][-1]["content"], _text(ctx))

    async def test_a_manifest_default_exists_so_an_old_kernel_changes_nothing_else(self):
        # skill.config starts from the manifest defaults, so a kernel that
        # predates the key still leaves the skill with a value.
        self.assertEqual(self._saved["coalesce_ms"], 25)


class UsageOnTheAttestation(_ConfigIsolation):
    async def test_engine_reported_usage_reaches_the_final_envelope(self):
        def fake_stream(messages, *, usage=None, stop=None, **_kw):
            yield "hi"
            usage.update({"input_tokens": 120, "output_tokens": 1,
                          "cached_input_tokens": 100})

        ctx = _Ctx("s-usage", "hello")
        with patch.object(main.backend, "stream_chat", side_effect=fake_stream):
            await main.handle(ctx)

        attest = ctx.emitted[-1][2]
        self.assertEqual(
            attest.to_wire()["usage"],
            {"input_tokens": 120, "output_tokens": 1, "cached_input_tokens": 100})

    async def test_no_usage_means_no_field_rather_than_a_guess(self):
        ctx = _Ctx("s-nousage", "hello")
        with patch.object(main.backend, "stream_chat", return_value=iter(["hi"])):
            await main.handle(ctx)
        self.assertNotIn("usage", ctx.emitted[-1][2].to_wire())


class StablePrefix(_ConfigIsolation):
    """The property the trim exists for: between compactions, each prompt is
    the previous conversation plus something at the end."""

    async def test_prompts_extend_each_other_except_at_a_compaction(self):
        main.skill.config["history_limit"] = 12
        prompts: list[list[dict]] = []

        def fake_stream(messages, **_kw):
            prompts.append([dict(m) for m in messages])
            return iter(["ok"])

        with patch.object(main.backend, "stream_chat", side_effect=fake_stream):
            for i in range(40):
                await main.handle(_Ctx("s-prefix", f"q{i}"))

        resets = 0
        for before, after in zip(prompts, prompts[1:]):
            if after[:len(before)] != before:
                resets += 1
        # The old one-turn-at-a-time trim reset the prefix on every turn past
        # the limit — 34 of these 39 steps. In blocks, at this deliberately
        # tiny limit, it is one step in three.
        self.assertLessEqual(resets, 14)
        self.assertGreater(resets, 0, "a 40-turn conversation at limit 12 must compact")
        for prompt in prompts:
            self.assertLessEqual(len(prompt), 12 + 1)
            self.assertEqual(prompt[0]["role"], "system")
            self.assertEqual(prompt[1]["role"], "user")


class _SlowLlama:
    """A stand-in for llama_cpp.Llama that notices concurrent generation."""

    def __init__(self) -> None:
        self.active = 0
        self.max_active = 0
        self.n_tokens = 0
        self._lock = threading.Lock()

    def create_chat_completion(self, **_kwargs):
        def gen():
            with self._lock:
                self.active += 1
                self.max_active = max(self.max_active, self.active)
            try:
                for i in range(5):
                    time.sleep(0.01)
                    yield {"choices": [{"delta": {"content": f"{i}"}}]}
            finally:
                with self._lock:
                    self.active -= 1
        return gen()


def _local_backend(llama) -> ChatBackend:
    backend = ChatBackend()
    backend._api_key = None
    backend._inference_url = ""
    backend._load_local = lambda: llama
    return backend


class OneSequenceAtATime(unittest.TestCase):
    def test_two_sessions_never_generate_on_the_context_at_once(self):
        llama = _SlowLlama()
        backend = _local_backend(llama)
        results: dict[int, str] = {}

        def run(n: int) -> None:
            results[n] = "".join(backend.stream_chat(
                [{"role": "user", "content": "hey"}], max_tokens=8, temperature=0.0))

        threads = [threading.Thread(target=run, args=(n,)) for n in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=10)

        self.assertEqual(llama.max_active, 1)
        self.assertEqual(results, {n: "01234" for n in range(4)})

    def test_a_reply_cancelled_while_queued_gives_up_its_place(self):
        llama = _SlowLlama()
        backend = _local_backend(llama)

        backend._local_gate.acquire()  # someone else is generating
        stop = threading.Event()
        out: list[str] = []
        t = threading.Thread(target=lambda: out.extend(backend.stream_chat(
            [{"role": "user", "content": "hey"}], stop=stop)))
        t.start()
        time.sleep(0.1)
        stop.set()
        t.join(timeout=2)
        self.assertFalse(t.is_alive(), "a cancelled reply must not wait for the model")
        self.assertEqual(out, [])
        backend._local_gate.release()
        self.assertEqual(llama.max_active, 0)

    def test_an_abandoned_generator_releases_the_model(self):
        backend = _local_backend(_SlowLlama())
        stream = backend.stream_chat([{"role": "user", "content": "hey"}])
        next(stream)
        stream.close()
        self.assertTrue(backend._local_gate.acquire(timeout=1))
        backend._local_gate.release()

    def test_local_usage_is_read_off_the_context(self):
        llama = _SlowLlama()
        llama.n_tokens = 47  # prompt + the 5 tokens generated
        backend = _local_backend(llama)
        usage: dict = {}
        list(backend.stream_chat([{"role": "user", "content": "hey"}], usage=usage))
        self.assertEqual(usage, {"input_tokens": 42, "output_tokens": 5})


if __name__ == "__main__":
    unittest.main()
