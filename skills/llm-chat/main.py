"""
llm-chat — cognitive.llm.chat.

Serves a local GGUF via llama.cpp by default (downloads a small model on
first start, no account and no network needed after that), or any
OpenAI-compatible cloud model — including Gemini — if OPENAI_API_KEY is set.
Per-session conversation history, token streaming. See aura.llm.ChatBackend
for the backend-selection logic — shared with skills/memory-context's
summarizer, the other first-party consumer of the same local-or-cloud chat
client.

Runtime-tunable (C1 `config`, see skill.yaml): temperature, max_tokens,
top_p, top_k, repeat_penalty, history_limit and system_prompt (all hot —
applied on the next message) and context_window (restart_required — only
takes effect on the next process start, since the local model's context
size is fixed at load time). top_k/repeat_penalty only affect the local
GGUF backend — the cloud (OpenAI-compatible) path silently ignores them,
see aura.llm.ChatBackend. Set via the control-plane UI, a --config file,
or PUT /v1/skills/config?id=<id>.

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

import models
from aura import ChatBackend, Context, Skill, run_all, sha256_text

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("llm-chat")

skill = Skill()

# Which model, and from where — see models.py for the order. This used to be two
# constants, so a model downloaded through `model-manager` changed nothing here.
_choice = models.resolve(skill.config.get("model", ""))
log.info("model: %s (via %s)",
         _choice.get("path") or f"{_choice['repo']}/{_choice['file']}", _choice["source"])

if "path" in _choice:
    # ChatBackend already reads AURA_MODEL_PATH, so a resolved local file is
    # handed over the way the SDK already understands rather than by widening
    # its constructor — the SDK is Apache-2.0 and its surface is a contract.
    os.environ["AURA_MODEL_PATH"] = _choice["path"]

# context_window and model are restart_required (see skill.yaml): read once, at
# the declared/file default available before the kernel connection even opens —
# a value changed live only takes effect after this process restarts.
backend = ChatBackend(
    model_repo=_choice.get("repo", models.DEFAULT_REPO),
    model_file=_choice.get("file", models.DEFAULT_FILE),
    ctx=skill.config["context_window"],
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


def _generation_kwargs(config: dict) -> dict:
    """Map `skill.config` to the kwargs `ChatBackend.stream_chat` accepts.

    Pure so it can be tested without a live backend or kernel connection.
    """
    return {
        "max_tokens": config["max_tokens"],
        "temperature": config["temperature"],
        "top_p": config["top_p"],
        "top_k": config["top_k"],
        "repeat_penalty": config["repeat_penalty"],
    }


def _attestation(config: dict, messages: list[dict], reply: str):
    """Build the C5 record for the reply just produced.

    The prompt is hashed rather than carried: an attestation must not become a
    permanent, undeletable copy of what a user typed. The hash still binds the
    record to that exact input, which is all an auditor comparing two runs
    needs.

    Only the last user turn is hashed, not the whole history — the history is
    already reconstructable from the session's causal event log, and hashing
    it would make the attestation change every turn for reasons unrelated to
    the inference.
    """
    last_user = next(
        (m["content"] for m in reversed(messages) if m.get("role") == "user"), "")
    att = backend.attestation(**_generation_kwargs(config))
    att.prompt_sha256 = sha256_text(last_user)
    att.output_sha256 = sha256_text(reply)
    return att


def _produce(messages, generation_kwargs, stop, queue, loop) -> None:
    """Run the model in a worker thread, pushing tokens to the event loop.

    `stream_chat` is a synchronous generator. Iterating it on the loop blocks
    the skill's socket the whole time a token takes — it only worked because
    `await asyncio.sleep(0)` per token happened to let one inbound message
    through, which is an accident, not a design. Off the loop, cancel latency
    is bounded by a queue hop rather than by however long a token takes.
    """
    try:
        for token in backend.stream_chat(messages, **generation_kwargs):
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
        _produce, list(messages), _generation_kwargs(skill.config),
        generation.stop, queue, loop))

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

    reply_text = "".join(reply_parts) + (" [interrupted]" if interrupted else "")

    # C5: attest on the final chunk, once per reply rather than once per token.
    # The kernel de-duplicates identical records anyway, but a per-token
    # attestation would put a few hundred redundant bytes on every frame of a
    # streaming reply for nothing.
    #
    # This is what makes any downstream effect — a TTS clause spoken, an ERP
    # row written — cite the model that argued for it. Bound to the *whole*
    # reply, which is why prompt/output hashes are computed here and not
    # mid-stream.
    await ctx.emit(
        "text_out", {"text": "", "final": True},
        attest=_attestation(skill.config, messages, reply_text),
    )

    # Record what was actually said, marked if it was cut short — otherwise the
    # next turn is answered against a reply the listener never heard in full.
    messages.append({"role": "assistant", "content": reply_text})

    if len(messages) > skill.config["history_limit"]:
        del messages[1:3]


if __name__ == "__main__":
    run_all([skill])
