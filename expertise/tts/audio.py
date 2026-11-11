"""Pure PCM/WAV manipulation for motor.tts.speak — stdlib only (`io`, `wave`).

Deliberately has no import of `pyttsx3` or `piper`: both backends produce
raw PCM or a WAV file and hand it here to be re-chunked or (re)packaged, but
neither engine needs to be installed to exercise this logic. That is what
lets test_audio.py run on the CI lane that has neither optional dependency —
if this module ever grows an import that needs one of them, that lane breaks
silently at collection time, not at a clearly-labelled test failure.
"""
import io
import wave


def _pcm_chunks(pcm: bytes, sample_rate: int, chunk_ms: int) -> list[bytes]:
    """Split raw 16-bit mono PCM into ~chunk_ms pieces for streaming output."""
    size = max(int(sample_rate * chunk_ms / 1000) * 2, 2)
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
