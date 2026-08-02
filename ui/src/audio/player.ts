import { base64Decode, fromPCM16 } from "./resample";

/**
 * Playback of an audio stream that arrives in pieces.
 *
 * Two things this deliberately does not do. It does not call
 * `decodeAudioData` per chunk — that produces an audible click at every
 * boundary. And it does not stop abruptly on barge-in: a hard stop is itself
 * a click loud enough to retrigger voice detection, so the assistant would
 * interrupt itself. Chunks are scheduled back to back on a shared clock, and
 * stopping ramps the gain down over 20ms.
 */

/** How far ahead of the clock to schedule. Absorbs network jitter; every
 *  millisecond of it is also added latency, so it stays small. */
const PREBUFFER_S = 0.12;
const FADE_S = 0.02;

export class Player {
  private context?: AudioContext;
  private gain?: GainNode;
  private nextStart = 0;
  private sources = new Set<AudioBufferSourceNode>();
  private onIdleTimer?: number;

  constructor(private readonly onPlayingChange?: (playing: boolean) => void) {}

  private ensure(): AudioContext {
    if (!this.context) {
      this.context = new AudioContext();
      this.gain = this.context.createGain();
      this.gain.connect(this.context.destination);
    }
    // A context created before a user gesture starts suspended.
    void this.context.resume();
    return this.context;
  }

  /** Queue one std/audio-chunk@1 payload. */
  play(pcmBase64: string, sampleRate: number): void {
    if (!pcmBase64) return;
    const context = this.ensure();
    const samples = fromPCM16(base64Decode(pcmBase64));
    if (!samples.length) return;

    // Each chunk carries its own rate — a synthesiser's output rate is not
    // the microphone's, and assuming otherwise plays speech at the wrong
    // pitch.
    const buffer = context.createBuffer(1, samples.length, sampleRate);
    buffer.getChannelData(0).set(samples);

    const source = context.createBufferSource();
    source.buffer = buffer;
    source.connect(this.gain!);

    const now = context.currentTime;
    if (this.nextStart < now + 0.005) {
      this.nextStart = now + PREBUFFER_S;
      this.gain!.gain.cancelScheduledValues(now);
      this.gain!.gain.setValueAtTime(1, now);
      this.onPlayingChange?.(true);
    }
    source.start(this.nextStart);
    this.nextStart += buffer.duration;

    this.sources.add(source);
    source.onended = () => {
      this.sources.delete(source);
      this.scheduleIdleCheck();
    };
  }

  private scheduleIdleCheck(): void {
    window.clearTimeout(this.onIdleTimer);
    this.onIdleTimer = window.setTimeout(() => {
      if (this.sources.size === 0) this.onPlayingChange?.(false);
    }, 60);
  }

  get isPlaying(): boolean {
    return this.sources.size > 0;
  }

  /** Stop now, without the click a bare stop() would make. */
  flush(): void {
    if (!this.context || !this.gain) return;
    const now = this.context.currentTime;
    this.gain.gain.cancelScheduledValues(now);
    this.gain.gain.setValueAtTime(this.gain.gain.value, now);
    this.gain.gain.linearRampToValueAtTime(0, now + FADE_S);

    for (const source of this.sources) {
      try {
        source.stop(now + FADE_S);
      } catch {
        /* already stopped */
      }
    }
    this.sources.clear();
    this.nextStart = 0;
    this.onPlayingChange?.(false);
  }

  async close(): Promise<void> {
    this.flush();
    await this.context?.close();
    this.context = undefined;
  }
}
