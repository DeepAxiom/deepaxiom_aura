"""
asr — sensorial.asr.transcribe (model plane driver: faster-whisper).

Two ways in, because a voice assistant and a document pipeline want different
things:

  audio_in        std/document@1     a whole WAV, transcribed once
  audio_chunk_in  std/audio-chunk@1  a live stream of PCM, transcribed as it
                                     arrives, with partial results

Two ways out, and the distinction matters:

  transcript_out  std/transcript@1   partials AND the final — each REPLACES the
                                     previous hypothesis, for a UI to show
  text_out        std/text@1         the final only — for an LLM to answer

Partials must not go to text_out. There, `text` is a delta that consumers
concatenate; here every hypothesis supersedes the last, because whisper
re-decodes its whole buffer and hypothesis 3 is not hypothesis 2 plus a
suffix. Sending partials as std/text@1 would make a chat skill answer four
half-sentences instead of one question.

Endpointing (deciding an utterance ended) belongs in the client, which knows
when the user started speaking without a round trip. This skill still carries
its own safety net — trailing silence and a hard cap — so it stays usable from
curl, from a test, and from a client that forgets.

The faster-whisper wrapper, utterance buffering and the abandoned-utterance
sweep live in `engine.py`, kept separate so they can be unit tested without
the model installed. This module stays the C1 glue: the `@skill.on` handlers,
plus reading `skill.config` and handing plain values to `engine`.

Config (see skill.yaml): model_size, language, partials_enabled,
partial_interval_ms, max_utterance_s, silence_endpoint_ms, silence_rms,
preload_on_boot, beam_size_final, beam_size_partial.
"""
import asyncio
import base64
import io
import logging
import threading
import time

import engine
from aura import Context, Skill, run_all

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("asr")

skill = Skill()

# Below this much speech a hypothesis is not worth showing anyone.
MIN_PARTIAL_SECONDS = 1.0


def _wav_pcm(wav_bytes: bytes) -> tuple[bytes, int]:
    import wave

    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        return w.readframes(w.getnframes()), w.getframerate()


# ── the batch path (unchanged behaviour) ──────────────────────────────────

@skill.on("audio_in")
async def handle_document(ctx: Context) -> None:
    payload = ctx.payload or {}
    b64 = payload.get("bytes_b64", "")
    if not b64:
        await ctx.error("status_out", "audio_in expects std/document@1 with bytes_b64")
        return
    if not engine.model_ready.is_set():
        await ctx.status("status_out", "working", "loading the speech model...")
    try:
        pcm, rate = await asyncio.to_thread(_wav_pcm, base64.b64decode(b64))
        language = payload.get("language") or skill.config["language"]
        text = await asyncio.to_thread(
            engine.transcribe, pcm, rate, language,
            skill.config["beam_size_final"], skill.config["model_size"],
            ctx.cancel_event)
        await ctx.emit("text_out", {"text": text, "final": True})
        await ctx.emit("transcript_out", {"text": text, "final": True, "replace": True})
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"transcription failed: {exc}")


# ── the streaming path ────────────────────────────────────────────────────

@skill.on("audio_chunk_in")
async def handle_chunk(ctx: Context) -> None:
    payload = ctx.payload or {}
    b64 = payload.get("pcm_b64")
    if b64 is None:
        await ctx.error("status_out", "audio_chunk_in expects std/audio-chunk@1")
        return

    key = (ctx.session, ctx.node)
    # Everything that mutates shared state happens BEFORE the first await.
    # The SDK starts one task per envelope, and tasks run to their first
    # suspension in arrival order — so this is what keeps chunks in order.
    utt = engine.utterances.get(key)
    if utt is None:
        utt = engine.Utterance(id=f"{ctx.session}:{int(time.time() * 1000)}")
        engine.utterances[key] = utt
    engine.last_seen[key] = time.monotonic()

    try:
        pcm = base64.b64decode(b64)
    except Exception:  # noqa: BLE001
        await ctx.error("status_out", "pcm_b64 is not valid base64")
        return

    utt.sample_rate = int(payload.get("sample_rate") or utt.sample_rate)
    utt.add(pcm, payload.get("seq"))

    chunk_ms = (len(pcm) / 2 / max(utt.sample_rate, 1)) * 1000
    if engine.rms(pcm) < skill.config["silence_rms"]:
        utt.silent_ms += chunk_ms
    else:
        utt.silent_ms = 0.0

    client_final = bool(payload.get("final"))
    hit_silence = utt.silent_ms >= skill.config["silence_endpoint_ms"]
    too_long = utt.duration() >= skill.config["max_utterance_s"]

    if client_final or hit_silence or too_long:
        engine.utterances.pop(key, None)
        engine.last_seen.pop(key, None)
        await _finalize(ctx, utt, reason="client" if client_final
                        else "silence" if hit_silence else "max-length")
        return

    engine.sweep()
    await _maybe_partial(ctx, utt)


