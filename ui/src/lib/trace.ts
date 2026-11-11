/**
 * A session's event log, folded into the causal tree it already is.
 *
 * Every envelope names the one that caused it (`cause_id`), so the log the
 * kernel keeps is a tree written out in arrival order. This puts it back:
 * parent = cause, children in log order, and runs of sibling envelopes on the
 * same `node.port` collapsed into one row — a streamed reply is one row saying
 * "77 envelopes over 2.7 s", not 77 rows.
 *
 * The collapse is the only lossy step and it is reversible on screen: a row
 * keeps the ids it stands for.
 *
 * Pure. No React, no fetch, no clock.
 */

import type { Envelope } from "../api/types";

export interface Usage {
  input_tokens?: number;
  output_tokens?: number;
  cached_input_tokens?: number;
}

export interface TraceRow {
  /** Stable key: the first envelope id the row stands for. */
  key: string;
  ids: string[];
  depth: number;
  parent: string | null;
  kind: Envelope["kind"];
  node: string;
  port: string;
  schema: string;
  /** Envelopes collapsed into this row. 1 for anything that is not a stream. */
  count: number;
  /** Milliseconds from the session's first event to this row's first and last envelope. */
  t0: number;
  t1: number;
  /** Concatenated `payload.text` of the row, when it carries text. */
  text: string;
  /** Sum of `attest.usage` over the row; null when no envelope attested any. */
  usage: Usage | null;
  /** W3C trace id (the 32 hex digits), when the chain carries one. */
  traceId: string;
  /** Ledger receipt, on a delivery the kernel sealed. */
  receipt: string;
  error: string;
  /** Logged after its chain was cancelled: the work happened, the output went nowhere. */
  suppressed: boolean;
  /** Payload bytes the kernel elided on a realtime edge, summed. */
  elidedBytes: number;
  /** For confirm_request: the question. For confirm_response: "approve" / "deny". */
  note: string;
  hasChildren: boolean;
}

export interface Trace {
  rows: TraceRow[];
  /** Milliseconds from first to last event. */
  duration: number;
  events: number;
  errors: number;
  usage: Usage | null;
  traceIds: string[];
  receipts: string[];
}

interface Item {
  env: Envelope;
  at: number;
  index: number;
  children: Item[];
  suppressed: boolean;
}

function payloadOf(env: Envelope): Record<string, unknown> {
  return env.payload && typeof env.payload === "object" ? (env.payload as Record<string, unknown>) : {};
}

export function usageOf(env: Envelope): Usage | null {
  const u = (env.attest as { usage?: Usage } | undefined)?.usage;
  return u && typeof u === "object" ? u : null;
}

export function addUsage(a: Usage | null, b: Usage | null): Usage | null {
  if (!b) return a;
  if (!a) return { ...b };
  const out: Usage = { ...a };
  for (const k of ["input_tokens", "output_tokens", "cached_input_tokens"] as const) {
    if (b[k] !== undefined) out[k] = (out[k] ?? 0) + (b[k] ?? 0);
  }
  return out;
}

/** The 32-hex trace id out of a `traceparent`, or "" when it is not one. */
export function traceIdOf(traceparent: string | undefined): string {
  const m = /^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$/.exec(traceparent ?? "");
  return m ? m[1] : "";
}

function rowFor(group: Item[], depth: number, parent: string | null, start: number): TraceRow {
  const first = group[0].env;
  let text = "";
  let usage: Usage | null = null;
  let elided = 0;
  let error = "";
  let note = "";
  for (const { env } of group) {
    const p = payloadOf(env);
    if (typeof p.text === "string") text += p.text;
    if (p.elided === true && typeof p.bytes === "number") elided += p.bytes;
    usage = addUsage(usage, usageOf(env));
    if (env.kind === "error" && !error) error = String(p.detail ?? p.state ?? "error");
    if (env.kind === "confirm_request" && !note) note = String(p.question ?? "");
    if (env.kind === "confirm_response" && !note) note = p.approve === true ? "approve" : "deny";
  }
  return {
    key: first.id,
    ids: group.map((g) => g.env.id),
    depth,
    parent,
    kind: first.kind,
    node: first.node ?? "",
    port: first.port ?? "",
    schema: first.schema ?? "",
    count: group.length,
    t0: group[0].at - start,
    t1: group[group.length - 1].at - start,
    text,
    usage,
    traceId: traceIdOf(group.find((g) => g.env.trace)?.env.trace),
    receipt: group.find((g) => g.env.receipt)?.env.receipt ?? "",
    error,
    suppressed: group.every((g) => g.suppressed),
    elidedBytes: elided,
    note,
    hasChildren: group.some((g) => g.children.length > 0),
  };
}

/**
 * Fold a session's events into rows, depth-first.
 *
 * `timestamps` is index-aligned with `events` (unix millis, as the kernel
 * returns it). When it is missing every row sits at t=0: order is still right,
 * timing is simply not known, and the caller can see that from `duration`.
 */
