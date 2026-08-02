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

Config (see skill.yaml): model_size, language, partials_enabled,
partial_interval_ms, max_utterance_s, silence_endpoint_ms, silence_rms.
"""
import asyncio
import base64
import io
import logging
import math
import os
import threading
import time
from dataclasses import dataclass, field

from aura import Context, Skill, run_all

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("asr")

skill = Skill()

# Below this much speech a hypothesis is not worth showing anyone.
MIN_PARTIAL_SECONDS = 1.0

_model = None
_model_lock = threading.Lock()
_model_ready = threading.Event()


def _load():
    """Load once. Called from a warm-up thread at start so the first utterance
    does not pay a 10-30 second model load and time out."""
    global _model
    with _model_lock:
        if _model is None:
            from faster_whisper import WhisperModel

            size = skill.config["model_size"]
            log.info("loading whisper %s (CPU int8) ...", size)
            _model = WhisperModel(size, device="cpu", compute_type="int8")
            log.info("model ready")
        _model_ready.set()
        return _model


# ── per-utterance state ───────────────────────────────────────────────────

@dataclass
class Utterance:
    """One in-progress utterance for one (session, node).

    Chunks are kept by `seq` rather than appended, so a gap left by a dropped
    frame on a realtime channel is visible instead of silently splicing two
    unrelated moments of speech together.
    """
    id: str
    sample_rate: int = 16000
    chunks: dict[int, bytes] = field(default_factory=dict)
    started: float = field(default_factory=time.monotonic)
    bytes_at_last_partial: int = 0
    last_decode_cost: float = 0.0
    decoding: bool = False
    silent_ms: float = 0.0
    next_seq: int = 0

    def add(self, pcm: bytes, seq: int | None) -> None:
        if seq is None:
            seq = self.next_seq
        self.next_seq = max(self.next_seq, seq + 1)
        self.chunks[seq] = pcm

    def pcm(self, max_seconds: float | None = None) -> bytes:
        data = b"".join(self.chunks[k] for k in sorted(self.chunks))
        if max_seconds is not None:
            keep = int(max_seconds * self.sample_rate) * 2
            if len(data) > keep:
                data = data[-keep:]
        return data

    def gaps(self) -> int:
        return self.next_seq - len(self.chunks)

    def duration(self) -> float:
        return len(self.pcm()) / 2 / max(self.sample_rate, 1)


_utterances: dict[tuple[str, str], Utterance] = {}
_last_seen: dict[tuple[str, str], float] = {}


def _sweep() -> None:
    """Drop abandoned utterances. The SDK has no session-end hook, so without
    this a node that runs for weeks accumulates the tail of every conversation
    that was cut off mid-sentence."""
    cutoff = time.monotonic() - 120
    for key, seen in list(_last_seen.items()):
        if seen < cutoff:
            _utterances.pop(key, None)
            _last_seen.pop(key, None)


def _rms(pcm: bytes) -> float:
    """Loudness of a chunk, without a model. Used only to notice that the
    speaker stopped — cheap enough to run on every chunk."""
    if len(pcm) < 2:
        return 0.0
    import array

    samples = array.array("h")
    samples.frombytes(pcm[: len(pcm) // 2 * 2])
    if not samples:
        return 0.0
    return math.sqrt(sum(float(s) * s for s in samples) / len(samples))


def _transcribe(pcm: bytes, sample_rate: int, language: str | None,
                beam: int, stop: threading.Event | None) -> str:
    """Blocking decode. Runs in a worker thread; `stop` lets a barge-in abandon
    a partial nobody is waiting for any more."""
    if stop is not None and stop.is_set():
        return ""
    model = _load()
    if not pcm:
        return ""
    import numpy as np

    audio = np.frombuffer(pcm, dtype=np.int16).astype(np.float32) / 32768.0
    if sample_rate != 16000:
        # whisper wants 16k; resampling badly is worse than saying so.
        idx = np.linspace(0, len(audio) - 1, int(len(audio) * 16000 / sample_rate))
        audio = np.interp(idx, np.arange(len(audio)), audio).astype(np.float32)
    segments, _ = model.transcribe(
        audio, beam_size=beam, language=language or None,
        condition_on_previous_text=False,
    )
    return " ".join(seg.text.strip() for seg in segments).strip()


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
    if not _model_ready.is_set():
        await ctx.status("status_out", "working", "loading the speech model...")
    try:
        pcm, rate = await asyncio.to_thread(_wav_pcm, base64.b64decode(b64))
        language = payload.get("language") or skill.config["language"]
        text = await asyncio.to_thread(
            _transcribe, pcm, rate, language, 5, ctx.cancel_event)
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
    utt = _utterances.get(key)
    if utt is None:
        utt = Utterance(id=f"{ctx.session}:{int(time.time() * 1000)}")
        _utterances[key] = utt
    _last_seen[key] = time.monotonic()

    try:
        pcm = base64.b64decode(b64)
    except Exception:  # noqa: BLE001
        await ctx.error("status_out", "pcm_b64 is not valid base64")
        return

    utt.sample_rate = int(payload.get("sample_rate") or utt.sample_rate)
    utt.add(pcm, payload.get("seq"))

    chunk_ms = (len(pcm) / 2 / max(utt.sample_rate, 1)) * 1000
    if _rms(pcm) < skill.config["silence_rms"]:
        utt.silent_ms += chunk_ms
    else:
        utt.silent_ms = 0.0

    client_final = bool(payload.get("final"))
    hit_silence = utt.silent_ms >= skill.config["silence_endpoint_ms"]
    too_long = utt.duration() >= skill.config["max_utterance_s"]

    if client_final or hit_silence or too_long:
        _utterances.pop(key, None)
        _last_seen.pop(key, None)
        await _finalize(ctx, utt, reason="client" if client_final
                        else "silence" if hit_silence else "max-length")
        return

    _sweep()
    await _maybe_partial(ctx, utt)


async def _maybe_partial(ctx: Context, utt: Utterance) -> None:
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
            _transcribe, utt.pcm(max_seconds=10), utt.sample_rate,
            skill.config["language"], 1, ctx.cancel_event)
    except Exception as exc:  # noqa: BLE001
        log.warning("partial decode failed: %s", exc)
        return
    finally:
        utt.last_decode_cost = time.monotonic() - started
        utt.decoding = False

    if text and not ctx.cancelled:
        await ctx.emit("transcript_out", {
            "text": text, "final": False, "replace": True, "utterance": utt.id})


async def _finalize(ctx: Context, utt: Utterance, reason: str) -> None:
    if utt.gaps():
        # Say so rather than pretend: the transcript is of audio with holes.
        log.warning("utterance %s is missing %d chunk(s)", utt.id, utt.gaps())
        await ctx.status("status_out", "working",
                         f"{utt.gaps()} audio chunk(s) were dropped in transit")
    try:
        text = await asyncio.to_thread(
            _transcribe, utt.pcm(), utt.sample_rate,
            skill.config["language"], 5, ctx.cancel_event)
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


if __name__ == "__main__":
    if os.environ.get("AURA_ASR_PRELOAD", "1") != "0":
        threading.Thread(target=_load, daemon=True).start()
    run_all([skill])
