"""
asr.engine — the faster-whisper wrapper, utterance buffering, VAD/endpointing
loudness check, and the sweep for abandoned utterances.

Split out of main.py so the C1 glue (the `@skill.on` handlers and the reads
of `skill.config`) stays thin, and so the pieces that do not need a loaded
model — `Utterance`, `rms`, `sweep` — can be unit tested without
`faster-whisper` installed.

Import of `faster_whisper` MUST stay lazy (inside `_make_model`, not at
module level): CI's `skills` job runs this skill's tests with only `pyyaml`
installed (see `.github/workflows/ci.yml`, job `skills`), and a top-level
`from faster_whisper import WhisperModel` would turn every import of this
module — including from tests that never touch the model — into a hard
`ModuleNotFoundError`. The same goes for numpy (`_audio_16k`, resample.py)
and ctranslate2 (`_cuda_available`).

This module knows nothing about `skill.config` or the kernel connection on
purpose: callers (main.py) read config and pass plain values in, which keeps
this module mockable in tests without a running `Skill`.

Two streaming policies live side by side (main.py picks by `streaming_policy`):

  redecode         `transcribe()` on the last 10 s for each partial and on the
                   whole utterance for the final. Every hypothesis is decoded
                   from scratch. The default, and the one with production
                   mileage.
  local_agreement  `stream_partial()` / `stream_final()`: decode only the
                   uncommitted tail, commit the words two consecutive
                   hypotheses agree on, cut the buffer behind them. The policy
                   itself is `streaming.StreamingDecoder`; this module only
                   feeds it audio and a model.

How far `local_agreement` has been verified is written down further below,
above `drop_silent_tail`. Short version: unit tests against a scripted
decoder, never against a model in CI (CI installs none), plus one manual run
on two synthetic clips that found it stable but neither cheaper per partial
nor as accurate as redecode — which is why it is opt-in.

One model, many sessions: every decode in the skill goes through `gate`
(scheduler.DecodeGate) — one at a time, finals before partials.
"""
import logging
import math
import threading
import time
from dataclasses import dataclass, field

from scheduler import FINAL, PARTIAL, DecodeGate, DecodeSkipped  # noqa: F401  (re-exported)
from streaming import DEFAULT_CONTEXT_S, Hypothesis, StreamingDecoder, Word

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("asr.engine")

# The one gate in front of the one model. Module-level for the same reason
# `_model` is: every handler invocation of every session shares it.
gate = DecodeGate()

# ── model loading ─────────────────────────────────────────────────────────

_model = None
# What `_model` was loaded FOR: (model_size, device as asked, compute_type as
# asked). Not what it resolved to — "auto" stays "auto" here, so asking again
# with the same config is a cache hit whatever auto picked.
_model_key = None
# What it actually runs on, for logs and tests.
loaded_on = None
_model_lock = threading.Lock()
model_ready = threading.Event()

# Device preference. Set by main.py from skill.config through `configure()`
# rather than passed to `load_model`/`transcribe`: those two signatures are
# what callers and tests already use, and the device is a property of the
# process, not of one decode.
_prefs = {"device": "cpu", "compute_type": "int8"}


def configure(device: str | None = None, compute_type: str | None = None) -> None:
    if device:
        _prefs["device"] = device
    if compute_type:
        _prefs["compute_type"] = compute_type


def _cuda_available() -> bool:
    """Whether ctranslate2 (faster-whisper's backend) sees a CUDA device.
    Lazy import for the same reason as faster_whisper; any failure is "no"."""
    try:
        import ctranslate2

        return ctranslate2.get_cuda_device_count() > 0
    except Exception as exc:  # noqa: BLE001
        log.info("no CUDA device visible to ctranslate2 (%s)", exc)
        return False


