/**
 * What the graph is doing right now, at a frame rate a screen can use.
 *
 * A running session is not a slow trickle of events. A streaming skill emits an
 * envelope per token, so a single reply can be hundreds of messages a second,
 * and every one of them names a node. Calling setState on each would re-render
 * the canvas hundreds of times a second and drop frames on exactly the
 * interaction that has to look immediate.
 *
 * So envelopes land in a ref — no render — and one requestAnimationFrame per
 * frame publishes whatever accumulated. Five hundred envelopes a second become
 * sixty renders a second, the highlight is a CSS class rather than a layout
 * change, and the cost of watching a graph run is bounded by the display
 * instead of by the graph.
 *
 * Nothing here talks to the node. These envelopes already arrive for the
 * session the view is running; this only decides how often React hears about
 * them. Watching a graph therefore costs the kernel nothing at all.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import type { Envelope } from "../api/types";

/** How long a node keeps glowing after its last envelope. */
const ACTIVE_MS = 900;

export type NodeState = "idle" | "active" | "waiting" | "error" | "done";

export interface Activity {
  /** Per node ref: what it is doing. */
  nodes: Record<string, NodeState>;
  /** Envelope count per node, so a busy path is visibly busier. */
  counts: Record<string, number>;
  /** Edge ids currently holding for a human. */
  gated: string[];
}

const EMPTY: Activity = { nodes: {}, counts: {}, gated: [] };

interface Pending {
  lastSeen: Map<string, number>;
  state: Map<string, NodeState>;
  counts: Map<string, number>;
  gated: Set<string>;
}

export function useRunActivity(live: boolean) {
  const [activity, setActivity] = useState<Activity>(EMPTY);

  // Everything mutable lives here: written on every envelope, read once a
  // frame. A ref rather than state is the whole point — see the module note.
  const pending = useRef<Pending>({
    lastSeen: new Map(),
    state: new Map(),
    counts: new Map(),
    gated: new Set(),
  });
  const frame = useRef<number | null>(null);

  /** Record one envelope. Cheap by construction: three map writes, no render. */
  const observe = useCallback((env: Envelope) => {
    const p = pending.current;
    // `node` is the skill ref the envelope came from or is bound for. The
    // client's own emissions carry "client", which is a real node on the canvas
    // and should light up like any other.
    const ref = env.node || "client";
    const now = performance.now();

    p.lastSeen.set(ref, now);
    p.counts.set(ref, (p.counts.get(ref) ?? 0) + 1);

    switch (env.kind) {
      case "confirm_request":
        p.state.set(ref, "waiting");
        p.gated.add(env.id);
        break;
      case "confirm_response":
        p.gated.delete(env.cause_id ?? "");
        p.state.set(ref, "active");
        break;
      case "error":
        p.state.set(ref, "error");
        break;
      case "done":
        p.state.set(ref, "done");
        break;
      default:
        // A node that was waiting stays waiting until its gate resolves: data
        // flowing past it elsewhere must not clear the thing a person is
        // being asked about.
        if (p.state.get(ref) !== "waiting") p.state.set(ref, "active");
    }
  }, []);

  const reset = useCallback(() => {
    pending.current = {
      lastSeen: new Map(), state: new Map(), counts: new Map(), gated: new Set(),
    };
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

      setActivity((prev) => {
        const gated = [...p.gated];
        // Skip the render when nothing moved. A finished session would
        // otherwise keep React busy for as long as the view is open.
        if (
          shallowEqual(prev.nodes, nodes) &&
          shallowEqual(prev.counts, counts) &&
          prev.gated.length === gated.length &&
          prev.gated.every((g, i) => g === gated[i])
        ) {
          return prev;
        }
        return { nodes, counts, gated };
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
