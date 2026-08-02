"""piper backend — motor.tts.speak's primary voice engine, the one that
streams. See ../main.py's header for why streaming matters: sound starts
before the whole clause has finished synthesising.

Piper's Python API has moved across releases. The streaming entry point that
used to be exposed as `synthesize_stream_raw` or `synthesize_raw` on
`PiperVoice` is, as of piper-tts 1.6.0, just `synthesize` (confirmed by
installing that exact version in a scratch env — `pip install piper-tts`
resolved 1.6.0 — and reading the installed `site-packages/piper/voice.py`,
which defines only `synthesize`, `synthesize_wav` and
`phoneme_ids_to_audio`). Rather than pin to whichever name is current, all
three are still tried in order and the first that yields raw audio wins —
an install that renames the method again degrades to the OS voice instead of
taking the skill down.

Synthesis params were confirmed the same way, by reading the installed
`piper.config.SynthesisConfig` dataclass (site-packages/piper/config.py) in
that 1.6.0 install:

    speaker_id: Optional[int]      index for multi-speaker voices
    length_scale: Optional[float]  phoneme length (<1 faster, >1 slower)
    noise_scale: Optional[float]   generator noise
    noise_w_scale: Optional[float] phoneme-width noise (the on-disk JSON key
                                    is "noise_w"; the Python attribute is
                                    "noise_w_scale" — confirmed by reading
                                    PiperConfig.from_dict in the same file)

All four default to `None` in the library, meaning "use the value baked
into the voice model" — so `skill.yaml` declares -1 as the sentinel for
"leave it alone" (0.0 is a real value for noise_scale, so it cannot double
as the sentinel) rather than forcing every model to one fixed rate.
`SynthesisConfig` is only ever handed to whichever streaming method accepts
it *in this exact build* (see `_call`): an older or newer piper whose method
does not take a `syn_config` kwarg falls back to a plain `fn(text)` call and
logs once, instead of failing synthesis outright. `speaker_id` on a
single-speaker model is already a no-op downstream
(`phoneme_ids_to_audio` clears it when `config.num_speakers <= 1`), so this
module does not duplicate that check.
"""
import dataclasses
import logging
import os
import threading

log = logging.getLogger("tts.backends.piper")

# skill.yaml's default for the four synthesis params below: "not set, use
# whatever the voice model itself carries". -1 is never a valid scale or
# speaker index, unlike 0.0 (a legitimate noise_scale), so it is safe to use
# as the sentinel rather than needing a separate has-the-user-touched-this
# flag per field.
_UNSET = -1


def _synthesis_config(config: dict):
    """Build a piper SynthesisConfig from skill config. Returns None if this
    piper build has no such class at all (older releases configured
    synthesis a different way, which this module does not attempt to
    reach — see the module docstring)."""
    try:
        from piper.config import SynthesisConfig  # type: ignore[import-not-found]
    except ImportError:
        return None

    field_names = {f.name for f in dataclasses.fields(SynthesisConfig)}
    kwargs = {}
    for key in ("speaker_id", "length_scale", "noise_scale", "noise_w_scale"):
        value = config.get(key, _UNSET)
        if value == _UNSET:
            continue
        if key not in field_names:
            log.warning(
                "this piper build's SynthesisConfig has no %s field; ignoring it", key)
            continue
        kwargs[key] = value
    return SynthesisConfig(**kwargs)


def _call(fn, text: str, syn_config):
    """Call a candidate streaming method, passing syn_config only if this
    build's version of that method accepts it."""
    if syn_config is None:
        return fn(text)
    try:
        return fn(text, syn_config=syn_config)
    except TypeError:
        log.warning(
            "%s does not take a syn_config kwarg on this piper build; "
            "synthesis params from config are ignored", getattr(fn, "__name__", fn))
        return fn(text)


def _piper_stream(text: str, stop: threading.Event, config: dict):
    """Yield (pcm, sample_rate) as piper produces it.

    The streaming entry point has been renamed across piper releases, so it is
    looked up rather than called by name — a rename should degrade to the OS
    voice, not take the skill down.
    """
    from piper import PiperVoice  # type: ignore[import-not-found]

    voice = PiperVoice.load(os.environ["AURA_PIPER_MODEL"])
    rate = getattr(getattr(voice, "config", None), "sample_rate", 22050)
    syn_config = _synthesis_config(config)

    for name in ("synthesize_stream_raw", "synthesize_raw", "synthesize"):
        fn = getattr(voice, name, None)
        if fn is None:
            continue
        produced = False
        for piece in _call(fn, text, syn_config):
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