def resolve_device(device: str, compute_type: str, cuda: bool | None = None) -> tuple[str, str]:
    """Turn the configured pair into the pair to load with.

    `auto` device is cuda when ctranslate2 reports one, else cpu. `auto`
    compute type follows the device: float16 on cuda, int8 on cpu. Explicit
    values are passed through untouched — an impossible combination is
    ctranslate2's to reject, loudly, rather than ours to second-guess.
    """
    if device == "auto":
        if cuda is None:
            cuda = _cuda_available()
        device = "cuda" if cuda else "cpu"
    if compute_type == "auto":
        compute_type = "float16" if device == "cuda" else "int8"
    return device, compute_type


def _make_model(model_size: str, device: str, compute_type: str):
    from faster_whisper import WhisperModel

    return WhisperModel(model_size, device=device, compute_type=compute_type)


def load_model(model_size: str):
    """Load once and cache. Called from a warm-up thread at start (if
    `preload_on_boot` is set) so the first utterance does not pay a
    10-30 second model load and time out.

    The cache is keyed by (model_size, device, compute_type) as configured:
    asking for something other than what is loaded loads again. That matters
    because the boot preload runs before the kernel has sent the effective
    config, i.e. with the manifest defaults; without the key, a non-default
    device (or model size) would be silently ignored for the life of the
    process once the preload had won.

    With device `auto`, a GPU load that fails falls back to cpu/int8 and says
    why. Limit: that covers failures at LOAD time. A CUDA install that loads
    and then fails on its first inference (a missing cuDNN/cuBLAS library
    typically surfaces there) is not caught — set `device: cpu`.
    """
    global _model, _model_key, loaded_on
    with _model_lock:
        want = (model_size, _prefs["device"], _prefs["compute_type"])
        if _model is None or _model_key != want:
            device, compute_type = resolve_device(want[1], want[2])
            log.info("loading whisper %s (%s %s) ...", model_size, device, compute_type)
            try:
                model = _make_model(model_size, device, compute_type)
            except Exception as exc:  # noqa: BLE001
                if want[1] != "auto" or device == "cpu":
                    raise
                log.warning(
                    "loading whisper %s on %s/%s failed (%s: %s); "
                    "falling back to cpu/int8", model_size, device,
                    compute_type, type(exc).__name__, exc)
                device, compute_type = "cpu", "int8"
                model = _make_model(model_size, device, compute_type)
            _model, _model_key, loaded_on = model, want, (device, compute_type)
            log.info("model ready")
        model_ready.set()
        return _model