async def _maybe_partial(ctx: Context, utt: engine.Utterance) -> None:
    """Emit a partial if enough new audio has arrived and the machine kept up.

    whisper has no incremental decode: a partial costs a full re-decode of the
    buffer. On a slow machine that would fall further behind every round, so
    the interval adapts — and if it degrades to nothing, the final still lands.
    """
    if not skill.config["partials_enabled"] or utt.decoding:
        return

    interval = skill.config["partial_interval_ms"] / 1000
    new_bytes = len(utt.pcm()) - utt.bytes_at_last_partial
    if new_bytes / 2 / max(utt.sample_rate, 1) < interval:
        return
    # A hypothesis from under a second of speech is noise — measured on this
    # model, one second yields something like "Tern de Kichele" for "turn on
    # the kitchen light". Flashing that into a UI and replacing it is worse
    # than showing nothing yet.
    if utt.duration() < MIN_PARTIAL_SECONDS:
        return
    if utt.last_decode_cost > interval:
        # Falling behind: a decode costs roughly the same whatever the buffer
        # holds (whisper pads to a fixed 30s window), so if one round takes
        # longer than the interval, the next partial would already be stale
        # before it existed. Skip and let the audio catch up.
        utt.bytes_at_last_partial = len(utt.pcm())
        return

    utt.decoding = True
    utt.bytes_at_last_partial = len(utt.pcm())
    started = time.monotonic()
    try:
        text = await asyncio.to_thread(
            engine.transcribe, utt.pcm(max_seconds=10), utt.sample_rate,
            skill.config["language"], skill.config["beam_size_partial"],
            skill.config["model_size"], ctx.cancel_event)
    except Exception as exc:  # noqa: BLE001
        log.warning("partial decode failed: %s", exc)
        return
    finally:
        utt.last_decode_cost = time.monotonic() - started
        utt.decoding = False

    if text and not ctx.cancelled:
        await ctx.emit("transcript_out", {
            "text": text, "final": False, "replace": True, "utterance": utt.id})


async def _finalize(ctx: Context, utt: engine.Utterance, reason: str) -> None:
    if utt.gaps():
        # Say so rather than pretend: the transcript is of audio with holes.
        log.warning("utterance %s is missing %d chunk(s)", utt.id, utt.gaps())
        await ctx.status("status_out", "working",
                         f"{utt.gaps()} audio chunk(s) were dropped in transit")
    try:
        text = await asyncio.to_thread(
            engine.transcribe, utt.pcm(), utt.sample_rate,
            skill.config["language"], skill.config["beam_size_final"],
            skill.config["model_size"], ctx.cancel_event)
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"transcription failed: {exc}")
        return

    log.info("utterance %s: %.1fs, ended by %s -> %r",
             utt.id, utt.duration(), reason, text)
    await ctx.emit("transcript_out", {
        "text": text, "final": True, "replace": True, "utterance": utt.id})
    # Only the settled transcript goes to text_out, where a chat skill is
    # listening and would otherwise answer every partial.
    await ctx.emit("text_out", {"text": text, "final": True})


def _preload_if_configured() -> None:
    """Warm the model in a background thread when `preload_on_boot` is set, so
    the first utterance does not pay a 10-30s model load and time out.
    Replaces the old AURA_ASR_PRELOAD env var read (skill.config carries the
    effective value from boot, per Skill.__init__)."""
    if skill.config["preload_on_boot"]:
        threading.Thread(
            target=engine.load_model, args=(skill.config["model_size"],),
            daemon=True).start()


if __name__ == "__main__":
    _preload_if_configured()
    run_all([skill])
