/**
 * Getting audio to 16kHz, which is what speech recognisers want.
 *
 * The browser is asked for a 16kHz AudioContext first, and Chromium and Edge
 * honour it. When one does not, the samples have to be converted here — and
 * the filter is not optional. Dropping every third sample of 48kHz audio folds
 * everything above 8kHz back down into the speech band as aliasing noise: the
 * result still sounds like speech to a person, and measurably raises a
 * recogniser's error rate. That failure is invisible unless you know to look
 * for it, which is why it is spelled out here.
 */

export const TARGET_RATE = 16000;

/** A modest windowed-sinc low-pass. Order 32 is plenty for this job. */
function lowPassKernel(cutoffRatio: number, taps = 32): Float32Array {
  const kernel = new Float32Array(taps);
  const mid = (taps - 1) / 2;
  let sum = 0;
  for (let i = 0; i < taps; i++) {
    const x = i - mid;
    const sinc = x === 0 ? 2 * cutoffRatio : Math.sin(2 * Math.PI * cutoffRatio * x) / (Math.PI * x);
    // Hamming window, to keep the stop-band from ringing.
    const window = 0.54 - 0.46 * Math.cos((2 * Math.PI * i) / (taps - 1));
    kernel[i] = sinc * window;
    sum += kernel[i];
  }
  for (let i = 0; i < taps; i++) kernel[i] /= sum;
  return kernel;
}

/**
 * Convert float samples to 16kHz. Returns the input untouched when it is
 * already at the target rate, which is the common case.
 */
export function toTargetRate(samples: Float32Array, sourceRate: number): Float32Array {
  if (sourceRate === TARGET_RATE) return samples;

  const ratio = TARGET_RATE / sourceRate;
  if (ratio < 1) {
    const kernel = lowPassKernel(ratio / 2);
    const filtered = new Float32Array(samples.length);
    for (let i = 0; i < samples.length; i++) {
      let acc = 0;
      for (let k = 0; k < kernel.length; k++) {
        const j = i - k;
        if (j >= 0) acc += samples[j]! * kernel[k]!;
      }
      filtered[i] = acc;
    }
    samples = filtered;
  }

  const outLength = Math.max(Math.round(samples.length * ratio), 1);
  const out = new Float32Array(outLength);
  for (let i = 0; i < outLength; i++) {
    const pos = i / ratio;
    const lo = Math.floor(pos);
    const hi = Math.min(lo + 1, samples.length - 1);
    const frac = pos - lo;
    out[i] = samples[lo]! * (1 - frac) + samples[hi]! * frac;
  }
  return out;
}

/** Float samples in [-1, 1] to the PCM16 little-endian the wire carries. */
export function toPCM16(samples: Float32Array): Uint8Array {
  const out = new Uint8Array(samples.length * 2);
  const view = new DataView(out.buffer);
  for (let i = 0; i < samples.length; i++) {
    const clamped = Math.max(-1, Math.min(1, samples[i]!));
    view.setInt16(i * 2, Math.round(clamped * 32767), true);
  }
  return out;
}

/** PCM16 little-endian back to float, for playback. */
export function fromPCM16(bytes: Uint8Array): Float32Array {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const out = new Float32Array(bytes.byteLength / 2);
  for (let i = 0; i < out.length; i++) out[i] = view.getInt16(i * 2, true) / 32768;
  return out;
}

export function base64Encode(bytes: Uint8Array): string {
  let binary = "";
  const CHUNK = 0x8000; // avoid blowing the argument limit on long buffers
  for (let i = 0; i < bytes.length; i += CHUNK) {
    binary += String.fromCharCode(...bytes.subarray(i, i + CHUNK));
  }
  return btoa(binary);
}

export function base64Decode(text: string): Uint8Array {
  const binary = atob(text);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}
