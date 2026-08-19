/**
 * The graph canvas — authoring C2 IR by drawing it.
 *
 * Until this view existed a graph was raw IR JSON or planner output, and the
 * Graphs view was a read-only JSON dump. The gap that mattered was not
 * aesthetic: the C2 fields that decide how a graph *behaves* under load —
 * `qos`, `speculative`, `deadline_ms`, `priority`, `gate` — were invisible
 * unless you already knew to look for them, so in practice nobody set them.
 *
 * Deliberately built without a graph library. react-flow and its peers are
 * 100–200 kB of dependency for pan, zoom, drag and bezier edges; this UI is
 * compiled into the kernel binary by `go:embed`, so every kilobyte here is a
 * kilobyte in a 26 MB executable that ships as one file. The interactions
 * below are a few hundred lines of pointer maths, and they owe nobody a
 * major-version migration.
 *
 * One coordinate system: a single transformed container holds the SVG edge
 * layer and the HTML node layer, so pan and zoom move both and screen↔world
 * conversion happens in exactly one function (`toWorld`).
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api/client";
import type { GraphIR, SkillManifest } from "../api/types";
import { GraphNode } from "../components/canvas/GraphNode";
import { Inspector } from "../components/canvas/Inspector";
import { LiveRail } from "../components/canvas/LiveRail";
import { useLiveGraphs } from "../hooks/useLiveGraphs";
import {
  type CanvasEdge,
  type CanvasGraph,
  type CanvasNode,
  NODE_W,
  autoLayout,
  candidateCount,
  fromIR,
  portOffsetY,
  resolvePorts,
  savePositions,
  suggestRef,
  toIR,
  validate,
} from "../graph/model";

type Selection = { kind: "node"; ref: string } | { kind: "edge"; id: string } | null;

interface Viewport {
  x: number;
  y: number;
  k: number;
}

interface LinkDrag {
  fromRef: string;
  fromPort: string;
  /** World coordinates of the pointer, for the rubber-band line. */
  x: number;
  y: number;
}

const EMPTY_GRAPH: CanvasGraph = {
  graphId: "",
  nodes: [{ ref: "client", x: 80, y: 160, isClient: true }],
  edges: [],
  origin: { kind: "declared" },
};

