/**
 * Deciding when someone starts and stops speaking.
 *
 * This lives in the client, not in the recogniser skill, because barge-in has
 * to be decided locally: the moment the user speaks, playback must stop. A
 * round trip to ask the server first would add exactly the latency the whole
 * voice path exists to avoid.
 *
 * Energy against an adaptive noise floor — no model, no download, a few
 * operations per frame. It is not the best VAD available; it is the one that
 * costs nothing and is good enough that replacing it is a later decision
 * rather than a blocker.
 */

export interface VadOptions {
  /** Ignore bursts shorter than this — a keyboard click is not speech. */
  minSpeechMs?: number;
  /** Keep listening this long after the voice drops, so pauses between words
   *  do not end the utterance. */
  hangoverMs?: number;
  /** How much audio from *before* the trigger to keep. */
  prerollMs?: number;
  /** How far above the measured noise floor counts as voice. */
  thresholdFactor?: number;
}

export type VadEvent = "speech-start" | "speech-end" | null;

interface Frame {
  samples: Float32Array;
  rms: number;
}

export class Vad {
  private readonly minSpeechMs: number;
  private readonly hangoverMs: number;
  private readonly prerollFrames: number;
  private readonly thresholdFactor: number;

  private noiseFloor = 0.005;
  private speaking = false;
  private voicedMs = 0;
  private quietMs = 0;
  private preroll: Frame[] = [];

  constructor(private readonly frameMs: number, options: VadOptions = {}) {
    this.minSpeechMs = options.minSpeechMs ?? 120;
    this.hangoverMs = options.hangoverMs ?? 300;
    this.thresholdFactor = options.thresholdFactor ?? 3.0;
    this.prerollFrames = Math.ceil((options.prerollMs ?? 300) / frameMs);
  }

  get isSpeaking(): boolean {
    return this.speaking;
  }

  /**
   * Feed one frame. Returns a transition, or null.
   *
   * On "speech-start" the caller should also drain `takePreroll()`: without
   * those frames the recogniser never hears the first phoneme, and mishears
   * the first word — the single most common way a voice demo feels broken.
   */
  push(samples: Float32Array, rms: number): VadEvent {
    const loud = rms > Math.max(this.noiseFloor * this.thresholdFactor, 0.008);

    if (!this.speaking) {
      // Track the quiet background, but only while nobody is talking, or the
      // speaker's own voice would raise the floor until they are inaudible.
      this.noiseFloor = this.noiseFloor * 0.95 + rms * 0.05;

      this.preroll.push({ samples, rms });
      if (this.preroll.length > this.prerollFrames) this.preroll.shift();

      this.voicedMs = loud ? this.voicedMs + this.frameMs : 0;
      if (this.voicedMs >= this.minSpeechMs) {
        this.speaking = true;
        this.quietMs = 0;
        return "speech-start";
      }
      return null;
    }

    this.quietMs = loud ? 0 : this.quietMs + this.frameMs;
    if (this.quietMs >= this.hangoverMs) {
      this.speaking = false;
      this.voicedMs = 0;
      this.preroll = [];
      return "speech-end";
    }
    return null;
  }

  /** The audio captured just before the trigger fired. */
  takePreroll(): Float32Array[] {
    const frames = this.preroll.map((f) => f.samples);
    this.preroll = [];
    return frames;
  }

  reset(): void {
    this.speaking = false;
    this.voicedMs = 0;
    this.quietMs = 0;
    this.preroll = [];
  }
}
