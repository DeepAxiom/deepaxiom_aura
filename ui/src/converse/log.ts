/**
 * A conversation with a graph, as a fold over envelopes.
 *
 * Typing and talking are the same conversation over two transports, so the
 * transcript they produce is one model — React-free and DOM-free, so the part
 * with the actual rules can be asserted on directly.
 *
 * **The rule worth knowing before reading anything else:** the two streams that
 * arrive during a spoken exchange grow in opposite ways.
 *
 *   - `transcript_in` (`std/transcript@1`) **replaces**. Speech recognition
 *     revises: "recommend" can become "recommended" three words later, and the
 *     new hypothesis supersedes the old one rather than continuing it. That is
 *     the entire reason `std/transcript@1` exists apart from `std/text@1`.
 *   - `text_in` (`std/text@1`) **appends**. A language model streams tokens,
 *     and each one continues the last.
 *
 * Get that backwards and the failure is quiet and ugly: a partial transcript
 * concatenated with its own revisions ("recomm recommend recommended"), or a
 * model's tokens each replacing the one before so only the final word survives.
 * Both look like a rendering glitch and are actually a schema misread, which is
 * why the fold keys off the port and not off some notion of "is this streaming".
 */

import type { Envelope } from "../api/types";

export type Line =
  /** Something the person said or typed. `partial` marks a live ASR hypothesis. */
  | { kind: "user"; key: string; text: string; partial?: boolean }
  /** The graph's reply. `open` means more tokens are still expected. */
  | { kind: "aura"; key: string; text: string; open: boolean }
  | { kind: "status"; key: string; text: string }
  | { kind: "error"; key: string; text: string }
  /** A non-text payload. Rendered as JSON because guessing at it would lie. */
  | { kind: "result"; key: string; json: string }
  | { kind: "gate"; key: string; question: string; requestId: string };

/**
 * The few strings the fold needs, passed in rather than imported.
 *
 * Keeps i18n out of a pure module: the caller translates, the fold stays a
 * function of its arguments and a test can pass stubs.
 */
export interface Vocab {
  sessionReady: (session: string, graph: string) => string;
  approve: string;
  unknownError: string;
}

/** Ports that carry sound rather than text. The transcript is the visible half. */
const AUDIO_PORTS = new Set(["audio_in", "audio_chunk_in"]);

function last(lines: Line[]): Line | undefined {
  return lines[lines.length - 1];
}

/**
 * Fold one envelope into the transcript.
 *
 * Returns the same array when an envelope changes nothing, so a caller can
 * skip a render on the audio chunks that arrive many times a second.
 */
export function fold(lines: Line[], env: Envelope, vocab: Vocab): Line[] {
  const payload = (env.payload ?? {}) as Record<string, unknown>;

  // Audio is played, not written down; its transcript arrives separately.
  if (env.port && AUDIO_PORTS.has(env.port)) return lines;

  if (env.port === "transcript_in") {
    const text = String(payload.text ?? "");
    const settled = payload.final === true;
    const prev = last(lines);
    // Replace, never append — see the module docstring.
    if (prev?.kind === "user" && prev.partial) {
      return [...lines.slice(0, -1), { kind: "user", key: prev.key, text, partial: !settled }];
    }
    return [...lines, { kind: "user", key: env.id, text, partial: !settled }];
  }

  switch (env.kind) {
    case "status": {
      if (payload.state === "ready") {
        return [...lines, {
          kind: "status",
          key: env.id,
          text: vocab.sessionReady(String(payload.session ?? ""), String(payload.graph ?? "")),
        }];
      }
      if (payload.detail) {
        return [...lines, { kind: "status", key: env.id, text: String(payload.detail) }];
      }
      return lines;
    }

    case "data": {
      if (typeof payload.text !== "string") {
        return [...lines, { kind: "result", key: env.id, json: JSON.stringify(env.payload, null, 2) }];
      }
      const token = payload.text;
      const done = payload.final === true;
      const prev = last(lines);
      // Append — a model streams continuations.
      if (prev?.kind === "aura" && prev.open) {
        return [...lines.slice(0, -1), { ...prev, text: prev.text + token, open: !done }];
      }
      // An empty token that opens nothing is a keepalive, not a reply.
      if (!token) return lines;
      return [...lines, { kind: "aura", key: env.id, text: token, open: !done }];
    }

    case "done": {
      const prev = last(lines);
      if (prev?.kind !== "aura" || !prev.open) return lines;
      return [...lines.slice(0, -1), { ...prev, open: false }];
    }

    case "error":
      return [...lines, { kind: "error", key: env.id, text: String(payload.detail ?? vocab.unknownError) }];

    case "confirm_request":
      return [...lines, {
        kind: "gate",
        key: env.id,
        requestId: env.id,
        question: String(payload.question ?? vocab.approve),
      }];

    default:
      return lines;
  }
}

/** What the person typed, recorded locally — the kernel never echoes it back. */
export function spoken(lines: Line[], text: string): Line[] {
  return [...lines, { kind: "user", key: `u-${Date.now()}-${lines.length}`, text }];
}