export function foldTrace(events: readonly Envelope[], timestamps?: readonly number[]): Trace {
  // The log can hold the same envelope twice (a confirm_response is recorded
  // on receipt and again on resolution). One id is one event.
  const byId = new Map<string, Item>();
  const items: Item[] = [];
  events.forEach((env, index) => {
    if (!env || !env.id || byId.has(env.id)) return;
    const item: Item = { env, at: timestamps?.[index] ?? 0, index, children: [], suppressed: false };
    byId.set(env.id, item);
    items.push(item);
  });

  const roots: Item[] = [];
  const rootOf = new Map<string, string>();
  for (const item of items) {
    const parent = item.env.cause_id ? byId.get(item.env.cause_id) : undefined;
    // A cause that is not in the log (trimmed, or from another session) makes
    // the envelope a root rather than dropping it from the picture.
    if (parent && parent !== item) {
      parent.children.push(item);
      rootOf.set(item.env.id, rootOf.get(parent.env.id) ?? parent.env.id);
    } else {
      roots.push(item);
      rootOf.set(item.env.id, item.env.id);
    }
  }

  // Cancel: everything of that chain logged after the cancel was suppressed.
  for (const c of items) {
    if (c.env.kind !== "cancel" || !c.env.cause_id) continue;
    const chain = rootOf.get(c.env.cause_id);
    if (!chain) continue;
    for (const item of items) {
      if (item.index > c.index && item !== c && rootOf.get(item.env.id) === chain && item.env.kind !== "cancel") {
        item.suppressed = true;
      }
    }
  }

  const start = items.length ? Math.min(...items.map((i) => i.at)) : 0;
  const rows: TraceRow[] = [];

  const emit = (siblings: Item[], depth: number, parent: string | null) => {
    // Group siblings by where they are and what they are, keeping first-seen order.
    const groups = new Map<string, Item[]>();
    for (const s of siblings) {
      const collapsible = s.env.kind === "data";
      const key = collapsible ? `${s.env.node}.${s.env.port}|${s.suppressed}` : `#${s.env.id}`;
      const g = groups.get(key);
      if (g) g.push(s); else groups.set(key, [s]);
    }
    for (const group of groups.values()) {
      const row = rowFor(group, depth, parent, start);
      rows.push(row);
      const kids = group.flatMap((g) => g.children).sort((a, b) => a.index - b.index);
      if (kids.length) emit(kids, depth + 1, row.key);
    }
  };
  emit(roots, 0, null);

  let usage: Usage | null = null;
  const traceIds = new Set<string>();
  const receipts = new Set<string>();
  let errors = 0;
  for (const { env } of items) {
    usage = addUsage(usage, usageOf(env));
    const tid = traceIdOf(env.trace);
    if (tid) traceIds.add(tid);
    if (env.receipt) receipts.add(env.receipt);
    if (env.kind === "error") errors += 1;
  }

  return {
    rows,
    duration: items.length ? Math.max(...items.map((i) => i.at)) - start : 0,
    events: items.length,
    errors,
    usage,
    traceIds: [...traceIds],
    receipts: [...receipts],
  };
}

export interface TraceFilter {
  query: string;
  /** Only rows of these kinds; empty means all. */
  kinds: string[];
  errorsOnly: boolean;
}

/** Rows matching a filter, plus every ancestor of a match so the tree still reads as one. */
export function filterTrace(rows: readonly TraceRow[], f: TraceFilter): TraceRow[] {
  const q = f.query.trim().toLowerCase();
  if (!q && !f.kinds.length && !f.errorsOnly) return rows.slice();
  const byKey = new Map(rows.map((r) => [r.key, r]));
  const keep = new Set<string>();
  for (const r of rows) {
    if (f.errorsOnly && !r.error) continue;
    if (f.kinds.length && !f.kinds.includes(r.kind)) continue;
    if (q) {
      const hay = `${r.node}.${r.port} ${r.kind} ${r.schema} ${r.text} ${r.error} ${r.note} ${r.traceId} ${r.receipt} ${r.ids.join(" ")}`.toLowerCase();
      if (!hay.includes(q)) continue;
    }
    for (let cur: TraceRow | undefined = r; cur && !keep.has(cur.key); cur = cur.parent ? byKey.get(cur.parent) : undefined) {
      keep.add(cur.key);
    }
  }
  return rows.filter((r) => keep.has(r.key));
}

/** Hide the descendants of collapsed rows. */
export function visibleRows(rows: readonly TraceRow[], collapsed: ReadonlySet<string>): TraceRow[] {
  const hidden = new Set<string>();
  const out: TraceRow[] = [];
  for (const r of rows) {
    if (r.parent && (hidden.has(r.parent) || collapsed.has(r.parent))) {
      hidden.add(r.key);
      continue;
    }
    out.push(r);
  }
  return out;
}

/**
 * Which graph edge an envelope travelled, for the canvas.
 *
 * The log records an emission at `from.port` and then a delivery at `to.port`
 * whose cause is that emission, so an edge crossing is exactly a
 * (cause, effect) pair on two different node.ports.
 */
export function edgeCrossing(env: Envelope, cause: Envelope | undefined): string | null {
  if (!cause || !env.node || !env.port || !cause.node || !cause.port) return null;
  if (env.kind !== "data" && env.kind !== "done") return null;
  if (cause.node === env.node) return null;
  return `${cause.node}.${cause.port}->${env.node}.${env.port}`;
}
