import { base64Encode, TARGET_RATE, toPCM16, toTargetRate } from "./resample";
import { Vad, type VadOptions } from "./vad";

const FRAME_MS = 20;
/** Frames per envelope. 20ms is right for VAD; 20ms on the wire would be 50
 *  envelopes a second and 100 SQLite writes, for 80ms of added latency that
 *  nobody can hear next to a recogniser's decode time. */
const FRAMES_PER_ENVELOPE = 5;

export interface CaptureEvents {
  /** A chunk ready to send, already base64 PCM16 at 16kHz. */
  onChunk: (pcmBase64: string, final: boolean) => void;
  /** The user started speaking — barge-in happens here. */
  onSpeechStart: () => void;
  onSpeechEnd: () => void;
  /** Loudness, for a level meter. */
  onLevel?: (rms: number) => void;
}

export class Capture {
  private context?: AudioContext;
  private stream?: MediaStream;
  private node?: AudioWorkletNode;
  private vad?: Vad;
  private pending: Float32Array[] = [];
  private muted = false;
  /** Ignore the VAD briefly after playback starts, so the assistant's own
   *  voice leaking into the microphone does not interrupt the assistant. */
  private guardUntil = 0;

  constructor(private readonly events: CaptureEvents, private readonly vadOptions: VadOptions = {}) {}

  async start(): Promise<void> {
    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: 1,
        // This is what stops the assistant hearing itself through the
        // speakers and treating it as the user interrupting.
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });

    // Ask for 16kHz directly; Chromium and Edge resample in the graph for
    // free. resample.ts handles the browsers that ignore this.
    this.context = new AudioContext({ sampleRate: TARGET_RATE });
    await this.context.audioWorklet.addModule("/worklets/capture-processor.js");

    this.vad = new Vad(FRAME_MS, this.vadOptions);
    const source = this.context.createMediaStreamSource(this.stream);
    this.node = new AudioWorkletNode(this.context, "capture-processor", {
      processorOptions: { frameMs: FRAME_MS },
    });

    this.node.port.onmessage = (event) => {
      const { frame, rms, sampleRate } = event.data as {
        frame: Float32Array;
        rms: number;
        sampleRate: number;
      };
      this.events.onLevel?.(rms);
      if (this.muted) return;
      this.handleFrame(toTargetRate(frame, sampleRate), rms);
    };

    source.connect(this.node);
    // Connecting to the destination would play the microphone back through
    // the speakers. A zero-gain node keeps the graph pulling without it.
    const silent = this.context.createGain();
    silent.gain.value = 0;
    this.node.connect(silent).connect(this.context.destination);
  }

  private handleFrame(samples: Float32Array, rms: number): void {
    const vad = this.vad!;
    const wasSpeaking = vad.isSpeaking;
    const event = vad.push(samples, rms);

    if (event === "speech-start") {
      if (performance.now() < this.guardUntil) {
        vad.reset();
        return;
      }
      // Everything captured just before the trigger — without it the
      // recogniser never hears the start of the first word.
      this.pending = vad.takePreroll();
      this.events.onSpeechStart();
    }

    if (vad.isSpeaking || event === "speech-end") {
      this.pending.push(samples);
      while (this.pending.length >= FRAMES_PER_ENVELOPE) {
        this.flush(this.pending.splice(0, FRAMES_PER_ENVELOPE), false);
      }
    }

    if (event === "speech-end") {
      if (this.pending.length) this.flush(this.pending.splice(0), true);
      else this.flush([new Float32Array(0)], true);
      this.events.onSpeechEnd();
    } else if (!vad.isSpeaking && wasSpeaking) {
      this.pending = [];
    }
  }

  private flush(frames: Float32Array[], final: boolean): void {
    const total = frames.reduce((n, f) => n + f.length, 0);
    const merged = new Float32Array(total);
    let offset = 0;
    for (const f of frames) {
      merged.set(f, offset);
      offset += f.length;
    }
    this.events.onChunk(base64Encode(toPCM16(merged)), final);
  }

  /** Suppress voice detection for a moment, e.g. just after playback starts. */
  guardFor(ms: number): void {
    this.guardUntil = performance.now() + ms;
  }

  /** Half-duplex fallback: stop listening entirely while the assistant talks.
   *  Guaranteed to work when echo cancellation is not enough. */
  setMuted(muted: boolean): void {
    this.muted = muted;
    if (muted) {
      this.vad?.reset();
      this.pending = [];
    }
  }

  async stop(): Promise<void> {
    this.node?.port.close();
    this.node?.disconnect();
    this.stream?.getTracks().forEach((t) => t.stop());
    await this.context?.close();
    this.context = undefined;
    this.node = undefined;
    this.stream = undefined;
  }
}
