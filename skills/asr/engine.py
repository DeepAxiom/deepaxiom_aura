"""
asr.engine — the faster-whisper wrapper, utterance buffering, VAD/endpointing
loudness check, and the sweep for abandoned utterances.

Split out of main.py so the C1 glue (the `@skill.on` handlers and the reads
of `skill.config`) stays thin, and so the pieces that do not need a loaded
model — `Utterance`, `rms`, `sweep` — can be unit tested without
`faster-whisper` installed.

Import of `faster_whisper` MUST stay lazy (inside `load_model`, not at
module level): CI's `skills` job runs this skill's tests with only `pyyaml`
installed (see `.github/workflows/ci.yml`, job `skills`), and a top-level
`from faster_whisper import WhisperModel` would turn every import of this
module — including from tests that never touch the model — into a hard
`ModuleNotFoundError`.

This module knows nothing about `skill.config` or the kernel connection on
purpose: callers (main.py) read config and pass plain values in, which keeps
this module mockable in tests without a running `Skill`.
"""
import logging
import math
import threading
import time
from dataclasses import dataclass, field

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("asr.engine")

# ── model loading ─────────────────────────────────────────────────────────

_model = None
_model_lock = threading.Lock()
model_ready = threading.Event()


def load_model(model_size: str):
    """Load once and cache. Called from a warm-up thread at start (if
    `preload_on_boot` is set) so the first utterance does not pay a
    10-30 second model load and time out."""
    global _model
    with _model_lock:
        if _model is None:
            from faster_whisper import WhisperModel

            log.info("loading whisper %s (CPU int8) ...", model_size)
            _model = WhisperModel(model_size, device="cpu", compute_type="int8")
            log.info("model ready")
        model_ready.set()
        return _model


def transcribe(pcm: bytes, sample_rate: int, language: str | None,
                beam: int, model_size: str,
                stop: threading.Event | None = None) -> str:
    """Blocking decode. Runs in a worker thread; `stop` lets a barge-in
    abandon a partial nobody is waiting for any more."""
    if stop is not None and stop.is_set():
        return ""
    model = load_model(model_size)
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


# (session, node) -> Utterance / last-activity timestamp. Module-level and
# shared across handler invocations on purpose: a streaming utterance spans
# many `audio_chunk_in` envelopes.
utterances: dict[tuple[str, str], Utterance] = {}
last_seen: dict[tuple[str, str], float] = {}

_SWEEP_IDLE_SECONDS = 120


def sweep() -> None:
    """Drop abandoned utterances. The SDK has no session-end hook, so without
    this a node that runs for weeks accumulates the tail of every conversation
    that was cut off mid-sentence."""
    cutoff = time.monotonic() - _SWEEP_IDLE_SECONDS
    for key, seen in list(last_seen.items()):
        if seen < cutoff:
            utterances.pop(key, None)
            last_seen.pop(key, None)


def rms(pcm: bytes) -> float:
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