export function CanvasView() {
  const { t } = useTranslation();
  const surfaceRef = useRef<HTMLDivElement>(null);

  const [skills, setSkills] = useState<SkillManifest[]>([]);
  const [mode, setMode] = useState("local");
  const [following, setFollowing] = useState(true);
  const [graph, setGraph] = useState<CanvasGraph>(EMPTY_GRAPH);
  const [view, setView] = useState<Viewport>({ x: 0, y: 0, k: 1 });
  const [sel, setSel] = useState<Selection>(null);
  const [link, setLink] = useState<LinkDrag | null>(null);
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null);
  const [showIR, setShowIR] = useState(false);
  const [irDraft, setIrDraft] = useState("");
  const [paletteQuery, setPaletteQuery] = useState("");

  // Pointer gestures. Held in a ref rather than state: they change on every
  // pointermove and re-rendering the whole canvas 120 times a second to track a
  // drag would drop frames on exactly the interaction that must feel direct.
  const gesture = useRef<
    | { kind: "pan"; startX: number; startY: number; origin: Viewport }
    | { kind: "node"; ref: string; dx: number; dy: number }
    | null
  >(null);

  // What the node is running, whether or not this UI put it there.
  const live = useLiveGraphs(following);

  useEffect(() => {
    api.skills().then(setSkills).catch(() => setSkills([]));
    api.health().then((h) => setMode(h.mode)).catch(() => undefined);
  }, []);

  // The catalogue gains skills as they connect, so the palette has to keep
  // looking rather than reading once at mount.
  useEffect(() => {
    if (!following) return;
    const timer = setInterval(() => {
      api.skills().then(setSkills).catch(() => undefined);
    }, 5000);
    return () => clearInterval(timer);
  }, [following]);

  /* ── coordinates ───────────────────────────────────────────────── */

  const toWorld = useCallback(
    (clientX: number, clientY: number) => {
      const r = surfaceRef.current?.getBoundingClientRect();
      if (!r) return { x: 0, y: 0 };
      return { x: (clientX - r.left - view.x) / view.k, y: (clientY - r.top - view.y) / view.k };
    },
    [view],
  );

  const portsByRef = useMemo(() => {
    const m = new Map<string, ReturnType<typeof resolvePorts>>();
    for (const n of graph.nodes) m.set(n.ref, resolvePorts(n, skills));
    return m;
  }, [graph.nodes, skills]);

  const findings = useMemo(() => validate(graph, skills, mode), [graph, skills, mode]);

  /** World-space anchor of one port, where an edge should start or end. */
  const anchor = useCallback(
    (ref: string, port: string, side: "in" | "out") => {
      const n = graph.nodes.find((x) => x.ref === ref);
      if (!n) return null;
      const p = portsByRef.get(ref);
      if (!p) return null;
      const list = side === "in" ? p.ingress : p.egress;
      const i = list.findIndex((x) => x.name === port);
      if (i < 0) return null;
      return { x: n.x + (side === "in" ? 0 : NODE_W), y: n.y + portOffsetY(i) };
    },
    [graph.nodes, portsByRef],
  );

  /* ── gestures ──────────────────────────────────────────────────── */

  const onSurfacePointerDown = (e: React.PointerEvent) => {
    if (e.button !== 0 && e.button !== 1) return;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    gesture.current = { kind: "pan", startX: e.clientX, startY: e.clientY, origin: view };
    setSel(null);
  };

  const onNodePointerDown = (e: React.PointerEvent, ref: string) => {
    // A port dot handles its own pointerdown and stops propagation; anything
    // else inside the box means "move this node".
    if ((e.target as HTMLElement).classList.contains("gport__dot")) return;
    e.stopPropagation();
    const n = graph.nodes.find((x) => x.ref === ref);
    if (!n) return;
    const w = toWorld(e.clientX, e.clientY);
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    gesture.current = { kind: "node", ref, dx: w.x - n.x, dy: w.y - n.y };
  };

  const onPointerMove = (e: React.PointerEvent) => {
    const g = gesture.current;
    if (g?.kind === "pan") {
      setView({ ...g.origin, x: g.origin.x + (e.clientX - g.startX), y: g.origin.y + (e.clientY - g.startY) });
      return;
    }
    if (g?.kind === "node") {
      const w = toWorld(e.clientX, e.clientY);
      setGraph((prev) => ({
        ...prev,
        nodes: prev.nodes.map((n) => (n.ref === g.ref ? { ...n, x: w.x - g.dx, y: w.y - g.dy } : n)),
      }));
      return;
    }
    if (link) {
      const w = toWorld(e.clientX, e.clientY);
      setLink({ ...link, x: w.x, y: w.y });
    }
  };

  const onPointerUp = () => {
    if (gesture.current?.kind === "node" && graph.graphId) savePositions(graph.graphId, graph.nodes);
    gesture.current = null;
    // A link released over open canvas is abandoned. Dropping a node picker
    // here instead was tempting, but it turns a mis-drag into a modal.
    setLink(null);
  };

  const onWheel = (e: React.WheelEvent) => {
    e.preventDefault();
    const r = surfaceRef.current?.getBoundingClientRect();
    if (!r) return;
    const mx = e.clientX - r.left;
    const my = e.clientY - r.top;
    const k = Math.min(2.2, Math.max(0.25, view.k * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
    // Zoom about the cursor: the world point under the pointer must not move.
    setView({ k, x: mx - (mx - view.x) * (k / view.k), y: my - (my - view.y) * (k / view.k) });
  };

  /* ── linking ───────────────────────────────────────────────────── */

  const onPortDown = (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => {
    if (side === "in") return; // links are drawn source → destination
    e.stopPropagation();
    const a = anchor(ref, port, "out");
    setLink({ fromRef: ref, fromPort: port, x: a?.x ?? 0, y: a?.y ?? 0 });
  };

  const onPortUp = (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => {
    if (!link || side !== "in") return;
    e.stopPropagation();
    if (link.fromRef === ref && link.fromPort === port) {
      setLink(null);
      return;
    }
    const id = `e${Date.now().toString(36)}-${link.fromRef}.${link.fromPort}-${ref}.${port}`;
    const next: CanvasEdge = {
      id,
      fromRef: link.fromRef,
      fromPort: link.fromPort,
      toRef: ref,
      toPort: port,
    };
    // Rule 5, applied while drawing rather than at instantiation: an edge into a
    // skill that acts on the world gets its gate written down now, so the author
    // sees the decision instead of inheriting it silently from the kernel.
    if (portsByRef.get(ref)?.type === "motor") next.gate = "human-approval";
    setGraph((p) => ({ ...p, edges: [...p.edges, next] }));
    setSel({ kind: "edge", id });
    setLink(null);
  };

  /** While linking, only ingress ports with a compatible schema light up. */
  const dropTargets = useMemo(() => {
    if (!link) return null;
    const src = portsByRef.get(link.fromRef)?.egress.find((p) => p.name === link.fromPort);
    const out = new Set<string>();
    for (const n of graph.nodes) {
      for (const p of portsByRef.get(n.ref)?.ingress ?? []) {
        const major = (s: string) => s.split("@")[1]?.split(".")[0] ?? "";
        const base = (s: string) => s.split("@")[0];
        if (!src || (base(src.schema) === base(p.schema) && major(src.schema) === major(p.schema))) {
          out.add(`${n.ref}.${p.name}`);
        }
      }
    }
    return out;
  }, [link, graph.nodes, portsByRef]);

  /* ── graph mutation ────────────────────────────────────────────── */

  const addSkill = (s: SkillManifest) => {
    const taken = new Set(graph.nodes.map((n) => n.ref));
    const ref = suggestRef(s.capability || s.id, taken);
    const r = surfaceRef.current?.getBoundingClientRect();
    const centre = toWorld((r?.left ?? 0) + (r?.width ?? 800) / 2, (r?.top ?? 0) + (r?.height ?? 600) / 2);
    setGraph((p) => ({
      ...p,
      nodes: [...p.nodes, { ref, resolve: s.capability, x: centre.x - NODE_W / 2, y: centre.y - 60 }],
    }));
    setSel({ kind: "node", ref });
  };

  const patchNode = (patch: Partial<CanvasNode>) => {
    if (sel?.kind !== "node") return;
    const oldRef = sel.ref;
    setGraph((p) => ({
      ...p,
      nodes: p.nodes.map((n) => (n.ref === oldRef ? { ...n, ...patch } : n)),
      // Renaming a node has to carry its edges with it, or the graph silently
      // loses every connection the author already drew.
      edges: patch.ref
        ? p.edges.map((e) => ({
            ...e,
            fromRef: e.fromRef === oldRef ? patch.ref! : e.fromRef,
            toRef: e.toRef === oldRef ? patch.ref! : e.toRef,
          }))
        : p.edges,
    }));
    if (patch.ref) setSel({ kind: "node", ref: patch.ref });
  };

  const patchEdge = (patch: Partial<CanvasEdge>) => {
    if (sel?.kind !== "edge") return;
    setGraph((p) => ({
      ...p,
      edges: p.edges.map((e) => (e.id === sel.id ? { ...e, ...patch } : e)),
    }));
  };

  const deleteSelected = useCallback(() => {
    if (!sel) return;
    if (sel.kind === "node") {
      const n = graph.nodes.find((x) => x.ref === sel.ref);
      if (n?.isClient) return;
      setGraph((p) => ({
        ...p,
        nodes: p.nodes.filter((x) => x.ref !== sel.ref),
        edges: p.edges.filter((e) => e.fromRef !== sel.ref && e.toRef !== sel.ref),
      }));
    } else {
      setGraph((p) => ({ ...p, edges: p.edges.filter((e) => e.id !== sel.id) }));
    }
    setSel(null);
  }, [sel, graph.nodes]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      if (e.key === "Delete" || e.key === "Backspace") {
        e.preventDefault();
        deleteSelected();
      }
      if (e.key === "Escape") {
        setLink(null);
        setSel(null);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [deleteSelected]);

  /* ── load / save ───────────────────────────────────────────────── */

  const load = useCallback(async (id: string) => {
    try {
      const ir = await api.graph(id);
      setGraph(fromIR(ir));
      setSel(null);
      setStatus(null);
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  }, []);

  // A graph can appear without anyone drawing it: `aura do` compiles one from a
  // sentence, a federated peer brings its own, a webhook route instantiates one
  // on first request. While following, the canvas shows it rather than leaving
  // the operator to notice a new row and click it.
  //
  // Guarded on a clean slate: following must never discard work in progress, so
  // a graph that appears while the author has an unregistered drawing open is
  // listed in the rail and waits to be clicked.
  const drawing = graph.edges.length > 0 || graph.nodes.some((n) => !n.isClient);
  useEffect(() => {
    if (!following || !live.appeared.length || drawing) return;
    void load(live.appeared[live.appeared.length - 1]);
  }, [following, live.appeared, drawing, load]);

  const register = async () => {
    const blocking = findings.filter((f) => f.severity === "error");
    if (blocking.length) {
      setStatus({ kind: "err", text: t("canvas.status.blocked", { count: blocking.length }) });
      return;
    }
    try {
      const res = await api.registerGraph(toIR(graph));
      savePositions(graph.graphId, graph.nodes);
      setStatus({ kind: "ok", text: t("canvas.status.registered", { id: res.graph_id }) });
      live.refresh();
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  };

  const applyIR = () => {
    try {
      const parsed = JSON.parse(irDraft) as GraphIR;
      setGraph(fromIR(parsed));
      setShowIR(false);
      setStatus(null);
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  };

  const relayout = () => {
    setGraph((p) => ({ ...p, nodes: autoLayout(p.nodes, p.edges) }));
  };

  const selectedNode = sel?.kind === "node" ? graph.nodes.find((n) => n.ref === sel.ref) ?? null : null;
  const selectedEdge = sel?.kind === "edge" ? graph.edges.find((e) => e.id === sel.id) ?? null : null;
  const selFindings = findings.filter(
    (f) =>
      (sel?.kind === "node" && f.nodeRef === sel.ref) ||
      (sel?.kind === "edge" && f.edgeId === sel.id),
  );

  const errors = findings.filter((f) => f.severity === "error").length;
  const warns = findings.filter((f) => f.severity === "warn").length;

  const filteredSkills = skills.filter((s) => {
    const q = paletteQuery.toLowerCase();
    return !q || s.name.toLowerCase().includes(q) || s.capability.toLowerCase().includes(q) || s.id.toLowerCase().includes(q);
  });

  return (
    <section className="view view--flush canvas-view">
      <div className="canvas-bar">
        <input
          className="input canvas-bar__id"
          value={graph.graphId}
          placeholder={t("canvas.graphIdPlaceholder")}
          onChange={(e) => setGraph((p) => ({ ...p, graphId: e.target.value }))}
        />
        <select className="select canvas-bar__open" value="" onChange={(e) => e.target.value && load(e.target.value)}>
          <option value="">{t("canvas.open")}</option>
          {live.graphs.map((id) => (
            <option key={id} value={id}>
              {id}
            </option>
          ))}
        </select>
        <button className="btn btn--ghost btn--sm" onClick={() => { setGraph(EMPTY_GRAPH); setSel(null); }}>
          {t("canvas.new")}
        </button>
        <button className="btn btn--ghost btn--sm" onClick={relayout}>
          {t("canvas.relayout")}
        </button>
        <button
          className="btn btn--ghost btn--sm"
          onClick={() => {
            setIrDraft(JSON.stringify(toIR(graph), null, 2));
            setShowIR((s) => !s);
          }}
        >
          {t("canvas.ir")}
        </button>
        <span className="canvas-bar__spacer" />
        {(errors > 0 || warns > 0) && (
          <span className="canvas-bar__count">
            {errors > 0 && <span className="finding-dot finding-dot--error">{errors}</span>}
            {warns > 0 && <span className="finding-dot finding-dot--warn">{warns}</span>}
          </span>
        )}
        <span className="canvas-bar__zoom">{Math.round(view.k * 100)}%</span>
        <button className="btn btn--sm" onClick={register} disabled={errors > 0}>
          {t("canvas.register")}
        </button>
      </div>

      {status && (
        <div className={status.kind === "ok" ? "canvas-status canvas-status--ok" : "error-banner"}>
          {status.text}
        </div>
      )}

      <div className="canvas-body">
        <aside className="palette">
          <input
            className="input palette__search"
            placeholder={t("canvas.palette.search")}
            value={paletteQuery}
            onChange={(e) => setPaletteQuery(e.target.value)}
          />
          <div className="palette__list">
            {filteredSkills.length === 0 && <div className="palette__empty">{t("canvas.palette.empty")}</div>}
            {filteredSkills.map((s) => (
              <button key={s.id} className="palette__item" onClick={() => addSkill(s)} title={s.description}>
                <span className={`palette__dot palette__dot--${s.type}`} />
                <span className="palette__name">{s.name}</span>
                <span className="palette__cap">{s.capability}</span>
              </button>
            ))}
          </div>
        </aside>

        <LiveRail
          activity={live.activity}
          openId={graph.graphId}
          following={following}
          error={live.error}
          onToggleFollow={() => setFollowing((f) => !f)}
          onOpen={load}
        />

        <div
          className="canvas-surface"
          ref={surfaceRef}
          onPointerDown={onSurfacePointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onPointerCancel={onPointerUp}
          onWheel={onWheel}
        >
          <div
            className="canvas-world"
            style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }}
          >
            <svg className="canvas-edges" aria-hidden>
              {graph.edges.map((e) => {
                const a = anchor(e.fromRef, e.fromPort, "out");
                const b = anchor(e.toRef, e.toPort, "in");
                if (!a || !b) return null;
                const bad = findings.some((f) => f.edgeId === e.id && f.severity === "error");
                const cls = [
                  "cedge",
                  sel?.kind === "edge" && sel.id === e.id ? "cedge--sel" : "",
                  bad ? "cedge--bad" : "",
                  e.gate === "human-approval" ? "cedge--gated" : "",
                  e.speculative ? "cedge--spec" : "",
                ].join(" ");
                return (
                  <g key={e.id}>
                    <path className={cls} d={curve(a, b)} />
                    <path
                      className="cedge__hit"
                      d={curve(a, b)}
                      onPointerDown={(ev) => {
                        ev.stopPropagation();
                        setSel({ kind: "edge", id: e.id });
                      }}
                    />
                    {e.gate === "human-approval" && (
                      <circle className="cedge__gate" cx={(a.x + b.x) / 2} cy={(a.y + b.y) / 2} r={5} />
                    )}
                  </g>
                );
              })}
              {link &&
                (() => {
                  const a = anchor(link.fromRef, link.fromPort, "out");
                  return a ? <path className="cedge cedge--draft" d={curve(a, { x: link.x, y: link.y })} /> : null;
                })()}
            </svg>

            {graph.nodes.map((n) => (
              <GraphNode
                key={n.ref}
                node={n}
                ports={portsByRef.get(n.ref) ?? { ingress: [], egress: [] }}
                selected={sel?.kind === "node" && sel.ref === n.ref}
                dropTargets={dropTargets}
                candidates={candidateCount(n, skills)}
                onPointerDown={onNodePointerDown}
                onSelect={(ref) => setSel({ kind: "node", ref })}
                onPortDown={onPortDown}
                onPortUp={onPortUp}
              />
            ))}
          </div>

          {graph.nodes.length <= 1 && graph.edges.length === 0 && (
            <div className="canvas-hint">{t("canvas.hint")}</div>
          )}
        </div>

        <Inspector
          node={selectedNode}
          edge={selectedEdge}
          ports={selectedNode ? portsByRef.get(selectedNode.ref) ?? null : null}
          skills={skills}
          findings={selFindings}
          destIsMotor={!!selectedEdge && portsByRef.get(selectedEdge.toRef)?.type === "motor"}
          onNodeChange={patchNode}
          onEdgeChange={patchEdge}
          onDelete={deleteSelected}
        />
      </div>

      {showIR && (
        <div className="ir-drawer">
          <div className="ir-drawer__head">
            <span>{t("canvas.irTitle")}</span>
            <button className="btn btn--ghost btn--sm" onClick={applyIR}>
              {t("canvas.irApply")}
            </button>
            <button className="btn btn--ghost btn--sm" onClick={() => setShowIR(false)}>
              {t("common.close")}
            </button>
          </div>
          <textarea className="textarea ir-drawer__text" value={irDraft} onChange={(e) => setIrDraft(e.target.value)} />
        </div>
      )}

      {findings.length > 0 && (
        <div className="findings-bar">
          {findings.slice(0, 6).map((f, i) => (
            <button
              key={i}
              className={`finding finding--${f.severity}`}
              onClick={() => {
                if (f.edgeId) setSel({ kind: "edge", id: f.edgeId });
                else if (f.nodeRef) setSel({ kind: "node", ref: f.nodeRef });
              }}
            >
              {f.rule && <span className="finding__rule">C2 §{f.rule}</span>}
              {f.message}
            </button>
          ))}
        </div>
      )}
    </section>
  );
}

/**
 * A cubic bezier between two ports, with the control points pushed
 * horizontally. The push scales with the gap so short hops stay tight and long
 * ones bow enough to be followed across a busy graph, and it has a floor so a
 * node wired back to one on its left still bulges instead of folding into a
 * straight line through both boxes.
 */
function curve(a: { x: number; y: number }, b: { x: number; y: number }): string {
  const dx = Math.max(40, Math.abs(b.x - a.x) * 0.5);
  return `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`;
}
