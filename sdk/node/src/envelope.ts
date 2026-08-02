import type { Envelope } from "./generated/types.js";

export const PROTOCOL_MAJOR = "1";

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/**
 * A C3 message id: 10 characters of millisecond timestamp followed by 16
 * random ones, Crockford base32.
 *
 * The timestamp prefix is not decoration — C3 calls the id lexicographically
 * sortable, which is what lets a causal event log be ordered by id alone.
 */
export function newId(): string {
  let ms = Date.now();
  const ts = new Array<string>(10);
  for (let i = 9; i >= 0; i--) {
    ts[i] = CROCKFORD.charAt(ms % 32);
    ms = Math.floor(ms / 32);
  }
  const random = new Uint8Array(16);
  crypto.getRandomValues(random);
  let suffix = "";
  for (const byte of random) suffix += CROCKFORD.charAt(byte % 32);
  return ts.join("") + suffix;
}

export function newSessionId(): string {
  const bytes = new Uint8Array(6);
  crypto.getRandomValues(bytes);
  return "sess-" + [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Build an envelope, filling in the fields every one of them carries. */
export function envelope(fields: Partial<Envelope> & Pick<Envelope, "kind">): Envelope {
  return { v: PROTOCOL_MAJOR, id: newId(), ...fields } as Envelope;
}

export type { Envelope };
