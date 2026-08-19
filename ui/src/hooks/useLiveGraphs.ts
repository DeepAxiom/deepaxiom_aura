/**
 * What the node is running, right now.
 *
 * A graph does not only arrive by someone drawing it. `aura do` compiles one
 * from a sentence and registers it; a federated peer brings its own; a webhook
 * route instantiates one on the first request. All of those appear in the
 * node's catalogue without anyone touching this UI, and a control plane that
 * only shows what you opened by hand is not a control plane.
 *
 * So this polls the two endpoints that answer "what exists" and "what is
 * running", joins them per graph, and reports which graphs are new since the
 * last look — which is what lets the canvas follow a planner-generated graph
 * the moment it appears.
 *
 * Polling rather than a socket, deliberately: `/v1/stream` is a *session*
 * channel bound to one graph, not a control-plane event feed. Inventing a
 * second WebSocket protocol for this would be a change to C3's surface for the
 * sake of a two-second refresh, and the node already answers both of these
 * cheaply — sessions come from an index, not a scan.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import type { SessionMeta } from "../api/types";

export interface GraphActivity {
  graphId: string;
  /** Sessions with no end timestamp — the graph is connected and running. */
  live: number;
  sessions: number;
  events: number;
  errors: number;
  /** Start of the most recent session, 0 if it has never run. */
  lastStarted: number;
}

export interface LiveState {
  graphs: string[];
  activity: Map<string, GraphActivity>;
  liveSessions: SessionMeta[];
  /** Graphs seen for the first time on the most recent poll. */
  appeared: string[];
  error: string;
  /** True once the first poll has resolved, so the UI can tell empty from unknown. */
  loaded: boolean;
}

const EMPTY: LiveState = {
  graphs: [],
  activity: new Map(),
  liveSessions: [],
  appeared: [],
  error: "",
  loaded: false,
};

function join(graphs: string[], sessions: SessionMeta[]): Map<string, GraphActivity> {
  const out = new Map<string, GraphActivity>();
  for (const id of graphs) {
    out.set(id, { graphId: id, live: 0, sessions: 0, events: 0, errors: 0, lastStarted: 0 });
  }
  for (const s of sessions) {
    // A session may name a graph the catalogue no longer lists — one registered
    // for a single run, or removed since. It still ran, so it still counts.
    const a =
      out.get(s.graph_id) ??
      { graphId: s.graph_id, live: 0, sessions: 0, events: 0, errors: 0, lastStarted: 0 };
    a.sessions += 1;
    a.events += s.events;
    a.errors += s.errors;
    if (!s.ended) a.live += 1;
    if (s.started > a.lastStarted) a.lastStarted = s.started;
    out.set(s.graph_id, a);
  }
  return out;
}

/**
 * @param enabled poll while true; a single load still runs when false, so the
 *   view is populated before anyone turns following on.
 * @param intervalMs how often to look. 2 s is below the interval at which a
 *   person notices lag and far above what these two queries cost.
 */
export function useLiveGraphs(enabled: boolean, intervalMs = 2000): LiveState & { refresh: () => void } {
  const [state, setState] = useState<LiveState>(EMPTY);
  // Previous graph ids, for the "appeared" diff. A ref rather than state: it is
  // read during a poll and writing it must not schedule a render of its own.
  const seen = useRef<Set<string> | null>(null);

  const poll = useCallback(async () => {
    try {
      const [graphs, sessions] = await Promise.all([api.graphs(), api.sessions()]);
      const known = seen.current;
      // The first poll establishes the baseline; everything existing then is
      // not "new", or opening the view would announce the whole catalogue.
      const appeared = known ? graphs.filter((g) => !known.has(g)) : [];
      seen.current = new Set(graphs);
      setState({
        graphs,
        activity: join(graphs, sessions),
        liveSessions: sessions.filter((s) => !s.ended),
        appeared,
        error: "",
        loaded: true,
      });
    } catch (e) {
      // Keep the last good picture and surface the failure beside it: a node
      // restarting should not blank the canvas the author is working in.
      setState((prev) => ({ ...prev, appeared: [], error: (e as Error).message, loaded: true }));
    }
  }, []);

  useEffect(() => {
    void poll();
    if (!enabled) return;
    const timer = setInterval(() => void poll(), intervalMs);
    return () => clearInterval(timer);
  }, [enabled, intervalMs, poll]);

  return { ...state, refresh: poll };
}
