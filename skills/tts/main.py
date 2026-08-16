"""
tts — motor.tts.speak (model plane driver: local voice engines).

Input:  std/text@1 — one clause at a time. Put skills/sentence-chunker in
        front of a streaming LLM, or this synthesises once per token.

Output: std/audio-chunk@1 on audio_chunk_out — PCM in ~chunk_ms pieces, for a
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
assumed, and a failure degrades instead of crashing — see
backends/piper.py's header for exactly what was confirmed against the
installed piper-tts version and how.

Module layout: backends/piper.py and backends/os_voice.py hold the two
engines (each imports its own third-party SDK lazily, inside a function, so
neither needs to be installed just to load this skill); audio.py holds the
pure PCM/WAV helpers shared by both. This module keeps the degradation chain
(_synthesize) and the C1 handler.
"""
import asyncio
import base64
import logging
import os
import threading

from aura import Context, Skill, run_all

from audio import _pcm_chunks, _pcm_to_wav
from backends.os_voice import _os_voice
from backends.piper import _piper_stream

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("tts")

skill = Skill()


def _synthesize(text: str, stop: threading.Event) -> tuple[str, list[bytes], int]:
    """Walk the degradation chain. Returns (backend, pcm pieces, sample rate)."""
    errors = []
    chunk_ms = skill.config["chunk_ms"]
    if os.getenv("AURA_PIPER_MODEL"):
        try:
            pieces, rate = [], 22050
            for pcm, rate in _piper_stream(text, stop, skill.config):
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
        pcm, rate = _os_voice(text, skill.config)
        return "os-voice", _pcm_chunks(pcm, rate, chunk_ms), rate
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

    chunk_ms = skill.config["chunk_ms"]
    seq = 0
    for pcm in pieces:
        # Between chunks is where a barge-in actually takes effect: the kernel
        # would suppress these anyway, but stopping here frees the machine for
        # whatever the person said instead.
        if ctx.cancelled:
            log.info("stopped speaking after %d chunk(s)", seq)
            return
        for piece in _pcm_chunks(pcm, rate, chunk_ms):
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
