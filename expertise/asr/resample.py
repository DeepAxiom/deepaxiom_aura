"""
asr.resample — rational resampling with an anti-alias filter, numpy only.

Whisper wants 16 kHz. What this replaces was linear interpolation between
input samples, which is not a low-pass filter: bringing 48 kHz down to 16 kHz
that way folds everything between 8 and 24 kHz back into the speech band
(sibilants, fricatives and room noise land on top of the formants) at close to
full level.

This is the textbook polyphase resampler: conceptually upsample by `up`,
low-pass with a Kaiser-windowed sinc cut just below the lower of the two
Nyquist rates, keep every `down`-th sample. The zero-stuffed signal is never
built — each output sample is a dot product of ~`2 * _ZERO_CROSSINGS * down /
up` input samples with one phase of the filter.

Limits, stated plainly: this is a block resampler for whole buffers (each call
filters from silence, so resampling a stream chunk by chunk would click at the
seams — the skill never does that, it resamples the buffer it is about to
decode). The stopband is about -80 dB by design, not measured against scipy,
which is not a dependency here.

numpy is imported inside the functions, not at module level, for the same
reason engine.py imports faster-whisper lazily: CI runs this skill's tests
without numpy.
"""
from __future__ import annotations

import math

# Filter half-length in zero crossings of the sinc at the cutoff. More is a
# steeper transition and a longer filter.
_ZERO_CROSSINGS = 16
# Kaiser beta ~ 8.6 gives a stopband around -80 dB.
_KAISER_BETA = 8.6
# Cutoff as a fraction of the limiting Nyquist. Below 1 so the transition band
# ends at Nyquist instead of straddling it.
_ROLLOFF = 0.9
# Output samples per vectorised block: bounds the temporary index matrix.
_BLOCK = 8192

_filters: dict[tuple[int, int], tuple] = {}


def _polyphase_filter(up: int, down: int):
    """(taps[phase, i], centre) for this ratio, cached. `taps[p, i]` is
    h[p + i*up], already scaled by `up` to undo the zero-stuffing loss."""
    cached = _filters.get((up, down))
    if cached is not None:
        return cached
    import numpy as np

    widest = max(up, down)
    centre = _ZERO_CROSSINGS * widest
    length = 2 * centre + 1
    cutoff = 0.5 * _ROLLOFF / widest  # cycles per sample, at the upsampled rate
    n = np.arange(length, dtype=np.float64) - centre
    h = 2 * cutoff * np.sinc(2 * cutoff * n) * np.kaiser(length, _KAISER_BETA)
    h *= up / h.sum()  # unity gain at DC after the 1-in-`up` zero stuffing
    per_phase = math.ceil(length / up)
    padded = np.zeros(per_phase * up, dtype=np.float64)
    padded[:length] = h
    taps = np.ascontiguousarray(padded.reshape(per_phase, up).T, dtype=np.float32)
    _filters[(up, down)] = (taps, centre)
    return taps, centre


def resample(audio, src_rate: int, dst_rate: int = 16000):
    """Resample a 1-D float array from `src_rate` to `dst_rate`.

    Returns float32. The output has ceil(len * dst / src) samples and no
    filter delay: output sample n sits at input time n * src / dst.
    """
    import numpy as np

    x = np.asarray(audio, dtype=np.float32)
    if src_rate <= 0 or dst_rate <= 0:
        raise ValueError(f"sample rates must be positive, got {src_rate} -> {dst_rate}")
    if src_rate == dst_rate or x.size == 0:
        return x
    g = math.gcd(int(src_rate), int(dst_rate))
    up, down = int(dst_rate) // g, int(src_rate) // g
    taps, centre = _polyphase_filter(up, down)
    per_phase = taps.shape[1]

    n_out = -(-x.size * up // down)
    # y[n] = sum_i h[p + i*up] * x[q - i], with p, q the remainder and
    # quotient of (n*down + centre) by up. Pad so q - i never leaves the array.
    pad = per_phase
    tail = (n_out * down + centre) // up + 1
    padded = np.zeros(pad + max(tail, x.size) + 1, dtype=np.float32)
    padded[pad:pad + x.size] = x
    reach = np.arange(per_phase, dtype=np.int64)

    out = np.empty(n_out, dtype=np.float32)
    for lo in range(0, n_out, _BLOCK):
        n = np.arange(lo, min(lo + _BLOCK, n_out), dtype=np.int64)
        pos = n * down + centre
        phase, q = pos % up, pos // up
        idx = (q + pad)[:, None] - reach[None, :]
        out[lo:lo + n.size] = np.einsum("nt,nt->n", padded[idx], taps[phase])
    return out
