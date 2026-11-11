/**
 * Derivations over GET /v1/stats.
 *
 * The node reports counters; the screen wants rates and ratios. Everything
 * between the two lives here, pure, so the arithmetic that decides what a
 * person reads off the Pulse view is the arithmetic the tests assert on.
 *
 * Absent members stay absent. The kernel omits a section it does not run (no
 * exporter, inline dispatch), and "not in use" must not be drawn as zero.
 */

export interface ExpertiseStats {
  id: string;
  capability: string;
  type: string;
  /** Live-session load of each connection of this package, oldest first. */
  replicas: number[];
  sessions: number;
}

export interface NodeStats {
  at: number;
  uptime_s: number;
  version?: string;
  sessions_live: number;
  expertise: ExpertiseStats[];
  mailbox?: { enabled: boolean; depth: number; queued: number; waits: number; dropped: number };
  tokens?: { input_tokens: number; output_tokens: number; cached_input_tokens: number; inferences: number };
  speculation?: { attempts: number; hits: number; misses: number; refused: number };
  traces?: { exported: number; dropped: number; failed: number; sampled_out: number };
  store?: { commits: number; rows: number; batch_mean: number; batch_largest: number; commit_ms_mean: number };
  event_log?: { bytes: number; segments: number; sessions: number };
  approvals_pending?: number;
  /** Read-only observers attached to live sessions (GET /v1/watch). */
  watch?: { watchers: number; cut_off: number };
}

/** a / b, or null when there is nothing to divide by. */
export function ratio(a: number | undefined, b: number | undefined): number | null {
  if (a === undefined || b === undefined || !Number.isFinite(a) || !Number.isFinite(b) || b <= 0) return null;
  return a / b;
}

/** Share of prompt tokens served from the prefix cache. */
export function cacheHitRatio(s: NodeStats | null): number | null {
  return s?.tokens ? ratio(s.tokens.cached_input_tokens, s.tokens.input_tokens) : null;
}

/**
 * Speculative deliveries that turned out right, over those that resolved.
 * `refused` never ran, so it is not in the denominator.
 */
export function speculationHitRate(s: NodeStats | null): number | null {
  if (!s?.speculation) return null;
  return ratio(s.speculation.hits, s.speculation.hits + s.speculation.misses);
}

/** Queued envelopes as a share of one mailbox's depth, clamped to [0,1]. */
export function mailboxFill(s: NodeStats | null): number | null {
  if (!s?.mailbox || !s.mailbox.enabled) return null;
  const r = ratio(s.mailbox.queued, s.mailbox.depth);
  return r === null ? null : Math.min(1, r);
}

/**
 * Per-second rate of a counter between two samples.
 *
 * Null when the samples are not usable as a pair: the same instant, time
 * running backwards, or the counter going down — which is a node restart, and
 * a restart drawn as a huge negative spike would be the chart's loudest
 * feature while meaning nothing.
 */
export function rate(prevValue: number | undefined, value: number | undefined, prevAt: number, at: number): number | null {
  if (prevValue === undefined || value === undefined) return null;
  const dt = (at - prevAt) / 1000;
  if (dt <= 0 || value < prevValue) return null;
  return (value - prevValue) / dt;
}

export interface Rates {
  tokensIn: number | null;
  tokensOut: number | null;
  inferences: number | null;
  commits: number | null;
  rows: number | null;
  mailboxWaits: number | null;
}

export function ratesBetween(prev: NodeStats, cur: NodeStats): Rates {
  const r = (a: number | undefined, b: number | undefined) => rate(a, b, prev.at, cur.at);
  return {
    tokensIn: r(prev.tokens?.input_tokens, cur.tokens?.input_tokens),
    tokensOut: r(prev.tokens?.output_tokens, cur.tokens?.output_tokens),
    inferences: r(prev.tokens?.inferences, cur.tokens?.inferences),
    commits: r(prev.store?.commits, cur.store?.commits),
    rows: r(prev.store?.rows, cur.store?.rows),
    mailboxWaits: r(prev.mailbox?.waits, cur.mailbox?.waits),
  };
}

/** Append to a bounded window, returning a new array (React state friendly). */
export function pushBounded<T>(window: readonly T[], item: T, max: number): T[] {
  const next = window.length >= max ? window.slice(window.length - max + 1) : window.slice();
  next.push(item);
  return next;
}

/** One series out of a window of samples; a sample with no predecessor has no rate. */
export function rateSeries(samples: readonly NodeStats[], pick: (r: Rates) => number | null): (number | null)[] {
  const out: (number | null)[] = [];
  for (let i = 1; i < samples.length; i += 1) out.push(pick(ratesBetween(samples[i - 1], samples[i])));
  return out;
}

export interface ReplicaView {
  id: string;
  capability: string;
  type: string;
  replicas: number[];
  sessions: number;
  /** Largest minus smallest replica load. 0 or 1 is what load-aware routing produces. */
  spread: number;
}

/** Expertise rows for the load panel: busiest first, then by id so the order is stable. */
export function replicaViews(s: NodeStats | null): ReplicaView[] {
  return (s?.expertise ?? [])
    .map((e) => {
      const loads = e.replicas ?? [];
      return {
        id: e.id, capability: e.capability, type: e.type, replicas: loads, sessions: e.sessions,
        spread: loads.length ? Math.max(...loads) - Math.min(...loads) : 0,
      };
    })
    .sort((a, b) => b.sessions - a.sessions || a.id.localeCompare(b.id));
}

/** The scale every replica bar shares, so two Expertise are comparable by eye. Never below 1. */
export function replicaScale(views: readonly ReplicaView[]): number {
  let max = 1;
  for (const v of views) for (const load of v.replicas) if (load > max) max = load;
  return max;
}

/**
 * Points for a sparkline polyline in a `w`×`h` box. Nulls break the line into
 * segments rather than being drawn as zero.
 */
export function sparkSegments(values: readonly (number | null)[], w: number, h: number, slots = values.length): string[] {
  const finite = values.filter((v): v is number => v !== null);
  if (!finite.length) return [];
  const max = Math.max(...finite, 1e-9);
  const step = slots > 1 ? w / (slots - 1) : w;
  const offset = slots - values.length;
  const segments: string[] = [];
  let run: string[] = [];
  values.forEach((v, i) => {
    if (v === null) {
      if (run.length) segments.push(run.join(" "));
      run = [];
      return;
    }
    const x = (offset + i) * step;
    const y = h - 1 - (v / max) * (h - 2);
    run.push(`${x.toFixed(1)},${y.toFixed(1)}`);
  });
  if (run.length) segments.push(run.join(" "));
  return segments;
}
