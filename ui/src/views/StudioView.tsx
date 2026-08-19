/**
 * The studio — build a graph, run it, and watch it run, on one canvas.
 *
 * This replaced two views. Canvas could draw a graph but never showed one
 * working; Operate could run one but showed it as a list of steps beside a
 * result. The thing an author actually wants is both at once: the graph they
 * are editing is the graph that lights up, in the same coordinates, so "what
 * did I build" and "what is it doing" are one picture instead of two screens
 * to correlate by hand.
 *
 * **Two modes over one surface.** Editing is on when nothing is running:
 * drag, wire, multi-select, undo. It goes read-only the moment a session
 * starts, because moving a node mid-run means asking "did it move because I
 * dragged it, or because something happened?" — and the answer must never be
 * ambiguous while you are watching for a failure.
 *
 * **What is live and what is not.** Planning is not: the planner emits one
 * complete `std/plan@1`, so the graph appears whole. Execution is: every
 * envelope names its node, so nodes glow as data reaches them and a gate
 * pulses while it waits for you.
 *
 * **It costs the node nothing.** Those envelopes already arrive on the
 * session's socket. The one real cost is client-side, which is why activity is
 * batched to one render per frame (`useRunActivity`) and every state it paints
 * is a class on an element already in the DOM — no node moves during a run, so
 * nothing re-layouts.
 *
 * Every rule about *what* an edit does lives in `graph/editor.ts`, which is
 * pure and tested. This file decides *when*.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, newId, streamURL } from "../api/client";
import type { Envelope, GraphIR, Plan, SkillManifest } from "../api/types";
import { GateCard } from "../components/GateCard";
import { GraphNode } from "../components/canvas/GraphNode";
import { Inspector } from "../components/canvas/Inspector";
import { LiveRail } from "../components/canvas/LiveRail";
import { Minimap } from "../components/canvas/Minimap";
import { ConversePanel } from "../components/converse/ConversePanel";
import { curve, portAnchor } from "../graph/geometry";
import {
  type CanvasEdge,
  type CanvasGraph,
  type CanvasNode,
  NODE_W,
  autoLayout,
  candidateCount,
  fromIR,
  nodeHeight,
  portMap,
  savePositions,
  toIR,
  validate,
} from "../graph/model";
import {
  type Clipboard,
  type History,
  type Selection,
  EMPTY_HISTORY,
  EMPTY_SELECTION,
  addSkill,
  commit,
  connect,
  copy,
  isSelected,
  moveBy,
  paste,
  redo,
  remove,
  rename,
  select,
  selectWithin,
  undo,
} from "../graph/editor";
import { useLiveGraphs } from "../hooks/useLiveGraphs";
import { useRunActivity } from "../hooks/useRunActivity";

type Item =
  | { kind: "status"; text: string; key: string }
  | { kind: "result"; json: string; key: string }
  | { kind: "gate"; question: string; requestId: string; key: string }
  | { kind: "error"; text: string; key: string };

type Phase = "idle" | "planning" | "executing" | "done" | "failed";

interface Viewport { x: number; y: number; k: number }

const EMPTY_GRAPH: CanvasGraph = {
  graphId: "",
  nodes: [{ ref: "client", x: 90, y: 180, isClient: true }],
  edges: [],
  origin: { kind: "declared" },
};

export function StudioView() {
  const { t } = useTranslation();
  const surfaceRef = useRef<HTMLDivElement>(null);

  const [graph, setGraph] = useState<CanvasGraph>(EMPTY_GRAPH);
  const [history, setHistory] = useState<History>(EMPTY_HISTORY);
  const [clipboard, setClipboard] = useState<Clipboard | null>(null);
  const [sel, setSel] = useState<Selection>(EMPTY_SELECTION);
  const [view, setView] = useState<Viewport>({ x: 0, y: 0, k: 1 });
  const [surfaceSize, setSurfaceSize] = useState({ w: 800, h: 600 });

  const [skills, setSkills] = useState<SkillManifest[]>([]);
  const [mode, setMode] = useState("local");
  const [paletteQuery, setPaletteQuery] = useState("");
  const [showIR, setShowIR] = useState(false);
  /**
   * Which pane the right column shows.
   *
   * "inspect" is implicit rather than sticky — selecting a node means you want
   * its fields, and making that a click as well would be a click nobody would
   * thank us for. The conversation, by contrast, is a place you go and stay,
   * so it is the one an author picks and keeps.
   */
  const [side, setSide] = useState<"auto" | "converse">("auto");
  const [irDraft, setIrDraft] = useState("");
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null);

  const [goal, setGoal] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [items, setItems] = useState<Item[]>([]);
  const execWS = useRef<WebSocket | null>(null);

  const running = phase === "planning" || phase === "executing";
  const editable = !running;
  const { activity, observe, reset: resetActivity } = useRunActivity(running);
  // Pause the catalogue poll while a session runs: its envelopes already say
  // what is happening, and polling on top would be duplicated work.
  const live = useLiveGraphs(!running);

  /* ── gestures ──────────────────────────────────────────────────── */

  const gesture = useRef<
    | { kind: "pan"; startX: number; startY: number; origin: Viewport }
    | { kind: "move"; refs: string[]; lastX: number; lastY: number }
    | { kind: "box"; startX: number; startY: number }
    | null
  >(null);
  const [box, setBox] = useState<{ x: number; y: number; w: number; h: number } | null>(null);
  const [link, setLink] = useState<{ ref: string; port: string; x: number; y: number } | null>(null);

  const push = useCallback((item: Item) => setItems((prev) => [...prev, item]), []);

  /** Every edit goes through here, so undo always has the state before it. */
  const edit = useCallback((next: (g: CanvasGraph) => CanvasGraph) => {
    setGraph((prev) => {
      setHistory((h) => commit(h, prev));
      return next(prev);
    });
  }, []);

  useEffect(() => {
    api.skills().then(setSkills).catch(() => setSkills([]));
    api.health().then((h) => setMode(h.mode)).catch(() => undefined);
  }, []);

  useEffect(() => {
    if (running) return;
    const timer = setInterval(() => { api.skills().then(setSkills).catch(() => undefined); }, 5000);
    return () => clearInterval(timer);
  }, [running]);

  useEffect(() => {
    const el = surfaceRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) =>
      setSurfaceSize({ w: entry.contentRect.width, h: entry.contentRect.height }));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useEffect(() => () => execWS.current?.close(), []);

  /* ── derived ───────────────────────────────────────────────────── */

  // Graph-aware on purpose: the client draws the ports this graph names, not
  // only the four C2 spells out. See portMap.
  const portsByRef = useMemo(() => portMap(graph, skills), [graph, skills]);

  const heightOf = useCallback(
    (n: CanvasNode) => nodeHeight(portsByRef.get(n.ref) ?? { ingress: [], egress: [] }),
    [portsByRef],
  );

  const findings = useMemo(() => validate(graph, skills, mode), [graph, skills, mode]);
  const errors = findings.filter((f) => f.severity === "error").length;
  const warns = findings.filter((f) => f.severity === "warn").length;

  const toWorld = useCallback((clientX: number, clientY: number) => {
    const r = surfaceRef.current?.getBoundingClientRect();
    if (!r) return { x: 0, y: 0 };
    return { x: (clientX - r.left - view.x) / view.k, y: (clientY - r.top - view.y) / view.k };
  }, [view]);

  const anchor = useCallback(
    (ref: string, port: string, side: "in" | "out") =>
      portAnchor(graph.nodes.find((n) => n.ref === ref), portsByRef.get(ref), port, side),
    [graph.nodes, portsByRef],
  );

  /* ── viewport ──────────────────────────────────────────────────── */

  const fit = useCallback(() => {
    const r = surfaceRef.current?.getBoundingClientRect();
    if (!r || !graph.nodes.length) return;
    const xs = graph.nodes.map((n) => n.x);
    const ys = graph.nodes.map((n) => n.y);
    // Backward edges bow out by half the gap measured face to face, so the
    // reply hop every graph has would be clipped by a node-box-only fit.
    const bow = Math.max(60, (Math.max(...xs) - Math.min(...xs) + NODE_W) * 0.5);
    const left = Math.min(...xs) - bow;
    const top = Math.min(...ys);
    const w = Math.max(...xs) + NODE_W + bow - left;
    const h = Math.max(...graph.nodes.map((n) => n.y + heightOf(n))) - top;
    const k = Math.min(1.4, Math.min((r.width - 48) / w, (r.height - 48) / h));
    const scale = Number.isFinite(k) && k > 0.15 ? k : 1;
    setView({ k: scale, x: 24 - left * scale, y: 24 - top * scale });
  }, [graph.nodes, heightOf]);

  const zoomBy = (factor: number) => {
    const k = Math.min(2.2, Math.max(0.2, view.k * factor));
    const cx = surfaceSize.w / 2;
    const cy = surfaceSize.h / 2;
    setView({ k, x: cx - (cx - view.x) * (k / view.k), y: cy - (cy - view.y) * (k / view.k) });
  };

  /* ── pointer ───────────────────────────────────────────────────── */

  const onSurfacePointerDown = (e: React.PointerEvent) => {
    if (e.button !== 0 && e.button !== 1) return;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    // Shift drags a selection box; a plain drag pans. Middle always pans.
    if (editable && e.shiftKey && e.button === 0) {
      const w = toWorld(e.clientX, e.clientY);
      gesture.current = { kind: "box", startX: w.x, startY: w.y };
      setBox({ x: w.x, y: w.y, w: 0, h: 0 });
      return;
    }
    gesture.current = { kind: "pan", startX: e.clientX, startY: e.clientY, origin: view };
    setSel(EMPTY_SELECTION);
  };

  const onNodePointerDown = (e: React.PointerEvent, ref: string) => {
    if ((e.target as HTMLElement).classList.contains("gport__dot")) return;
    e.stopPropagation();
    const additive = e.shiftKey || e.metaKey || e.ctrlKey;
    // Dragging a node inside a multi-selection moves the whole selection.
    const next = isSelected(sel, "node", ref) && !additive ? sel : select(sel, "node", ref, additive);
    setSel(next);
    if (!editable) return;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    const w = toWorld(e.clientX, e.clientY);
    gesture.current = {
      kind: "move",
      refs: next.nodes.length ? next.nodes : [ref],
      lastX: w.x, lastY: w.y,
    };
    setHistory((h) => commit(h, graph));
  };

  const onPointerMove = (e: React.PointerEvent) => {
    const g = gesture.current;
    if (g?.kind === "pan") {
      setView({ ...g.origin, x: g.origin.x + (e.clientX - g.startX), y: g.origin.y + (e.clientY - g.startY) });
      return;
    }
    if (g?.kind === "move") {
      const w = toWorld(e.clientX, e.clientY);
      const dx = w.x - g.lastX;
      const dy = w.y - g.lastY;
      g.lastX = w.x;
      g.lastY = w.y;
      // Straight setGraph, not `edit`: the history entry was taken once on
      // pointerdown, so a drag is one undo step rather than one per frame.
      setGraph((prev) => moveBy(prev, g.refs, dx, dy));
      return;
    }
    if (g?.kind === "box") {
      const w = toWorld(e.clientX, e.clientY);
      setBox({
        x: Math.min(g.startX, w.x), y: Math.min(g.startY, w.y),
        w: Math.abs(w.x - g.startX), h: Math.abs(w.y - g.startY),
      });
      return;
    }
    if (link) {
      const w = toWorld(e.clientX, e.clientY);
      setLink({ ...link, x: w.x, y: w.y });
    }
  };

  const onPointerUp = () => {
    const g = gesture.current;
    if (g?.kind === "move" && graph.graphId) savePositions(graph.graphId, graph.nodes);
    if (g?.kind === "box" && box) {
      setSel(selectWithin(graph, skills, box, (n) => ({ w: NODE_W, h: heightOf(n) })));
    }
    gesture.current = null;
    setBox(null);
    setLink(null);
  };

  const onPortDown = (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => {
    if (!editable || side === "in") return;
    e.stopPropagation();
    const a = anchor(ref, port, "out");
    setLink({ ref, port, x: a?.x ?? 0, y: a?.y ?? 0 });
  };

  const onPortUp = (e: React.PointerEvent, ref: string, port: string, side: "in" | "out") => {
    if (!link || side !== "in") return;
    e.stopPropagation();
    const from = { ref: link.ref, port: link.port };
    setLink(null);
    edit((g) => connect(g, skills, from, { ref, port })?.graph ?? g);
  };

  /** Ingress ports whose schema matches the one being dragged. */
  const dropTargets = useMemo(() => {
    if (!link) return null;
    const src = portsByRef.get(link.ref)?.egress.find((p) => p.name === link.port);
    const out = new Set<string>();
    const base = (s: string) => s.split("@")[0];
    const major = (s: string) => s.split("@")[1]?.split(".")[0] ?? "";
    for (const n of graph.nodes) {
      for (const p of portsByRef.get(n.ref)?.ingress ?? []) {
        if (!src || (base(src.schema) === base(p.schema) && major(src.schema) === major(p.schema))) {
          out.add(`${n.ref}.${p.name}`);
        }
      }
    }
    return out;
  }, [link, graph.nodes, portsByRef]);

  /* ── commands ──────────────────────────────────────────────────── */

  const doUndo = useCallback(() => {
    const r = undo(history, graph);
    if (r) { setHistory(r.history); setGraph(r.graph); setSel(EMPTY_SELECTION); }
  }, [history, graph]);

  const doRedo = useCallback(() => {
    const r = redo(history, graph);
    if (r) { setHistory(r.history); setGraph(r.graph); setSel(EMPTY_SELECTION); }
  }, [history, graph]);

  const doCopy = useCallback(() => setClipboard(copy(graph, sel)), [graph, sel]);

  const doPaste = useCallback(() => {
    if (!clipboard) return;
    setHistory((h) => commit(h, graph));
    const out = paste(graph, clipboard);
    setGraph(out.graph);
    setSel(out.selection);
  }, [clipboard, graph]);

  const doDuplicate = useCallback(() => {
    const clip = copy(graph, sel);
    if (!clip) return;
    setHistory((h) => commit(h, graph));
    const out = paste(graph, clip);
    setGraph(out.graph);
    setSel(out.selection);
  }, [graph, sel]);

  const doDelete = useCallback(() => {
    if (!sel.nodes.length && !sel.edges.length) return;
    edit((g) => remove(g, sel));
    setSel(EMPTY_SELECTION);
  }, [sel, edit]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.key.toLowerCase() === "z" && !e.shiftKey) { e.preventDefault(); doUndo(); return; }
      if (mod && (e.key.toLowerCase() === "y" || (e.key.toLowerCase() === "z" && e.shiftKey))) {
        e.preventDefault(); doRedo(); return;
      }
      if (!editable) return;
      if (mod && e.key.toLowerCase() === "c") { doCopy(); return; }
      if (mod && e.key.toLowerCase() === "v") { doPaste(); return; }
      if (mod && e.key.toLowerCase() === "d") { e.preventDefault(); doDuplicate(); return; }
      if (mod && e.key.toLowerCase() === "a") {
        e.preventDefault();
        setSel({ nodes: graph.nodes.map((n) => n.ref), edges: graph.edges.map((x) => x.id) });
        return;
      }
      if (e.key === "Delete" || e.key === "Backspace") { e.preventDefault(); doDelete(); return; }
      if (e.key === "Escape") { setLink(null); setSel(EMPTY_SELECTION); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [doUndo, doRedo, doCopy, doPaste, doDuplicate, doDelete, editable, graph]);

  /* ── graph lifecycle ───────────────────────────────────────────── */

  const load = useCallback(async (id: string) => {
    try {
      const ir = await api.graph(id);
      setGraph(fromIR(ir));
      setHistory(EMPTY_HISTORY);
      setSel(EMPTY_SELECTION);
      setStatus(null);
      resetActivity();
      setItems([]);
      setPhase("idle");
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  }, [resetActivity]);

  useEffect(() => { fit(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [graph.graphId]);

  const register = async () => {
    if (errors) { setStatus({ kind: "err", text: t("studio.blocked", { count: errors }) }); return; }
    try {
      const res = await api.registerGraph(toIR(graph));
      savePositions(graph.graphId, graph.nodes);
      setStatus({ kind: "ok", text: t("studio.registered", { id: res.graph_id }) });
      live.refresh();
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  };

  /* ── run ───────────────────────────────────────────────────────── */

  const requestPlan = (text: string): Promise<Plan> =>
    new Promise((resolve, reject) => {
      const ws = new WebSocket(streamURL("plan"));
      const timer = setTimeout(() => { ws.close(); reject(new Error(t("studio.planTimeout"))); }, 120_000);
      ws.onerror = () => reject(new Error(t("studio.unreachable")));
      ws.onmessage = (ev) => {
        const env = JSON.parse(ev.data) as Envelope;
        const p = (env.payload ?? {}) as Record<string, unknown>;
        if (env.kind === "status" && p.state === "ready") ws.send(JSON.stringify({ text }));
        else if (env.kind === "status" && p.detail) push({ kind: "status", key: env.id, text: String(p.detail) });
        else if (env.kind === "data" && env.schema === "std/plan@1") {
          clearTimeout(timer); ws.close(); resolve(env.payload as Plan);
        } else if (env.kind === "error") {
          clearTimeout(timer); ws.close();
          reject(new Error(String(p.detail ?? t("studio.needPlanner"))));
        }
      };
    });

  const executeGraph = (graphId: string, inputs: Plan["inputs"]) => {
    setPhase("executing");
    let pending = inputs.length;
    const ws = new WebSocket(streamURL(graphId));
    execWS.current = ws;
    let seq = 0;

    ws.onmessage = (ev) => {
      const env = JSON.parse(ev.data) as Envelope;
      const p = (env.payload ?? {}) as Record<string, unknown>;
      observe(env); // cheap: map writes, no render — the frame loop publishes

      switch (env.kind) {
        case "status":
          if (p.state === "ready") {
            for (const input of inputs) {
              seq += 1;
              ws.send(JSON.stringify({
                v: "1", id: newId(), node: "client", port: input.port, seq,
                idem: `ui:studio:${input.port}:${seq}:${newId()}`,
                schema: input.schema, kind: "data", payload: input.payload,
              }));
            }
          } else if (p.detail) push({ kind: "status", key: env.id, text: String(p.detail) });
          break;
        case "confirm_request":
          push({ kind: "gate", key: env.id, requestId: env.id, question: String(p.question ?? "Approve?") });
          break;
        case "data":
          push({ kind: "result", key: env.id, json: JSON.stringify(env.payload, null, 2) });
          if (--pending <= 0) { setPhase("done"); ws.close(); }
          break;
        case "done":
          if (--pending <= 0) { setPhase("done"); ws.close(); }
          break;
        case "error":
          push({ kind: "error", key: env.id, text: String(p.detail ?? "error") });
          setPhase("failed");
          ws.close();
          break;
      }
    };
  };

  /** Ask the planner, draw what it decided, then run it. */
  const runGoal = async () => {
    const text = goal.trim();
    if (!text || running) return;
    setItems([]); setStatus(null); resetActivity(); setPhase("planning");
    try {
      const p = await requestPlan(text);
      setGraph(fromIR(p.graph));       // complete, because this is when it exists
      setHistory(EMPTY_HISTORY);
      setSel(EMPTY_SELECTION);
      await api.registerGraph(p.graph);
      executeGraph(p.graph.graph_id, p.inputs);
    } catch (e) {
      setPhase("failed");
      setStatus({ kind: "err", text: (e as Error).message });
    }
  };

  /** Run what is on the canvas, without asking the planner for anything. */
  const runCanvas = async () => {
    if (running || errors || !graph.graphId) return;
    setItems([]); setStatus(null); resetActivity(); setPhase("executing");
    try {
      await api.registerGraph(toIR(graph));
      const inputs = (portsByRef.get("client")?.egress ?? [])
        .filter((p) => graph.edges.some((e) => e.fromRef === "client" && e.fromPort === p.name))
        .map((p) => ({ port: p.name, schema: p.schema, payload: { text: "", final: true } }));
      executeGraph(graph.graphId, inputs);
    } catch (e) {
      setPhase("failed");
      setStatus({ kind: "err", text: (e as Error).message });
    }
  };

  const respondGate = (requestId: string, approve: boolean) =>
    execWS.current?.send(JSON.stringify({
      v: "1", id: newId(), cause_id: requestId, kind: "confirm_response", payload: { approve },
    }));

  /* ── inspector plumbing ────────────────────────────────────────── */

  const inspecting = sel.nodes.length === 1 || sel.edges.length === 1;
  const selectedNode = sel.nodes.length === 1 ? graph.nodes.find((n) => n.ref === sel.nodes[0]) ?? null : null;
  const selectedEdge = sel.edges.length === 1 ? graph.edges.find((e) => e.id === sel.edges[0]) ?? null : null;
  const selFindings = findings.filter(
    (f) => (f.nodeRef && sel.nodes.includes(f.nodeRef)) || (f.edgeId && sel.edges.includes(f.edgeId)),
  );

  const patchNode = (patch: Partial<CanvasNode>) => {
    if (!selectedNode) return;
    const oldRef = selectedNode.ref;
    edit((g) => {
      const renamed = patch.ref && patch.ref !== oldRef ? rename(g, oldRef, patch.ref) : g;
      const ref = patch.ref ?? oldRef;
      return { ...renamed, nodes: renamed.nodes.map((n) => (n.ref === ref ? { ...n, ...patch } : n)) };
    });
    if (patch.ref) setSel({ nodes: [patch.ref], edges: [] });
  };

  const patchEdge = (patch: Partial<CanvasEdge>) => {
    if (!selectedEdge) return;
    edit((g) => ({ ...g, edges: g.edges.map((e) => (e.id === selectedEdge.id ? { ...e, ...patch } : e)) }));
  };

  const filteredSkills = skills.filter((s) => {
    const q = paletteQuery.toLowerCase();
    return !q || s.name.toLowerCase().includes(q) || s.capability.toLowerCase().includes(q);
  });

  const centre = () => toWorld(
    (surfaceRef.current?.getBoundingClientRect().left ?? 0) + surfaceSize.w / 2,
    (surfaceRef.current?.getBoundingClientRect().top ?? 0) + surfaceSize.h / 2,
  );

  return (
    <section className="view view--flush studio">
      {/* run bar */}
      <div className="studio__bar">
        <input
          className="input studio__goal"
          placeholder={t("studio.placeholder")}
          value={goal}
          disabled={running}
          onChange={(e) => setGoal(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && runGoal()}
        />
        <button className="btn" onClick={runGoal} disabled={!goal.trim() || running}>
          {running ? t("studio.running") : t("studio.run")}
        </button>
        <span className={`studio__phase studio__phase--${phase}`}>{t(`studio.phase.${phase}`)}</span>
      </div>

      {/* build bar */}
      <div className="studio__tools">
        <input
          className="input studio__id"
          value={graph.graphId}
          placeholder={t("studio.graphId")}
          disabled={!editable}
          onChange={(e) => setGraph((g) => ({ ...g, graphId: e.target.value }))}
        />
        <select className="select studio__open" value="" onChange={(e) => e.target.value && load(e.target.value)}>
          <option value="">{t("studio.open")}</option>
          {live.graphs.map((id) => <option key={id} value={id}>{id}</option>)}
        </select>

        <span className="studio__sep" />
        <button className="iconbtn" onClick={doUndo} disabled={!history.past.length} title={t("studio.undo")}>↶</button>
        <button className="iconbtn" onClick={doRedo} disabled={!history.future.length} title={t("studio.redo")}>↷</button>
        <button className="iconbtn" onClick={doDuplicate} disabled={!editable || !sel.nodes.length} title={t("studio.duplicate")}>⧉</button>
        <button className="iconbtn" onClick={doDelete} disabled={!editable || (!sel.nodes.length && !sel.edges.length)} title={t("studio.delete")}>🗑</button>

        <span className="studio__sep" />
        <button className="iconbtn" onClick={() => zoomBy(1 / 1.2)} title={t("studio.zoomOut")}>−</button>
        <span className="studio__zoom">{Math.round(view.k * 100)}%</span>
        <button className="iconbtn" onClick={() => zoomBy(1.2)} title={t("studio.zoomIn")}>+</button>
        <button className="btn btn--ghost btn--sm" onClick={fit}>{t("studio.fit")}</button>
        <button
          className="btn btn--ghost btn--sm"
          disabled={!editable}
          onClick={() => edit((g) => ({ ...g, nodes: autoLayout(g.nodes, g.edges) }))}
        >
          {t("studio.tidy")}
        </button>
        <button
          className="btn btn--ghost btn--sm"
          onClick={() => { setIrDraft(JSON.stringify(toIR(graph), null, 2)); setShowIR((s) => !s); }}
        >
          {t("studio.ir")}
        </button>

        <span className="studio__spacer" />
        {(errors > 0 || warns > 0) && (
          <span className="canvas-bar__count">
            {errors > 0 && <span className="finding-dot finding-dot--error">{errors}</span>}
            {warns > 0 && <span className="finding-dot finding-dot--warn">{warns}</span>}
          </span>
        )}
        <button className="btn btn--ghost btn--sm" onClick={runCanvas} disabled={running || !!errors || !graph.graphId}>
          {t("studio.runCanvas")}
        </button>
        <button className="btn btn--sm" onClick={register} disabled={!editable || !!errors}>
          {t("studio.register")}
        </button>
      </div>

      {status && (
        <div className={status.kind === "ok" ? "canvas-status--ok" : "error-banner"}>{status.text}</div>
      )}

      <div className="studio__body">
        <aside className="palette">
          <input
            className="input palette__search"
            placeholder={t("studio.searchSkills")}
            value={paletteQuery}
            onChange={(e) => setPaletteQuery(e.target.value)}
          />
          <div className="palette__list">
            {filteredSkills.length === 0 && <div className="palette__empty">{t("studio.noSkills")}</div>}
            {filteredSkills.map((s) => (
              <button
                key={s.id}
                className="palette__item"
                disabled={!editable}
                title={s.description}
                onClick={() => {
                  const c = centre();
                  edit((g) => addSkill(g, s, { x: c.x - NODE_W / 2, y: c.y - 60 }).graph);
                }}
              >
                <span className={`palette__dot palette__dot--${s.type}`} />
                <span className="palette__name">{s.name}</span>
                <span className="palette__cap">{s.capability}</span>
              </button>
            ))}
          </div>
          <LiveRail
            activity={live.activity}
            openId={graph.graphId}
            following={!running}
            error={live.error}
            onToggleFollow={() => undefined}
            onOpen={load}
          />
        </aside>

        <div
          className={`canvas-surface ${running ? "canvas-surface--running" : ""}`}
          ref={surfaceRef}
          onPointerDown={onSurfacePointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onPointerCancel={onPointerUp}
          onWheel={(e) => {
            const r = surfaceRef.current?.getBoundingClientRect();
            if (!r) return;
            const mx = e.clientX - r.left;
            const my = e.clientY - r.top;
            const k = Math.min(2.2, Math.max(0.2, view.k * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
            setView({ k, x: mx - (mx - view.x) * (k / view.k), y: my - (my - view.y) * (k / view.k) });
          }}
        >
          <div className="canvas-world" style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }}>
            <svg className="canvas-edges" aria-hidden>
              {graph.edges.map((e) => {
                const a = anchor(e.fromRef, e.fromPort, "out");
                const b = anchor(e.toRef, e.toPort, "in");
                if (!a || !b) return null;
                const bad = findings.some((f) => f.edgeId === e.id && f.severity === "error");
                const waiting = activity.nodes[e.toRef] === "waiting";
                const cls = [
                  "cedge",
                  isSelected(sel, "edge", e.id) ? "cedge--sel" : "",
                  bad ? "cedge--bad" : "",
                  e.gate === "human-approval" ? "cedge--gated" : "",
                  e.speculative ? "cedge--spec" : "",
                  activity.nodes[e.fromRef] === "active" ? "cedge--hot" : "",
                  waiting ? "cedge--holding" : "",
                ].join(" ");
                return (
                  <g key={e.id}>
                    <path className={cls} d={curve(a, b)} />
                    <path
                      className="cedge__hit"
                      d={curve(a, b)}
                      onPointerDown={(ev) => {
                        ev.stopPropagation();
                        setSel(select(sel, "edge", e.id, ev.shiftKey));
                      }}
                    />
                    {e.gate === "human-approval" && (
                      <circle
                        className={`cedge__gate ${waiting ? "cedge__gate--waiting" : ""}`}
                        cx={(a.x + b.x) / 2} cy={(a.y + b.y) / 2} r={5}
                      />
                    )}
                  </g>
                );
              })}
              {link && (() => {
                const a = anchor(link.ref, link.port, "out");
                return a ? <path className="cedge cedge--draft" d={curve(a, { x: link.x, y: link.y })} /> : null;
              })()}
              {box && (
                <rect
                  className="selectbox"
                  x={box.x} y={box.y} width={box.w} height={box.h}
                />
              )}
            </svg>

            {graph.nodes.map((n) => (
              <div key={n.ref} className={`livenode livenode--${activity.nodes[n.ref] ?? "idle"}`}>
                <GraphNode
                  node={n}
                  ports={portsByRef.get(n.ref) ?? { ingress: [], egress: [] }}
                  selected={isSelected(sel, "node", n.ref)}
                  dropTargets={dropTargets}
                  candidates={candidateCount(n, skills)}
                  onPointerDown={onNodePointerDown}
                  onSelect={() => undefined}
                  onPortDown={onPortDown}
                  onPortUp={onPortUp}
                />
                {(activity.counts[n.ref] ?? 0) > 0 && (
                  <span className="livenode__count" style={{ left: n.x, top: n.y - 9 }}>
                    {activity.counts[n.ref]}
                  </span>
                )}
              </div>
            ))}
          </div>

          {graph.nodes.length <= 1 && graph.edges.length === 0 && (
            <div className="canvas-hint">{t("studio.hint")}</div>
          )}

          <div className="studio__map">
            <Minimap
              nodes={graph.nodes}
              heightOf={heightOf}
              view={view}
              surface={surfaceSize}
              onJump={(w) => setView((v) => ({ ...v, x: surfaceSize.w / 2 - w.x * v.k, y: surfaceSize.h / 2 - w.y * v.k }))}
            />
          </div>
        </div>

        <aside className={`studio__side ${side === "converse" ? "studio__side--wide" : ""}`}>
          <div className="studio__tabs" role="tablist">
            <button
              role="tab"
              aria-selected={side === "auto"}
              className={`studio__tab ${side === "auto" ? "studio__tab--on" : ""}`}
              onClick={() => setSide("auto")}
            >
              {inspecting ? t("studio.tabInspect") : t("studio.tabActivity")}
            </button>
            <button
              role="tab"
              aria-selected={side === "converse"}
              className={`studio__tab ${side === "converse" ? "studio__tab--on" : ""}`}
              onClick={() => setSide("converse")}
            >
              {t("studio.tabConverse")}
            </button>
          </div>

          {side === "converse" ? (
            <ConversePanel />
          ) : (
          <div className="studio__pane">
          {inspecting ? (
            <Inspector
              node={selectedNode}
              edge={selectedEdge}
              ports={selectedNode ? portsByRef.get(selectedNode.ref) ?? null : null}
              skills={skills}
              findings={selFindings}
              destIsMotor={!!selectedEdge && portsByRef.get(selectedEdge.toRef)?.type === "motor"}
              onNodeChange={patchNode}
              onEdgeChange={patchEdge}
              onDelete={doDelete}
            />
          ) : (
            <div className="studio__activity">
              {items.length === 0 && <div className="studio__hint">{t("studio.sideHint")}</div>}
              {items.map((item) => {
                switch (item.kind) {
                  case "status": return <div key={item.key} className="status-line">{item.text}</div>;
                  case "gate":
                    return (
                      <GateCard
                        key={item.key}
                        question={item.question}
                        onRespond={(approve) => respondGate(item.requestId, approve)}
                      />
                    );
                  case "result": return <pre key={item.key} className="studio__result">{item.json}</pre>;
                  case "error": return <div key={item.key} className="finding finding--error">{item.text}</div>;
                }
              })}
            </div>
          )}
          </div>
          )}
        </aside>
      </div>

      {showIR && (
        <div className="ir-drawer">
          <div className="ir-drawer__head">
            <span>{t("studio.irTitle")}</span>
            <button
              className="btn btn--ghost btn--sm"
              disabled={!editable}
              onClick={() => {
                try {
                  edit(() => fromIR(JSON.parse(irDraft) as GraphIR));
                  setShowIR(false);
                } catch (e) { setStatus({ kind: "err", text: (e as Error).message }); }
              }}
            >
              {t("studio.irApply")}
            </button>
            <button className="btn btn--ghost btn--sm" onClick={() => setShowIR(false)}>{t("common.close")}</button>
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
                if (f.edgeId) setSel({ nodes: [], edges: [f.edgeId] });
                else if (f.nodeRef) setSel({ nodes: [f.nodeRef], edges: [] });
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
