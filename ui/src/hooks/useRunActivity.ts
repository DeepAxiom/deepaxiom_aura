/**
 * What the graph is doing right now, at a frame rate a screen can use.
 *
 * A running session is not a slow trickle of events. A streaming Expertise
 * emits an envelope per token, so a single reply can be hundreds of messages a
 * second, and every one of them names a node. Calling setState on each would
 * re-render the canvas hundreds of times a second and drop frames on exactly
 * the interaction that has to look immediate.
 *
 * So envelopes land in a ref — no render — and one requestAnimationFrame per
 * frame publishes whatever accumulated. The highlight is a CSS class rather
 * than a layout change, and the cost of watching a graph run is bounded by the
 * display instead of by the graph.
 *
 * **Where the envelopes come from.** The session's client socket only carries
 * what is delivered to the client pseudo-node, so it cannot say what happened
 * between two Expertise. The session's event log can: it records every
 * emission and every delivery, each with its cause. `useSessionTail` reads
 * that log and feeds it here, which is why an edge lights when an envelope
 * actually crossed it and a node shows the text it is actually producing —
 * for any live session on the node, not only one this tab started.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import type { Envelope } from "../api/types";
import { type Usage, addUsage, edgeCrossing, usageOf } from "../lib/trace";

/** How long a node or edge stays lit after its last envelope. */
const ACTIVE_MS = 900;
/** Characters of streamed text kept per node — what fits under a node box. */
const TEXT_TAIL = 220;

export type NodeState = "idle" | "active" | "waiting" | "error" | "done";

export interface Activity {
  /** Per node ref: what it is doing. */
  nodes: Record<string, NodeState>;
  /** Envelope count per node, so a busy path is visibly busier. */
  counts: Record<string, number>;
  /** Envelopes that crossed each edge, keyed `from.port->to.port`. */
  edges: Record<string, number>;
  /** Edges an envelope crossed within the last moment. */
  hot: string[];
  /** Edges with a delivery held for a human. */
  holding: string[];
  /** Pending confirm_request ids. */
  gated: string[];
  /** The tail of the text each node is emitting; `open` while more is expected. */
  text: Record<string, { text: string; open: boolean }>;
  /** Tokens each node reported in `attest.usage`. */
  usage: Record<string, Usage>;
  /**
   * The last data payload each node produced, so a node's real output can be
   * pinned without asking the author to type it out. Read when someone pins,
   * never rendered.
   */
  lastPayload: Record<string, unknown>;
}

const EMPTY: Activity = {
  nodes: {}, counts: {}, edges: {}, hot: [], holding: [], gated: [], text: {}, usage: {}, lastPayload: {},
};

interface Pending {
  lastSeen: Map<string, number>;
  state: Map<string, NodeState>;
  counts: Map<string, number>;
  edges: Map<string, number>;
  edgeSeen: Map<string, number>;
  /** confirm_request id → { edge key, node ref held } */
  gates: Map<string, { edge: string; ref: string }>;
  text: Map<string, { text: string; open: boolean }>;
  usage: Map<string, Usage>;
  lastPayload: Record<string, unknown>;
  dirty: boolean;
}

function fresh(): Pending {
  return {
    lastSeen: new Map(), state: new Map(), counts: new Map(), edges: new Map(), edgeSeen: new Map(),
    gates: new Map(), text: new Map(), usage: new Map(), lastPayload: {}, dirty: true,
  };
}

