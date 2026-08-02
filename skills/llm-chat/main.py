"""
llm-chat — cognitive.llm.chat.

Serves a local GGUF via llama.cpp by default (downloads a small model on
first start, no account and no network needed after that), or any
OpenAI-compatible cloud model — including Gemini — if OPENAI_API_KEY is set.
Per-session conversation history, token streaming. See aura.llm.ChatBackend
for the backend-selection logic shared with skills/vision-reasoner.

Runtime-tunable (C1 `config`, see skill.yaml): temperature, max_tokens,
system_prompt (hot — applied on the next message) and context_window
(restart_required — only takes effect on the next process start, since
the local model's context size is fixed at load time). Set via the
control-plane UI, a --config file, or PUT /v1/skills/config?id=<id>.

Config (env, model selection only — not runtime-tunable):
  AURA_MODEL_PATH   absolute path to a local .gguf (skips download)
  AURA_MODEL_REPO   HF repo   (default: Qwen/Qwen2.5-1.5B-Instruct-GGUF)
  AURA_MODEL_FILE   HF file   (default: qwen2.5-1.5b-instruct-q4_k_m.gguf)
  OPENAI_API_KEY    set to switch this skill to a cloud model instead of local
  OPENAI_BASE_URL   OpenAI-compatible endpoint (default: api.openai.com/v1;
                     for Gemini: https://generativelanguage.googleapis.com/v1beta/openai/)
  OPENAI_MODEL      model name at that endpoint (default: gpt-4o-mini)
"""
import asyncio
import contextlib
import logging
import os
import threading
from dataclasses import dataclass, field

from aura import ChatBackend, Context, Skill, run_all

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("llm-chat")

MODEL_REPO = os.getenv("AURA_MODEL_REPO", "Qwen/Qwen2.5-1.5B-Instruct-GGUF")
MODEL_FILE = os.getenv("AURA_MODEL_FILE", "qwen2.5-1.5b-instruct-q4_k_m.gguf")

skill = Skill()
# context_window is restart_required (see skill.yaml): read once, at the
# declared/file default available before the kernel connection even opens —
# a value changed live only takes effect after this process restarts.
backend = ChatBackend(
    model_repo=MODEL_REPO, model_file=MODEL_FILE, ctx=skill.config["context_window"],
)

_history: dict[str, list[dict]] = {}


@dataclass
class Generation:
    """One in-flight reply, and the two ways it can be stopped."""
    stop: threading.Event = field(default_factory=threading.Event)
    finished: asyncio.Event = field(default_factory=asyncio.Event)


# The newest question for a session is the one that matters. Serialising with
# a lock instead — as this did — makes an interruption WAIT for the very reply
# it is interrupting: the new handler blocks on the lock, and the cancel that
# would release it names the old generation's cause_id, so it never arrives.
# In a voice conversation that is the difference between "stop, I meant
# something else" working and being ignored.
_current: dict[str, Generation] = {}


def _produce(messages, max_tokens, temperature, stop, queue, loop) -> None:
    """Run the model in a worker thread, pushing tokens to the event loop.

    `stream_chat` is a synchronous generator. Iterating it on the loop blocks
    the skill's socket the whole time a token takes — it only worked because
    `await asyncio.sleep(0)` per token happened to let one inbound message
    through, which is an accident, not a design. Off the loop, cancel latency
    is bounded by a queue hop rather than by however long a token takes.
    """
    try:
        for token in backend.stream_chat(
                messages, max_tokens=max_tokens, temperature=temperature):
            if stop.is_set():
                break
            loop.call_soon_threadsafe(queue.put_nowait, token)
    except Exception as exc:  # noqa: BLE001
        loop.call_soon_threadsafe(queue.put_nowait, exc)
    finally:
        loop.call_soon_threadsafe(queue.put_nowait, None)  # end of stream


async def _preempt(session: str) -> None:
    """Stop whatever this session was saying, and wait for it to let go."""
    previous = _current.get(session)
    if previous is None:
        return
    previous.stop.set()
    with contextlib.suppress(asyncio.TimeoutError):
        await asyncio.wait_for(previous.finished.wait(), timeout=2.0)


@skill.on("text_in")
async def handle(ctx: Context) -> None:
    text = ((ctx.payload or {}).get("text") or "").strip()
    if not text:
        return

    await _preempt(ctx.session)
    if ctx.cancelled:
        return

    generation = Generation()
    _current[ctx.session] = generation

    if not backend.is_cloud:
        await ctx.status("status_out", "working", "loading local model...")

    messages = _history.setdefault(
        ctx.session, [{"role": "system", "content": skill.config["system_prompt"]}]
    )
    messages.append({"role": "user", "content": text})

    queue: asyncio.Queue = asyncio.Queue()
    loop = asyncio.get_running_loop()
    worker = asyncio.create_task(asyncio.to_thread(
        _produce, list(messages), skill.config["max_tokens"],
        skill.config["temperature"], generation.stop, queue, loop))

    reply_parts: list[str] = []
    interrupted = False
    try:
        while True:
            item = await queue.get()
            if item is None:
                break
            if isinstance(item, Exception):
                await ctx.error("status_out", f"generation failed: {item}")
                interrupted = True
                break
            # Either the listener abandoned this chain, or a newer question
            # arrived. Both mean: stop producing words nobody wants.
            if ctx.cancelled or generation.stop.is_set():
                generation.stop.set()
                interrupted = True
                log.info("stopped mid-reply (session %s)", ctx.session)
                break
            reply_parts.append(item)
            await ctx.emit("text_out", {"text": item, "final": False})
    finally:
        generation.stop.set()
        generation.finished.set()
        if _current.get(ctx.session) is generation:
            _current.pop(ctx.session, None)
        with contextlib.suppress(Exception):
            await worker

    await ctx.emit("text_out", {"text": "", "final": True})

    # Record what was actually said, marked if it was cut short — otherwise the
    # next turn is answered against a reply the listener never heard in full.
    reply_text = "".join(reply_parts) + (" [interrupted]" if interrupted else "")
    messages.append({"role": "assistant", "content": reply_text})

    if len(messages) > 40:
        del messages[1:3]


if __name__ == "__main__":
    run_all([skill])