def _audio_16k(pcm: bytes, sample_rate: int):
    """PCM16LE bytes -> float32 in [-1, 1) at the 16 kHz whisper wants."""
    import numpy as np

    audio = np.frombuffer(pcm[: len(pcm) // 2 * 2], dtype=np.int16).astype(np.float32) / 32768.0
    if sample_rate != 16000:
        from resample import resample

        audio = resample(audio, sample_rate, 16000)
    return audio


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
    segments, _ = model.transcribe(
        _audio_16k(pcm, sample_rate), beam_size=beam, language=language or None,
        condition_on_previous_text=False,
    )
    return " ".join(seg.text.strip() for seg in segments).strip()


def transcribe_words(pcm: bytes, sample_rate: int, language: str | None,
                     beam: int, model_size: str,
                     stop: threading.Event | None = None,
                     prompt: str = "", info: dict | None = None) -> list[Word]:
    """Like `transcribe`, but returns timed words and accepts a prompt — what
    the local_agreement policy needs to find a commit point and to cut the
    buffer there.

    Word timestamps are not free: faster-whisper runs an alignment pass over
    the cross-attention weights to get them, on top of the decode.

    Decodes at temperature 0 only, unlike `transcribe`. faster-whisper's
    default retries a low-confidence segment at five higher temperatures;
    measured on a 2.4 s tail whose trailing silence the model filled with
    "No, no, no.", that ladder took 4-7.5 s to return the SAME text the first
    pass gave in 1.5 s. A streaming decode has to have a bounded cost, and
    what the ladder exists to escape (loops, invented tails) is caught after
    the fact here: streaming.strip_repeats and drop_silent_tail.

    `info`, if given, receives the detected language. `stop` is checked
    between segments (faster-whisper decodes lazily, segment by segment, so
    that is a real exit); the words so far are returned and the caller decides
    what a truncated hypothesis is worth.
    """
    if stop is not None and stop.is_set():
        return []
    model = load_model(model_size)
    if not pcm:
        return []
    segments, meta = model.transcribe(
        _audio_16k(pcm, sample_rate), beam_size=beam, language=language or None,
        condition_on_previous_text=False, word_timestamps=True,
        initial_prompt=prompt or None, temperature=0.0,
    )
    if info is not None:
        info["language"] = getattr(meta, "language", None)
    words: list[Word] = []
    for seg in segments:
        for w in getattr(seg, "words", None) or []:
            raw = w.word
            words.append(Word(raw.strip(), float(w.start), float(w.end),
                              space=raw[:1].isspace()))
        if stop is not None and stop.is_set():
            break
    return words


# ── per-utterance state ───────────────────────────────────────────────────

@dataclass
class Utterance:
    """One in-progress utterance for one (session, node).

    Chunks are kept by `seq` rather than appended, so a gap left by a dropped
    frame on a realtime channel is visible instead of silently splicing two
    unrelated moments of speech together.

    `chunks` stays the record of what arrived; the audio itself is ALSO kept
    as one contiguous buffer, extended in place when a chunk arrives in order
    (the normal case) and rebuilt only after one arrives out of order. Joining
    every chunk on every read made an utterance quadratic: the handler asks
    for the length several times per chunk. `add()` is the only supported way
    to put audio in — writing to `chunks` directly is noticed only when the
    number of chunks changes, and costs a rebuild.

    Guarded by a lock because a local_agreement decode reads the newest audio
    from its worker thread while the event loop keeps appending.
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

    # local_agreement state. Untouched (None/False) under `redecode`.
    stream: StreamingDecoder | None = field(default=None, repr=False)
    finalized: bool = False
    detected_language: str | None = None
    last_partial: tuple | None = field(default=None, repr=False)

    _buf: bytearray = field(default_factory=bytearray, repr=False, compare=False)
    _buffered: int = field(default=0, repr=False, compare=False)  # chunks in _buf
    _stale: bool = field(default=False, repr=False, compare=False)
    _top_seq: int = field(default=-1, repr=False, compare=False)
    _inserts: list = field(default_factory=list, repr=False, compare=False)
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False, compare=False)

    def add(self, pcm: bytes, seq: int | None) -> None:
        with self._lock:
            if seq is None:
                seq = self.next_seq
            self.next_seq = max(self.next_seq, seq + 1)
            old = self.chunks.get(seq)
            self.chunks[seq] = pcm
            if old is None and seq > self._top_seq:
                if not self._stale and self._buffered == len(self.chunks) - 1:
                    self._buf += pcm
                    self._buffered += 1
                else:
                    self._stale = True
            else:
                # Out of order, or a resend of a seq already held: the audio
                # after this point has moved. Remember where and by how much,
                # for a streaming decoder whose window is anchored in time.
                at = sum(len(c) for k, c in self.chunks.items() if k < seq)
                self._inserts.append((at, len(pcm) - len(old or b"")))
                self._stale = True
            self._top_seq = max(self._top_seq, seq)

    def _sync(self) -> bytearray:
        """The contiguous buffer, rebuilt if it no longer matches `chunks`.
        Call with the lock held."""
        if self._stale or self._buffered != len(self.chunks):
            self._buf = bytearray(b"".join(self.chunks[k] for k in sorted(self.chunks)))
            self._buffered = len(self.chunks)
            self._stale = False
        return self._buf

    @property
    def nbytes(self) -> int:
        """Bytes of audio held. O(1) while chunks arrive in order."""
        with self._lock:
            return len(self._sync())

    def pcm(self, max_seconds: float | None = None) -> bytes:
        with self._lock:
            data = self._sync()
            if max_seconds is not None:
                keep = int(max_seconds * self.sample_rate) * 2
                if len(data) > keep:
                    return bytes(data[-keep:])
            return bytes(data)

    def pcm_from(self, offset: int) -> bytes:
        """The audio from byte `offset` to the end — the uncommitted window."""
        with self._lock:
            return bytes(self._sync()[max(offset, 0):])

    def take_inserts(self) -> list:
        """(byte position, byte delta) of every out-of-order arrival since the
        last call."""
        with self._lock:
            inserts, self._inserts = self._inserts, []
            return inserts

    def gaps(self) -> int:
        return self.next_seq - len(self.chunks)

    def duration(self) -> float:
        return self.nbytes / 2 / max(self.sample_rate, 1)


# ── local_agreement glue ──────────────────────────────────────────────────
#
# Verification status, so nobody reads more into this than there is:
#   * The policy (streaming.py), the gate (scheduler.py) and the functions
#     below are covered by unit tests against a SCRIPTED decoder. That is all
#     CI runs.
#   * Run by hand, once, against a real model: faster-whisper 1.2.1, `base`,
#     cpu/int8, on a 4-core laptop that was busy with other work, over two
#     ~14 s clips of SYNTHETIC speech (Windows SAPI voices, one English, one
#     Spanish), fed in 100 ms chunks with a partial every 900 ms. Not a
#     benchmark: no microphone, no noise, one voice per language, timings
#     with a spread of 2x between passes. What it showed:
#       - Stability held. In every run, text reported as stable was a prefix
#         of every later partial and of the final.
#       - Partials were NOT cheaper: about 0.9-1.0 s median under both
#         policies. Whisper encodes a fixed 30 s window whatever it is given,
#         so a shorter buffer saves only decoder steps, and word timestamps
#         cost about what that saves.
#       - The final was not reliably faster: English 1.3 s against 1.9 s for
#         redecode (median of 3), Spanish 1.6 s against 1.2 s — slower,
#         because the model filled the trailing silence with an invented
#         segment (dropped afterwards by drop_silent_tail, but paid for).
#       - Final text: English equal to redecode's but for punctuation ("730"
#         for "7.30"). Spanish differed in three places out of 46 words, all
#         three further from what was said ("misita" for "mi cita") — words
#         committed from a beam-1 partial and, by design, never revisited.
#     Three defects surfaced in that run and were fixed with a test each: a
#     window cut flush behind the last committed word decoded as nothing or
#     as nonsense (hence the committed context), invented words over
#     trailing silence, and a temperature-fallback ladder that made one
#     final take 4-7 s. Expect real audio to surface more.
#   * Never run: a GPU; any model but `base`; the streaming path at a sample
#     rate other than 16 kHz; an utterance long enough to hit the cap;
#     several sessions against a real model at once; the kernel end to end.
# What the policy buys today is a prefix that stops flickering, not CPU time.
# Treat it as experimental until it has seen real microphones.

# How much uncommitted audio a partial may carry before the policy gives up on
# agreement for the older half. Same figure as the redecode partial window.
MAX_UNCOMMITTED_SECONDS = 10.0
# Committed audio kept in front of the uncommitted tail as lead-in for the
# model (streaming.py explains the measurement behind it).
COMMITTED_CONTEXT_SECONDS = DEFAULT_CONTEXT_S


def _stream_for(utt: Utterance) -> StreamingDecoder:
    if utt.stream is None:
        utt.stream = StreamingDecoder(max_uncommitted_s=MAX_UNCOMMITTED_SECONDS,
                                      context_s=COMMITTED_CONTEXT_SECONDS)
    dec = utt.stream
    rate = max(utt.sample_rate, 1)
    for at, delta in utt.take_inserts():
        dec.notify_insert(at / 2 / rate, delta / 2 / rate)
    return dec


def _window(utt: Utterance, dec: StreamingDecoder) -> tuple[bytes, float]:
    rate = max(utt.sample_rate, 1)
    pcm = utt.pcm_from(int(dec.window_start * rate) * 2)
    return pcm, len(pcm) / 2 / rate


def drop_silent_tail(words: list[Word], pcm: bytes, sample_rate: int,
                     threshold: float) -> list[Word]:
    """Drop the words at the END of a hypothesis that are timed over silence.

    Measured on a real model: the tail "...a la lista de compras." followed
    by 0.8 s of quiet came back as "...a la lista de compras. No, no, no." —
    the three extra words stamped onto the quiet stretch, at RMS 183, 0 and 0
    against 2600-4700 for the real ones. A window that ends in silence is the
    normal case for a final (the endpoint detector WAITS for that silence),
    and a short window gives the model little else to go on.

    "Silence" is the skill's own definition, the `silence_rms` threshold the
    endpoint detector uses, so the two cannot disagree about whether someone
    was speaking. Only a trailing run is removed: a quiet word in the middle
    is more likely a soft syllable or a timestamp a little off than an
    invention. The cost is a final word spoken below the threshold — which
    the endpoint detector was already treating as not speech.
    """
    if threshold <= 0:
        return words
    rate = max(sample_rate, 1)
    keep = len(words)
    while keep:
        w = words[keep - 1]
        a, b = int(w.start * rate) * 2, int(w.end * rate) * 2
        if rms(pcm[max(a, 0):max(b, 0)]) >= threshold:
            break
        keep -= 1
    return words[:keep]


def _decoder_for(utt: Utterance, dec: StreamingDecoder, language: str | None,
                 beam: int, model_size: str, stop: threading.Event | None,
                 abandon_on_stop: bool, silence_rms: float = 0.0):
    def decode(pcm: bytes, prompt: str) -> list[Word]:
        # Language autodetection needs audio. While the window still starts at
        # the beginning of the utterance, let whisper detect on all of it, as
        # redecode does, and remember the answer; once the window has been
        # cut, a two-second tail is not enough to detect from, and a flip to
        # another language mid-sentence would be committed like anything else.
        lang = language or (utt.detected_language if dec.window_start > 0 else None)
        info: dict = {}
        words = transcribe_words(pcm, utt.sample_rate, lang, beam, model_size,
                                 stop, prompt, info)
        if abandon_on_stop and stop is not None and stop.is_set():
            # A truncated hypothesis must not be compared with the next one.
            raise DecodeSkipped("cancelled mid-decode")
        if not language and dec.window_start <= 0 and info.get("language"):
            utt.detected_language = info["language"]
        return drop_silent_tail(words, pcm, utt.sample_rate, silence_rms)

    return decode


def stream_partial(utt: Utterance, language: str | None, beam: int,
                   model_size: str, stop: threading.Event | None = None,
                   silence_rms: float = 0.0) -> Hypothesis:
    """One local_agreement round on the audio the utterance holds NOW (read
    here, on the worker, after any wait at the gate — not when the partial
    was requested). Call it through `gate`; the decoder is not thread-safe."""
    dec = _stream_for(utt)
    pcm, seconds = _window(utt, dec)
    return dec.update(pcm, seconds, _decoder_for(
        utt, dec, language, beam, model_size, stop, True, silence_rms))


def stream_final(utt: Utterance, language: str | None, beam: int,
                 model_size: str, stop: threading.Event | None = None,
                 full: bool = False, silence_rms: float = 0.0) -> str:
    """The settled transcript under local_agreement: committed words plus one
    decode of the uncommitted tail — the point of the policy, since the tail
    is short and the final beam is wide.

    Falls back to a full re-decode of the utterance, exactly as the redecode
    policy does it, when asked to (`full`), when there is no stream state to
    build on, or when audio arrived late inside the committed region — those
    samples were never transcribed and the only honest fix is to start over.
    """
    dec = _stream_for(utt) if utt.stream is not None else None
    if full or dec is None or dec.late_audio_s > 0:
        if dec is not None and dec.late_audio_s > 0:
            log.warning("utterance %s: %.2fs of audio arrived after its words "
                        "were committed; re-decoding the whole utterance",
                        utt.id, dec.late_audio_s)
        return transcribe(utt.pcm(), utt.sample_rate, language, beam, model_size, stop)
    pcm, seconds = _window(utt, dec)
    return dec.finish(pcm, seconds, _decoder_for(
        utt, dec, language, beam, model_size, stop, False, silence_rms))


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