export function useRunActivity(live: boolean) {
  const [activity, setActivity] = useState<Activity>(EMPTY);
  const pending = useRef<Pending>(fresh());
  const frame = useRef<number | null>(null);

  /**
   * Record one envelope from the session log. `cause` is the envelope it names
   * as its cause, when the caller has it — that pair is what identifies an edge.
   */
  const observe = useCallback((env: Envelope, cause?: Envelope) => {
    const p = pending.current;
    const ref = env.node || "client";
    const now = performance.now();
    const payload = (env.payload && typeof env.payload === "object" ? env.payload : {}) as Record<string, unknown>;
    p.dirty = true;

    if (env.kind === "confirm_request") {
      // Logged against the client, but it is about a delivery into `to_ref`:
      // that node is the one waiting, and that edge is the one holding.
      const held = String(payload.to_ref ?? ref);
      const edge = cause?.node && cause.port ? `${cause.node}.${cause.port}->${held}.${String(payload.to_port ?? "")}` : "";
      p.gates.set(env.id, { edge, ref: held });
      p.state.set(held, "waiting");
      p.lastSeen.set(held, now);
      return;
    }
    if (env.kind === "confirm_response") {
      const gate = p.gates.get(env.cause_id ?? "");
      if (gate) {
        p.gates.delete(env.cause_id ?? "");
        p.state.set(gate.ref, payload.approve === true ? "active" : "error");
        p.lastSeen.set(gate.ref, now);
      }
      return;
    }

    p.lastSeen.set(ref, now);
    p.counts.set(ref, (p.counts.get(ref) ?? 0) + 1);

    const crossed = edgeCrossing(env, cause);
    if (crossed) {
      p.edges.set(crossed, (p.edges.get(crossed) ?? 0) + 1);
      p.edgeSeen.set(crossed, now);
    }

    const usage = usageOf(env);
    if (usage) p.usage.set(ref, addUsage(p.usage.get(ref) ?? null, usage) ?? usage);

    // Text a node emits (not text delivered to it: that is its input).
    const emitted = !crossed && cause?.node === ref;
    if (env.kind === "data" && emitted && ref !== "client" && typeof payload.text === "string") {
      const prev = p.text.get(ref);
      const open = payload.final !== true;
      const base = prev?.open ? prev.text : "";
      const next = (base + payload.text).slice(-TEXT_TAIL);
      // A lone final marker closes the stream; it does not blank what was said.
      p.text.set(ref, { text: next || prev?.text || "", open });
    }
    if (env.kind === "data" && env.payload !== undefined && payload.elided !== true) p.lastPayload[ref] = env.payload;

    switch (env.kind) {
      case "error":
        p.state.set(ref, "error");
        break;
      case "done":
        p.state.set(ref, "done");
        break;
      default:
        // A node that is waiting stays waiting until its gate resolves: data
        // flowing past it elsewhere must not clear the thing a person is
        // being asked about.
        if (p.state.get(ref) !== "waiting") p.state.set(ref, "active");
    }
  }, []);

  const reset = useCallback(() => {
    pending.current = fresh();
    setActivity(EMPTY);
  }, []);

  useEffect(() => {
    if (!live) return;

    const tick = () => {
      const p = pending.current;
      const now = performance.now();
      const nodes: Record<string, NodeState> = {};
      const counts: Record<string, number> = {};

      for (const [ref, state] of p.state) {
        const quiet = now - (p.lastSeen.get(ref) ?? 0) > ACTIVE_MS;
        // `active` decays to idle; the terminal states do not — "this one
        // errored" is not something to forget half a second later.
        nodes[ref] = state === "active" && quiet ? "idle" : state;
        counts[ref] = p.counts.get(ref) ?? 0;
      }
      const hot: string[] = [];
      for (const [key, seen] of p.edgeSeen) if (now - seen <= ACTIVE_MS) hot.push(key);

      setActivity((prev) => {
        // Skip the render when nothing moved. A finished session would
        // otherwise keep React busy for as long as the view is open.
        if (
          !p.dirty &&
          shallowEqual(prev.nodes, nodes) &&
          prev.hot.length === hot.length &&
          prev.hot.every((h, i) => h === hot[i])
        ) {
          return prev;
        }
        p.dirty = false;
        const gates = [...p.gates.values()];
        return {
          nodes,
          counts,
          edges: Object.fromEntries(p.edges),
          hot,
          holding: gates.map((g) => g.edge).filter(Boolean),
          gated: [...p.gates.keys()],
          text: Object.fromEntries(p.text),
          usage: Object.fromEntries(p.usage),
          lastPayload: p.lastPayload,
        };
      });

      frame.current = requestAnimationFrame(tick);
    };

    frame.current = requestAnimationFrame(tick);
    return () => {
      if (frame.current !== null) cancelAnimationFrame(frame.current);
    };
  }, [live]);

  return { activity, observe, reset };
}

function shallowEqual<T>(a: Record<string, T>, b: Record<string, T>): boolean {
  const ka = Object.keys(a);
  if (ka.length !== Object.keys(b).length) return false;
  return ka.every((k) => a[k] === b[k]);
}

/**
 * Follow one session's event log and hand each new envelope, with its cause,
 * to `onEvent`. Polls: the log is an HTTP read, and 350 ms is under the
 * interval at which a moving edge looks late.
 */
export function useSessionTail(
  sessionId: string | null,
  onEvent: (env: Envelope, cause: Envelope | undefined) => void,
  intervalMs = 350,
): { events: number; error: string } {
  const [info, setInfo] = useState({ events: 0, error: "" });
  const handler = useRef(onEvent);
  handler.current = onEvent;

  useEffect(() => {
    if (!sessionId) { setInfo({ events: 0, error: "" }); return; }
    let alive = true;
    let busy = false;
    const seen = new Map<string, Envelope>();

    const poll = async () => {
      if (busy || document.hidden) return;
      busy = true;
      try {
        const d = await api.sessionEvents(sessionId);
        if (!alive) return;
        for (const env of d.events ?? []) {
          if (!env?.id || seen.has(env.id)) continue;
          seen.set(env.id, env);
          handler.current(env, env.cause_id ? seen.get(env.cause_id) : undefined);
        }
        setInfo((prev) => (prev.events === seen.size && !prev.error ? prev : { events: seen.size, error: "" }));
      } catch (e) {
        if (alive) setInfo((prev) => ({ ...prev, error: (e as Error).message }));
      } finally {
        busy = false;
      }
    };

    void poll();
    const timer = setInterval(() => void poll(), intervalMs);
    return () => { alive = false; clearInterval(timer); };
  }, [sessionId, intervalMs]);

  return info;
}
