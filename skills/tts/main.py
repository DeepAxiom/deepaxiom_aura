"""
tts — motor.tts.speak (model plane driver: local voice engines).

Input:  std/text@1 — one clause at a time. Put skills/sentence-chunker in
        front of a streaming LLM, or this synthesises once per token.

Output: std/audio-chunk@1 on audio_chunk_out — PCM in ~200ms pieces, for a
        client that plays as it arrives.
        std/document@1 on audio_out — the whole clause as a WAV, for anything
        that wants a file. Both are emitted; the router drops whichever has
        no edge.

The chunked output is produced whatever the backend is, and that is
deliberate: it means the client's playback path does not change depending on
which engine happens to be installed. What does change is *when* the first
chunk appears.

  piper      chunks are emitted as they are generated — sound starts before
             the clause has finished synthesising.
  OS voice   the engine only writes whole files, so the clause is synthesised
             and then chunked. The contract holds, the latency does not.

Piper is effectively required for voice that feels live. Its API has moved
between releases, so the streaming call is feature-detected rather than
assumed, and a failure degrades instead of crashing.
"""
import asyncio
import base64
import io
import logging
import os
import tempfile
import threading
import wave
from pathlib import Path

from aura import Context, Skill, run_all

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("tts")

skill = Skill()

CHUNK_MS = 200


def _pcm_chunks(pcm: bytes, sample_rate: int) -> list[bytes]:
    size = max(int(sample_rate * CHUNK_MS / 1000) * 2, 2)
    return [pcm[i:i + size] for i in range(0, len(pcm), size)]


def _wav_to_pcm(wav_bytes: bytes) -> tuple[bytes, int]:
    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        return w.readframes(w.getnframes()), w.getframerate()


def _pcm_to_wav(pcm: bytes, sample_rate: int) -> bytes:
    out = io.BytesIO()
    with wave.open(out, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(pcm)
    return out.getvalue()


# ── backends ──────────────────────────────────────────────────────────────

def _piper_stream(text: str, stop: threading.Event):
    """Yield (pcm, sample_rate) as piper produces it.

    The streaming entry point has been renamed across piper releases, so it is
    looked up rather than called by name — a rename should degrade to the OS
    voice, not take the skill down.
    """
    from piper import PiperVoice  # type: ignore[import-not-found]

    voice = PiperVoice.load(os.environ["AURA_PIPER_MODEL"])
    rate = getattr(getattr(voice, "config", None), "sample_rate", 22050)

    for name in ("synthesize_stream_raw", "synthesize_raw", "synthesize"):
        fn = getattr(voice, name, None)
        if fn is None:
            continue
        produced = False
        for piece in fn(text):
            if stop.is_set():
                return
            # Depending on the version a piece is raw bytes or an object
            # carrying them.
            pcm = piece if isinstance(piece, (bytes, bytearray)) else getattr(
                piece, "audio_int16_bytes", None)
            if pcm is None:
                break  # not a raw-audio generator; try the next name
            produced = True
            yield bytes(pcm), rate
        if produced:
            return
    raise RuntimeError("this piper build exposes no raw streaming synthesis")


def _os_voice(text: str) -> tuple[bytes, int]:
    """Whole-file synthesis. Cannot be interrupted once started."""
    import pyttsx3

    engine = pyttsx3.init()
    wanted = (skill.config["voice"] or "").lower()
    if wanted:
        for voice in engine.getProperty("voices"):
            if wanted in voice.name.lower() or wanted in voice.id.lower():
                engine.setProperty("voice", voice.id)
                break
    engine.setProperty("rate", skill.config["rate"])
    with tempfile.TemporaryDirectory() as tmp:
        out = Path(tmp) / "speech.wav"
        engine.save_to_file(text, str(out))
        engine.runAndWait()
        try:
            engine.stop()
        except Exception:  # noqa: BLE001
            pass
        return _wav_to_pcm(out.read_bytes())


def _synthesize(text: str, stop: threading.Event) -> tuple[str, list[bytes], int]:
    """Walk the degradation chain. Returns (backend, pcm pieces, sample rate)."""
    errors = []
    if os.getenv("AURA_PIPER_MODEL"):
        try:
            pieces, rate = [], 22050
            for pcm, rate in _piper_stream(text, stop):
                pieces.append(pcm)
            if pieces:
                return "piper", pieces, rate
            errors.append("piper: produced no audio")
        except Exception as exc:  # noqa: BLE001
            errors.append(f"piper: {exc}")
            log.warning("piper failed (%s); degrading to the OS voice", exc)
    if stop.is_set():
        return "cancelled", [], 16000
    try:
        pcm, rate = _os_voice(text)
        return "os-voice", _pcm_chunks(pcm, rate), rate
    except Exception as exc:  # noqa: BLE001
        errors.append(f"os-voice: {exc}")
    raise RuntimeError("all TTS backends failed: " + "; ".join(errors))


# ── the skill ─────────────────────────────────────────────────────────────

@skill.on("text_in")
async def handle(ctx: Context) -> None:
    payload = ctx.payload or {}
    text = (payload.get("text") or "").strip()
    if not text:
        # A streaming producer ends with an empty final; there is nothing to
        # say, and saying nothing loudly is not an error.
        return

    stop = ctx.cancel_event
    try:
        backend, pieces, rate = await asyncio.to_thread(_synthesize, text, stop)
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"synthesis failed: {exc}")
        return

    if backend == "cancelled" or ctx.cancelled:
        log.info("dropped a clause the listener no longer wants")
        return

    seq = 0
    for pcm in pieces:
        # Between chunks is where a barge-in actually takes effect: the kernel
        # would suppress these anyway, but stopping here frees the machine for
        # whatever the person said instead.
        if ctx.cancelled:
            log.info("stopped speaking after %d chunk(s)", seq)
            return
        for piece in _pcm_chunks(pcm, rate):
            await ctx.emit("audio_chunk_out", {
                "pcm_b64": base64.b64encode(piece).decode(),
                "sample_rate": rate, "seq": seq, "final": False})
            seq += 1

    await ctx.emit("audio_chunk_out", {
        "pcm_b64": "", "sample_rate": rate, "seq": seq, "final": True})

    whole = b"".join(pieces)
    await ctx.emit("audio_out", {
        "mime": "audio/wav",
        "bytes_b64": base64.b64encode(_pcm_to_wav(whole, rate)).decode()})
    await ctx.status("status_out", "working",
                     f"spoke {len(whole)/2/rate:.1f}s with {backend}")


if __name__ == "__main__":
    run_all([skill])
