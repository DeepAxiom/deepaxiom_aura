/**
 * Microphone capture, on the audio thread.
 *
 * An AudioWorklet rather than MediaRecorder: MediaRecorder produces
 * Opus-in-WebM, which a speech recogniser cannot read without ffmpeg, and
 * whose chunk boundaries are container-framed rather than time-framed. This
 * hands back raw samples on a stable ~3ms callback instead.
 *
 * It stays deliberately dumb — accumulate a frame, measure its loudness, post
 * it. Voice-activity decisions and resampling live on the main thread where
 * they can be read, tested and changed without touching the audio thread.
 */
const FRAME_MS = 20;

class CaptureProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const frameMs = options?.processorOptions?.frameMs ?? FRAME_MS;
    this.frameSize = Math.round((sampleRate * frameMs) / 1000);
    this.buffer = new Float32Array(this.frameSize);
    this.filled = 0;
  }

  process(inputs) {
    const channel = inputs[0]?.[0];
    if (!channel) return true;

    for (let i = 0; i < channel.length; i++) {
      this.buffer[this.filled++] = channel[i];
      if (this.filled < this.frameSize) continue;

      // Loudness is measured here, on the samples themselves, so the main
      // thread never has to look at raw audio just to decide whether anyone
      // is speaking.
      let sum = 0;
      for (let j = 0; j < this.frameSize; j++) sum += this.buffer[j] * this.buffer[j];
      const rms = Math.sqrt(sum / this.frameSize);

      const frame = this.buffer.slice();
      this.port.postMessage({ frame, rms, sampleRate }, [frame.buffer]);
      this.filled = 0;
    }
    return true;
  }
}

registerProcessor("capture-processor", CaptureProcessor);
