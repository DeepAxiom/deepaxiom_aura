/**
 * A graph you watch rather than edit.
 *
 * Shares the model layer and the node box with the editor — the same
 * `fromIR`, the same `GraphNode`, the same bezier — so a graph looks identical
 * whether you drew it or the planner did. What it drops is everything that
 * writes: no palette, no inspector, no dragging, no port targets.
 *
 * The only thing it adds is `activity`, which arrives already batched to one
 * update per frame (see useRunActivity). Every state it paints is a CSS class
 * on an element that is already there — no node moves, so nothing re-layouts
 * while a session runs.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import type { GraphIR, SkillManifest } from "../../api/types";
import { curve, portAnchor } from "../../graph/geometry";
import { NODE_W, autoLayout, fromIR, nodeHeight, resolvePorts, splitPortRef } from "../../graph/model";
import type { Activity } from "../../hooks/useRunActivity";
import { GraphNode } from "./GraphNode";

interface Props {
  ir: GraphIR | null;
  skills: SkillManifest[];
  activity: Activity;
  /** Shown centred when there is no graph yet. */
  placeholder: string;
}

export function LiveGraph({ ir, skills, activity, placeholder }: Props) {
  const surface = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ x: 0, y: 0, k: 1 });

  // Laid out from topology every time: a planner graph has never been opened
  // before, so there are no remembered positions to honour.
  const graph = useMemo(() => {
    if (!ir) return null;
    const g = fromIR(ir);
    return { ...g, nodes: autoLayout(g.nodes, g.edges) };
  }, [ir]);

  const ports = useMemo(() => {
    const m = new Map<string, ReturnType<typeof resolvePorts>>();
    for (const n of graph?.nodes ?? []) m.set(n.ref, resolvePorts(n, skills));
    return m;
  }, [graph, skills]);

  // Fit the graph when it arrives. A planner graph can be wider than the pane,
  // and a view that opens mid-graph looks broken rather than merely scrolled.
  useEffect(() => {
    if (!graph || !surface.current) return;
    const r = surface.current.getBoundingClientRect();
    const xs = graph.nodes.map((n) => n.x);
    const ys = graph.nodes.map((n) => n.y);
    if (!xs.length) return;
    // An edge that runs right-to-left — the reply hop back to `client`, which
    // almost every graph has — bows outward past both of its endpoints by half
    // the horizontal gap. Fitting to the node boxes alone therefore clips the
    // one edge every graph is guaranteed to contain.
    // `curve` pushes its control points out by half the horizontal gap, and for
    // a backward edge that gap is measured from one node's right face to the
    // other's left — so the widest bow is half of (span + one node's width),
    // not half the span. Using the span alone left the reply edge clipped.
    const span = Math.max(...xs) - Math.min(...xs);
    const bow = Math.max(60, (span + NODE_W) * 0.5);

    const left = Math.min(...xs) - bow;
    const w = Math.max(...xs) + NODE_W + bow - left;
    const top = Math.min(...ys);
    const h =
      Math.max(
        ...graph.nodes.map(
          (n) => n.y + nodeHeight(ports.get(n.ref) ?? { ingress: [], egress: [] }),
        ),
      ) - top;

    const k = Math.min(1, Math.min((r.width - 48) / w, (r.height - 48) / h));
    const scale = Number.isFinite(k) && k > 0.15 ? k : 1;
    setView({ k: scale, x: 24 - left * scale, y: 24 - top * scale });
  }, [graph, ports]);

  if (!graph) {
    return (
      <div className="livegraph livegraph--empty">
        <span>{placeholder}</span>
      </div>
    );
  }

  return (
    <div
      className="livegraph"
      ref={surface}
      onWheel={(e) => {
        const r = surface.current?.getBoundingClientRect();
        if (!r) return;
        const mx = e.clientX - r.left;
        const my = e.clientY - r.top;
        const k = Math.min(2, Math.max(0.2, view.k * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
        setView({ k, x: mx - (mx - view.x) * (k / view.k), y: my - (my - view.y) * (k / view.k) });
      }}
    >
      <div
        className="canvas-world"
        style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }}
      >
        <svg className="canvas-edges" aria-hidden>
          {graph.edges.map((e) => {
            const [fr, fp] = [e.fromRef, e.fromPort];
            const [tr, tp] = [e.toRef, e.toPort];
            const a = portAnchor(graph.nodes.find((n) => n.ref === fr), ports.get(fr), fp, "out");
            const b = portAnchor(graph.nodes.find((n) => n.ref === tr), ports.get(tr), tp, "in");
            if (!a || !b) return null;
            // An edge is "live" when the node feeding it is: the traffic is on
            // the edge, but only its endpoints are named in an envelope.
            const hot = activity.nodes[fr] === "active";
            const waiting = activity.nodes[tr] === "waiting";
            const cls = [
              "cedge",
              e.gate === "human-approval" ? "cedge--gated" : "",
              hot ? "cedge--hot" : "",
              waiting ? "cedge--holding" : "",
            ].join(" ");
            return (
              <g key={e.id}>
                <path className={cls} d={curve(a, b)} />
                {e.gate === "human-approval" && (
                  <circle
                    className={`cedge__gate ${waiting ? "cedge__gate--waiting" : ""}`}
                    cx={(a.x + b.x) / 2}
                    cy={(a.y + b.y) / 2}
                    r={5}
                  />
                )}
              </g>
            );
          })}
        </svg>

        {graph.nodes.map((n) => {
          const state = activity.nodes[n.ref] ?? "idle";
          const count = activity.counts[n.ref] ?? 0;
          return (
            <div key={n.ref} className={`livenode livenode--${state}`}>
              <GraphNode
                node={n}
                ports={ports.get(n.ref) ?? { ingress: [], egress: [] }}
                selected={false}
                dropTargets={null}
                candidates={0}
                onPointerDown={() => {}}
                onSelect={() => {}}
                onPortDown={() => {}}
                onPortUp={() => {}}
              />
              {count > 0 && (
                <span
                  className="livenode__count"
                  style={{ left: n.x, top: n.y - 9 }}
                  title="envelopes through this node"
                >
                  {count}
                </span>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

/** Re-exported for the studio's own edge lookups. */
export { splitPortRef };
