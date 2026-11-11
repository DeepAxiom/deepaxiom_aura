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
import { ContextMenu, type MenuItem } from "../components/canvas/ContextMenu";
import { QuickAdd } from "../components/canvas/QuickAdd";
import { StickyNote } from "../components/canvas/StickyNote";
import { Shortcuts, keyFor } from "../components/canvas/Shortcuts";
import { GraphHistory } from "../components/canvas/History";
import {
  contained, cycleGroupColor, frame, loadGroups, moveGroup, patchGroup,
  removeGroup, resizeGroup, saveGroups, type Group,
} from "../graph/groups";
import {
  addNote, cycleColor, loadNotes, moveNote, patchNote,
  removeNote, resizeNote, saveNotes, type Note,
} from "../graph/notes";
import { BACK_DROP, BACK_LANE, BACK_SWING, curve, edgeMid, isBackward, portAnchor } from "../graph/geometry";
import {
  type CanvasEdge,
  type CanvasGraph,
  type CanvasNode,
  NODE_W,
  autoLayout,
  candidateCount,
  dedupeSkills,
  irDigest,
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
  align,
  toggleDisabled,
  distribute,
  insertOn,
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
import { useRunActivity, useSessionTail } from "../hooks/useRunActivity";
import { navigate, replaceRoute } from "../app/store";
import {
  IconAlert, IconCode, IconCopy, IconFit, IconHistory, IconKeyboard, IconLock, IconMinus, IconPlay,
  IconPanel, IconPlus, IconRedo, IconTidy, IconTrace, IconTrash, IconUndo,
} from "../components/Icons";
import { fmtCompact } from "../lib/format";

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

export function StudioView({ active = true, graphArg }: { active?: boolean; graphArg?: string } = {}) {
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
  /** The catalog column can be folded away: a wide graph needs the width more. */
  const [showPalette, setShowPalette] = useState(() => localStorage.getItem("aura.studio.palette") !== "off");
  /**
   * Which pane the right column shows.
   *
   * "inspect" is implicit rather than sticky — selecting a node means you want
   * its fields, and making that a click as well would be a click nobody would
   * thank us for. The conversation, by contrast, is a place you go and stay,
   * so it is the one an author picks and keeps.
   */
  const [side, setSide] = useState<"auto" | "converse">("auto");

  /** Sticky notes for the open graph. Never part of the IR — see graph/notes. */
  const [notes, setNotes] = useState<Note[]>([]);
  /** An open right-click menu: where it is, and what it offers. */
  const [menu, setMenu] = useState<{ x: number; y: number; items: MenuItem[] } | null>(null);
  /**
   * An open quick-add. `onEdge` means the pick splices into that edge instead
   * of dropping a loose node, which is the same dialog answering two questions.
   */
  const [quick, setQuick] = useState<
    { vx: number; vy: number; world: { x: number; y: number }; onEdge?: string } | null
  >(null);
  const [showKeys, setShowKeys] = useState(false);
  /** The node whose ref is being retyped in place. */
  const [renaming, setRenaming] = useState<{ ref: string; draft: string } | null>(null);
  /** Frames for the open graph. Annotation, never IR — see graph/groups. */
  const [groups, setGroups] = useState<Group[]>([]);
  const [showHistory, setShowHistory] = useState(false);
  /** The digest of what is on the canvas, for "you are here" in the history. */
  const [digest, setDigest] = useState<string | null>(null);
  /**
   * Outputs supplied by the caller instead of produced by the skill.
   *
   * Kept per graph in this tab only, deliberately not persisted: a pin is a
   * thing you are doing right now, and one that survived a reload would let a
   * result come back tomorrow from a decision made today without anyone
   * remembering they made it.
   */
  const [pins, setPins] = useState<Record<string, { port: string; payload: unknown }>>({});
  const [irDraft, setIrDraft] = useState("");
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null);

  const [goal, setGoal] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [items, setItems] = useState<Item[]>([]);
  const execWS = useRef<WebSocket | null>(null);
  /** The session this view started itself (Run), once the kernel names it. */
  const [ownSession, setOwnSession] = useState<string | null>(null);
  /** The session the Run panel is holding, when it is talking to a graph. */
  const [panelSession, setPanelSession] = useState<{ id: string; graph: string } | null>(null);

  const running = phase === "planning" || phase === "executing";
  const editable = !running;
  const { activity, observe, reset: resetActivity } = useRunActivity(active);
  const live = useLiveGraphs(active);

  /**
   * Which session the canvas is showing. In order: one this view started, the
   * one the Run panel is talking to, else the newest live session of the open
   * graph — started by anyone: `aura chat`, a webhook, another tab.
   */
  const watched = useMemo(
    () => (graph.graphId ? live.liveSessions.find((x) => x.graph_id === graph.graphId)?.session_id ?? null : null),
    [live.liveSessions, graph.graphId],
  );
  const tailId = ownSession ?? (panelSession && panelSession.graph === graph.graphId ? panelSession.id : null) ?? watched;
  // Keep showing the last session after it ends, until another one starts:
  // the final state of a run is the thing you came to look at.
  const [shownSession, setShownSession] = useState<string | null>(null);
  useEffect(() => {
    if (!tailId || tailId === shownSession) return;
    resetActivity();
    setShownSession(tailId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tailId]);
  const tail = useSessionTail(active ? shownSession : null, observe);

  /* ── gestures ──────────────────────────────────────────────────── */

  const gesture = useRef<
    | { kind: "pan"; startX: number; startY: number; origin: Viewport }
    | { kind: "move"; refs: string[]; lastX: number; lastY: number }
    | { kind: "box"; startX: number; startY: number }
    | { kind: "note-move"; id: string; lastX: number; lastY: number }
    | { kind: "note-resize"; id: string; lastX: number; lastY: number }
    // A frame drags what it holds: `refs` is resolved once on pointerdown, so
    // a node crossing the boundary mid-drag does not join or leave halfway.
    | { kind: "group-move"; id: string; refs: string[]; lastX: number; lastY: number }
    | { kind: "group-resize"; id: string; lastX: number; lastY: number }
    | null
  >(null);
  const [box, setBox] = useState<{ x: number; y: number; w: number; h: number } | null>(null);
  const [link, setLink] = useState<{ ref: string; port: string; x: number; y: number } | null>(null);

  // Notes belong to a graph id, so opening another graph swaps them like the
  // canvas swaps nodes. Loaded rather than merged: a note from the last graph
  // showing up on this one would be a ghost nobody could explain.
  useEffect(() => {
    setNotes(loadNotes(graph.graphId));
    setGroups(loadGroups(graph.graphId));
    // Pins name nodes in a graph; carrying them to a different one would pin
    // refs that may mean something else entirely.
    setPins({});
  }, [graph.graphId]);

  // Recomputed on every edit so the history's "you are here" tracks the canvas
  // rather than the last thing that was registered.
  useEffect(() => {
    let alive = true;
    void irDigest(graph).then((d) => { if (alive) setDigest(d); });
    return () => { alive = false; };
  }, [graph]);

  /**
   * Every note change goes through here so the store is written exactly once
   * per change. Notes have no undo: they are not part of the graph, and an
   * undo stack that sometimes restores prose and sometimes topology is one
   * nobody can predict.
   */
  const editGroups = useCallback((next: (g: Group[]) => Group[]) => {
    setGroups((prev) => {
      const out = next(prev);
      saveGroups(graph.graphId, out);
      return out;
    });
  }, [graph.graphId]);

  const editNotes = useCallback((next: (n: Note[]) => Note[]) => {
    setNotes((prev) => {
      const out = next(prev);
      saveNotes(graph.graphId, out);
      return out;
    });
  }, [graph.graphId]);

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

  /** A node's drawn box. Declared here because framing, aligning and
   *  distributing all need it and all live further down. */
  const sizeOfNode = useCallback(
    (n: CanvasNode) => ({ w: NODE_W, h: heightOf(n) }),
    [heightOf],
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

  /** Each right-to-left edge gets its own lane under the graph, in edge order. */
  const lanes = useMemo(() => {
    const out = new Map<string, number>();
    for (const e of graph.edges) {
      const a = anchor(e.fromRef, e.fromPort, "out");
      const b = anchor(e.toRef, e.toPort, "in");
      if (a && b && isBackward(a, b)) out.set(e.id, out.size);
    }
    return out;
  }, [graph.edges, anchor]);

  /* ── viewport ──────────────────────────────────────────────────── */

  /** Re-lay the graph. One definition: the button, the menu and T all call it. */
  const tidy = useCallback(() => {
    if (!editable) return;
    edit((g) => ({ ...g, nodes: autoLayout(g.nodes, g.edges) }));
  }, [editable, edit]);

  const fit = useCallback(() => {
    const r = surfaceRef.current?.getBoundingClientRect();
    if (!r || !graph.nodes.length) return;
    const xs = graph.nodes.map((n) => n.x);
    const ys = graph.nodes.map((n) => n.y);
    // An edge that runs right-to-left (the reply hop most graphs have) swings
    // out past the node boxes and runs below them, so a node-box-only fit
    // would clip it. Measured from the edges actually present: padding every
    // graph for its worst possible edge left a simple chain drawn at a third
    // of the size it could be.
    let bow = 28;
    let drop = 0;
    for (const e of graph.edges) {
      const from = anchor(e.fromRef, e.fromPort, "out");
      const to = anchor(e.toRef, e.toPort, "in");
      if (from && to && isBackward(from, to)) { bow = BACK_SWING + lanes.size * 8; drop = BACK_DROP + lanes.size * BACK_LANE; }
    }
    const left = Math.min(...xs) - bow;
    const top = Math.min(...ys) - 28;
    const w = Math.max(...xs) + NODE_W + bow - left;
    // Room under the lowest node for the text it streams while running.
    const h = Math.max(...graph.nodes.map((n) => n.y + heightOf(n))) + Math.max(104, drop) - top;
    const k = Math.min(1.25, Math.min((r.width - 48) / w, (r.height - 48) / h));
    const scale = Number.isFinite(k) && k > 0.15 ? k : 1;
    setView({
      k: scale,
      x: (r.width - w * scale) / 2 - left * scale,
      y: Math.max(24, (r.height - h * scale) / 2) - top * scale,
    });
  }, [graph.nodes, graph.edges, heightOf, anchor, lanes]);

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
    if (g?.kind === "group-move" || g?.kind === "group-resize") {
      const w = toWorld(e.clientX, e.clientY);
      const dx = w.x - g.lastX;
      const dy = w.y - g.lastY;
      g.lastX = w.x;
      g.lastY = w.y;
      if (g.kind === "group-move") {
        setGroups((prev) => moveGroup(prev, g.id, dx, dy));
        // The frame carries its contents. Anything else makes a frame a
        // decoration you have to keep re-drawing around nodes you moved.
        if (g.refs.length) setGraph((prev) => moveBy(prev, g.refs, dx, dy));
      } else {
        setGroups((prev) => resizeGroup(prev, g.id, dx, dy));
      }
      return;
    }
    if (g?.kind === "note-move" || g?.kind === "note-resize") {
      const w = toWorld(e.clientX, e.clientY);
      const dx = w.x - g.lastX;
      const dy = w.y - g.lastY;
      g.lastX = w.x;
      g.lastY = w.y;
      // setNotes, not editNotes: a drag writes to the store once on pointerup
      // rather than on every frame.
      setNotes((prev) => (g.kind === "note-move"
        ? moveNote(prev, g.id, dx, dy)
        : resizeNote(prev, g.id, dx, dy)));
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
    if (g?.kind === "note-move" || g?.kind === "note-resize") saveNotes(graph.graphId, notes);
    if (g?.kind === "group-move" || g?.kind === "group-resize") {
      saveGroups(graph.graphId, groups);
      if (g.kind === "group-move" && graph.graphId) savePositions(graph.graphId, graph.nodes);
    }
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

  /* ── the system clipboard, in IR ─────────────────────────────────
   *
   * A graph is already text you can paste out of a chat message — C2 IR is the
   * format, and the IR drawer round-trips it — so this is that capability with
   * one fewer step between the clipboard and the canvas.
   *
   * Deliberately the whole graph rather than the selection: a fragment of IR
   * is not IR (it has no graph_id and may name nodes it does not carry), and
   * handing someone a document the kernel would reject is worse than handing
   * them a big one. In-canvas copy/paste of a selection is Ctrl+C, and stays
   * that way.
   */

  const doCopyIR = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(JSON.stringify(toIR(graph), null, 2));
      setStatus({ kind: "ok", text: t("studio.copiedIR") });
    } catch {
      // Clipboard writes need a secure context and, in some browsers, a user
      // gesture. Falling back to the drawer is better than a dead shortcut.
      setIrDraft(JSON.stringify(toIR(graph), null, 2));
      setShowIR(true);
    }
  }, [graph, t]);

  const doPasteIR = useCallback(async () => {
    if (!editable) return;
    try {
      const text = await navigator.clipboard.readText();
      const parsed = JSON.parse(text) as GraphIR;
      if (parsed.ir !== "1" || !Array.isArray(parsed.nodes)) {
        throw new Error(t("studio.notIR"));
      }
      edit(() => fromIR(parsed));
      setSel(EMPTY_SELECTION);
      setStatus({ kind: "ok", text: t("studio.pastedIR", { id: parsed.graph_id }) });
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  }, [editable, edit, t]);

  /* ── placing things ──────────────────────────────────────────────── */

  /** Drop a skill at a world point, or splice it into an edge. */
  const placeSkill = useCallback((skill: SkillManifest, at: { x: number; y: number }, onEdge?: string) => {
    if (onEdge) {
      const out = insertOn(graph, skills, onEdge, skill);
      if (!out) {
        setStatus({ kind: "err", text: t("studio.cannotSplice", { name: skill.name }) });
        return;
      }
      setHistory((h) => commit(h, graph));
      setGraph(out.graph);
      setSel({ nodes: [out.ref], edges: [] });
      return;
    }
    edit((g) => addSkill(g, skill, { x: at.x - NODE_W / 2, y: at.y - 60 }).graph);
  }, [graph, skills, edit, t]);

  const doDisable = useCallback(() => {
    if (!sel.nodes.length) return;
    edit((g) => toggleDisabled(g, sel.nodes));
  }, [edit, sel.nodes]);

  const doGroup = useCallback(() => {
    const picked = graph.nodes.filter((n) => sel.nodes.includes(n.ref));
    if (!picked.length) return;
    const made = frame(groups, picked, sizeOfNode, "");
    if (made) editGroups(() => made.groups);
  }, [graph.nodes, sel.nodes, groups, editGroups]);

  /** Pin a node's first egress to whatever it last produced in this session. */
  const doPin = useCallback((ref: string) => {
    const ports = portsByRef.get(ref);
    const port = ports?.egress[0]?.name;
    if (!port) return;
    const seen = activity.lastPayload[ref];
    setPins((prev) => ({ ...prev, [ref]: { port, payload: seen ?? {} } }));
    setStatus({ kind: "ok", text: t(seen ? "studio.pinned" : "studio.pinnedEmpty", { ref }) });
  }, [portsByRef, activity.lastPayload, t]);

  const doUnpin = useCallback((ref: string) => {
    setPins((prev) => {
      const next = { ...prev };
      delete next[ref];
      return next;
    });
  }, []);

  const doAddNote = useCallback((at: { x: number; y: number }) => {
    const out = addNote(notes, at);
    editNotes(() => out.notes);
  }, [notes, editNotes]);

  /* ── lining things up ────────────────────────────────────────────── */

  const sizeOf = sizeOfNode;

  const doAlign = useCallback((edge: Parameters<typeof align>[2]) => {
    edit((g) => align(g, sel.nodes, edge, sizeOf));
  }, [edit, sel.nodes, sizeOf]);

  const doDistribute = useCallback((axis: "x" | "y") => {
    edit((g) => distribute(g, sel.nodes, axis, sizeOf));
  }, [edit, sel.nodes, sizeOf]);

  const doDelete = useCallback(() => {
    if (!sel.nodes.length && !sel.edges.length) return;
    edit((g) => remove(g, sel));
    setSel(EMPTY_SELECTION);
  }, [sel, edit]);

  /* ── right-click menus ──────────────────────────────────────────── */

  /**
   * Built per target rather than one menu that greys out what does not apply.
   * A menu whose items are mostly disabled teaches you to stop opening it.
   */
  const openMenu = useCallback((e: React.MouseEvent, items: MenuItem[]) => {
    e.preventDefault();
    e.stopPropagation();
    if (items.length) setMenu({ x: e.clientX, y: e.clientY, items });
  }, []);

  const surfaceMenu = (e: React.MouseEvent): MenuItem[] => {
    const world = toWorld(e.clientX, e.clientY);
    return [
      { label: t("studio.menu.addSkill"), hint: keyFor("quickAdd"),
        onSelect: () => setQuick({ vx: e.clientX, vy: e.clientY, world }) },
      { label: t("studio.menu.addNote"), hint: keyFor("note"),
        onSelect: () => doAddNote(world) },
      { kind: "separator" },
      { label: t("studio.menu.pasteHere"), hint: keyFor("paste"),
        disabled: !clipboard, onSelect: doPaste },
      { label: t("studio.menu.pasteIR"), hint: keyFor("pasteIR"), onSelect: () => void doPasteIR() },
      { kind: "separator" },
      { label: t("studio.menu.selectAll"), hint: keyFor("selectAll"),
        onSelect: () => setSel({ nodes: graph.nodes.map((n) => n.ref), edges: graph.edges.map((x) => x.id) }) },
      { label: t("studio.menu.copyIR"), hint: keyFor("copyIR"), onSelect: () => void doCopyIR() },
      { label: t("studio.tidy"), hint: keyFor("tidy"), onSelect: tidy },
      { label: t("studio.fit"), hint: keyFor("fit"), onSelect: fit },
      { kind: "separator" },
      { label: t("studio.menu.history"), hint: keyFor("history"),
        disabled: !graph.graphId, onSelect: () => setShowHistory(true) },
    ];
  };

  const nodeMenu = (ref: string): MenuItem[] => {
    const node = graph.nodes.find((n) => n.ref === ref);
    const many = sel.nodes.length > 1 && sel.nodes.includes(ref);
    const items: MenuItem[] = [
      { label: t("studio.menu.rename"), hint: keyFor("rename"), disabled: !!node?.isClient,
        onSelect: () => node && setRenaming({ ref, draft: ref }) },
      { label: t("studio.menu.duplicate"), hint: keyFor("duplicate"),
        disabled: !!node?.isClient, onSelect: doDuplicate },
      { label: t("studio.menu.copy"), hint: keyFor("copy"), onSelect: doCopy },
      { kind: "separator" },
      { label: node?.disabled ? t("studio.menu.enable") : t("studio.menu.disable"),
        hint: keyFor("disable"), disabled: !!node?.isClient, onSelect: doDisable },
      { label: pins[ref] ? t("studio.menu.unpin") : t("studio.menu.pin"),
        disabled: !!node?.isClient,
        onSelect: () => (pins[ref] ? doUnpin(ref) : doPin(ref)) },
      { label: t("studio.menu.frame"), onSelect: doGroup },
    ];
    if (many) {
      items.push(
        { kind: "separator" },
        { label: t("studio.menu.alignLeft"), onSelect: () => doAlign("left") },
        { label: t("studio.menu.alignCx"), onSelect: () => doAlign("cx") },
        { label: t("studio.menu.alignTop"), onSelect: () => doAlign("top") },
        { label: t("studio.menu.alignCy"), onSelect: () => doAlign("cy") },
        // Three is where distribution starts meaning anything: two nodes
        // already have exactly one gap.
        { label: t("studio.menu.spreadX"), disabled: sel.nodes.length < 3,
          onSelect: () => doDistribute("x") },
        { label: t("studio.menu.spreadY"), disabled: sel.nodes.length < 3,
          onSelect: () => doDistribute("y") },
      );
    }
    items.push(
      { kind: "separator" },
      { label: t("studio.menu.delete"), hint: keyFor("delete"), danger: true,
        disabled: !!node?.isClient, onSelect: doDelete },
    );
    return items;
  };

  const edgeMenu = (id: string, e: React.MouseEvent): MenuItem[] => {
    const world = toWorld(e.clientX, e.clientY);
    return [
      // The gesture that makes an existing graph editable rather than
      // rebuildable: put something in the middle of a connection.
      { label: t("studio.menu.insertHere"),
        onSelect: () => setQuick({ vx: e.clientX, vy: e.clientY, world, onEdge: id }) },
      { label: t("studio.menu.inspect"), onSelect: () => { setSel({ nodes: [], edges: [id] }); setSide("auto"); } },
      { kind: "separator" },
      { label: t("studio.menu.delete"), hint: keyFor("delete"), danger: true,
        onSelect: () => { edit((g) => remove(g, { nodes: [], edges: [id] })); setSel(EMPTY_SELECTION); } },
    ];
  };

  const groupMenu = (id: string): MenuItem[] => [
    { label: t("studio.menu.frameColor"), onSelect: () => editGroups((g) => cycleGroupColor(g, id)) },
    { kind: "separator" },
    // Removing the frame is not removing what it held: it is annotation, and
    // nothing in this menu can reach the graph.
    { label: t("studio.menu.unframe"), danger: true,
      onSelect: () => editGroups((g) => removeGroup(g, id)) },
  ];

  const noteMenu = (id: string): MenuItem[] => [
    { label: t("studio.menu.noteColor"), onSelect: () => editNotes((n) => cycleColor(n, id)) },
    { kind: "separator" },
    { label: t("studio.menu.delete"), hint: keyFor("delete"), danger: true,
      onSelect: () => editNotes((n) => removeNote(n, id)) },
  ];

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // Mounted but not on screen: the keys belong to whichever view is.
      if (!active) return;
      const tag = (e.target as HTMLElement)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      // A dialog or the command palette is open above the canvas.
      if (document.querySelector(".scrim")) return;
      const mod = e.metaKey || e.ctrlKey;
      if (mod && e.key.toLowerCase() === "k") return;
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
      if (mod && e.shiftKey && e.key.toLowerCase() === "c") { e.preventDefault(); void doCopyIR(); return; }
      if (mod && e.shiftKey && e.key.toLowerCase() === "v") { e.preventDefault(); void doPasteIR(); return; }
      if (e.key === "Delete" || e.key === "Backspace") { e.preventDefault(); doDelete(); return; }
      if (e.key === "Escape") {
        // Innermost first: dismiss whatever is floating before touching the
        // selection, so one Escape never closes a menu *and* deselects.
        if (menu) { setMenu(null); return; }
        if (quick) { setQuick(null); return; }
        if (renaming) { setRenaming(null); return; }
        setLink(null); setSel(EMPTY_SELECTION);
        return;
      }
      // Unmodified letters, so they cannot collide with a browser shortcut.
      if (mod || e.altKey) return;
      if (e.key === "?") { e.preventDefault(); setShowKeys(true); return; }
      if (e.key === "/") { e.preventDefault(); document.querySelector<HTMLInputElement>(".palette__search")?.focus(); return; }
      if (e.key.toLowerCase() === "f") { e.preventDefault(); fit(); return; }
      if (e.key.toLowerCase() === "t") { e.preventDefault(); tidy(); return; }
      if (e.key.toLowerCase() === "n") { e.preventDefault(); doAddNote(centre()); return; }
      if (e.key.toLowerCase() === "d") { e.preventDefault(); doDisable(); return; }
      if (e.key.toLowerCase() === "g") { e.preventDefault(); doGroup(); return; }
      if (e.key.toLowerCase() === "h" && graph.graphId) { e.preventDefault(); setShowHistory(true); return; }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [doUndo, doRedo, doCopy, doPaste, doDuplicate, doDelete, doCopyIR, doPasteIR,
      doAddNote, doDisable, doGroup, editable, graph, menu, quick, renaming, active]);

  /* ── graph lifecycle ───────────────────────────────────────────── */

  const load = useCallback(async (id: string) => {
    try {
      const ir = await api.graph(id);
      setGraph(fromIR(ir));
      setHistory(EMPTY_HISTORY);
      setSel(EMPTY_SELECTION);
      setStatus(null);
      resetActivity();
      setOwnSession(null);
      setShownSession(null);
      setItems([]);
      setPhase("idle");
      if (active) replaceRoute("studio", id);
    } catch (e) {
      setStatus({ kind: "err", text: (e as Error).message });
    }
  }, [resetActivity, active]);

  // A deep link (#/studio/<graph>) or the command palette asks for a graph.
  useEffect(() => {
    if (graphArg && graphArg !== graph.graphId) void load(graphArg);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graphArg]);

  useEffect(() => {
    const onAction = (e: Event) => {
      const what = (e as CustomEvent).detail;
      if (what === "keys") setShowKeys(true);
      if (what === "new" && !running) {
        setGraph(EMPTY_GRAPH);
        setHistory(EMPTY_HISTORY);
        setSel(EMPTY_SELECTION);
        setOwnSession(null);
        setShownSession(null);
        resetActivity();
        replaceRoute("studio");
      }
    };
    window.addEventListener("aura:studio", onAction);
    return () => window.removeEventListener("aura:studio", onAction);
  }, [running, resetActivity]);

  // The side pane changes width with its tab; fit once it has settled, so the
  // graph is not left half under the pane it just made room for.
  useEffect(() => {
    const timer = setTimeout(fit, 240);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [side, showPalette]);

  // Hidden views have no size; fit again when this one comes back.
  useEffect(() => { if (active) fit(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [active]);

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

      switch (env.kind) {
        case "status":
          if (p.state === "ready") {
            // The canvas follows the session through its event log, not this
            // socket: the socket only carries what reaches the client.
            if (typeof p.session === "string") setOwnSession(p.session);
            // Pins first, and only before any data: the kernel refuses a pin
            // frame once a session has started producing, because one session
            // id describing two different graphs is a log nobody can read.
            if (Object.keys(pins).length) {
              ws.send(JSON.stringify({
                v: "1", id: newId(), node: "client",
                kind: "config_update", schema: "aura/pins@1",
                payload: { pins },
              }));
            }
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
    setItems([]); setStatus(null); resetActivity(); setOwnSession(null); setPhase("planning");
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
    setItems([]); setStatus(null); resetActivity(); setOwnSession(null); setPhase("executing");
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

  // One entry per skill, not per connection: see dedupeSkills. Everything that
  // offers skills to a person reads this, so the palette and the quick-add can
  // never show different catalogues.
  const catalog = useMemo(() => dedupeSkills(skills), [skills]);

  const filteredSkills = useMemo(() => {
    const q = paletteQuery.toLowerCase();
    return catalog.filter(
      (s) => !q || s.name.toLowerCase().includes(q) || s.capability.toLowerCase().includes(q),
    );
  }, [catalog, paletteQuery]);

  /**
   * The open graph, when a typed message can reach it: registered on the node
   * and wired from `client.text_out`. That is what the Run panel talks to.
   */
  const talkable =
    graph.graphId && live.graphs.includes(graph.graphId) &&
    graph.edges.some((e) => e.fromRef === "client" && e.fromPort === "text_out")
      ? graph.graphId
      : null;

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
          aria-label={t("studio.placeholder")}
          value={goal}
          disabled={running}
          onChange={(e) => setGoal(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && runGoal()}
        />
        <button className="btn btn--primary" onClick={runGoal} disabled={!goal.trim() || running}>
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
          aria-label={t("studio.graphId")}
          disabled={!editable}
          onChange={(e) => setGraph((g) => ({ ...g, graphId: e.target.value }))}
        />
        <select className="select studio__open" value="" aria-label={t("studio.open")} onChange={(e) => e.target.value && load(e.target.value)}>
          <option value="">{t("studio.open")}</option>
          {live.graphs.map((id) => <option key={id} value={id}>{id}</option>)}
        </select>

        <span className="studio__sep" />
        <button className="iconbtn" onClick={doUndo} disabled={!history.past.length} title={t("studio.undo")} aria-label={t("studio.undo")}><IconUndo size={15} /></button>
        <button className="iconbtn" onClick={doRedo} disabled={!history.future.length} title={t("studio.redo")} aria-label={t("studio.redo")}><IconRedo size={15} /></button>
        <button className="iconbtn" onClick={doDuplicate} disabled={!editable || !sel.nodes.length} title={t("studio.duplicate")} aria-label={t("studio.duplicate")}><IconCopy size={15} /></button>
        <button className="iconbtn" onClick={doDelete} disabled={!editable || (!sel.nodes.length && !sel.edges.length)} title={t("studio.delete")} aria-label={t("studio.delete")}><IconTrash size={15} /></button>

        <span className="studio__sep" />
        <button className="iconbtn" onClick={() => zoomBy(1 / 1.2)} title={t("studio.zoomOut")} aria-label={t("studio.zoomOut")}><IconMinus size={15} /></button>
        <span className="studio__zoom">{Math.round(view.k * 100)}%</span>
        <button className="iconbtn" onClick={() => zoomBy(1.2)} title={t("studio.zoomIn")} aria-label={t("studio.zoomIn")}><IconPlus size={15} /></button>
        <button className="iconbtn" onClick={fit} title={`${t("studio.fit")} (F)`} aria-label={t("studio.fit")}><IconFit size={15} /></button>
        <button className="iconbtn" disabled={!editable} onClick={tidy} title={`${t("studio.tidy")} (T)`} aria-label={t("studio.tidy")}><IconTidy size={15} /></button>

        <span className="studio__sep" />
        <button
          className="iconbtn"
          title={t("studio.irTitle")}
          aria-label={t("studio.ir")}
          aria-pressed={showIR}
          onClick={() => { setIrDraft(JSON.stringify(toIR(graph), null, 2)); setShowIR((x) => !x); }}
        >
          <IconCode size={15} />
        </button>
        <button className="iconbtn" disabled={!graph.graphId} onClick={() => setShowHistory(true)} title={`${t("studio.keys.history")} (H)`} aria-label={t("studio.keys.history")}><IconHistory size={15} /></button>
        <button
          className="iconbtn"
          aria-pressed={showPalette}
          title={t("studio.togglePalette")}
          aria-label={t("studio.togglePalette")}
          onClick={() => setShowPalette((v) => {
            localStorage.setItem("aura.studio.palette", v ? "off" : "on");
            return !v;
          })}
        >
          <IconPanel size={15} />
        </button>
        <button className="iconbtn" onClick={() => setShowKeys(true)} title={`${t("studio.shortcuts")} (?)`} aria-label={t("studio.shortcuts")}><IconKeyboard size={15} /></button>

        <span className="studio__spacer" />
        {(errors > 0 || warns > 0) && (
          <span className="canvas-bar__count">
            {errors > 0 && <span className="finding-dot finding-dot--error">{errors}</span>}
            {warns > 0 && <span className="finding-dot finding-dot--warn">{warns}</span>}
          </span>
        )}
        {shownSession && (
          <button
            className={`watch ${tailId ? "watch--live" : ""}`}
            onClick={() => navigate("sessions", shownSession)}
            title={t("studio.watch.help")}
          >
            <span className="dot dot--live" aria-hidden />
            <span className="watch__id">{shownSession}</span>
            <span className="watch__n">{t("studio.watch.events", { count: tail.events })}</span>
            <IconTrace size={13} />
          </button>
        )}
        <button className="btn btn--sm" onClick={runCanvas} disabled={running || !!errors || !graph.graphId} title={t("studio.runCanvasHelp")}>
          <IconPlay size={12} />{t("studio.runCanvas")}
        </button>
        <button className="btn btn--primary btn--sm" onClick={register} disabled={!editable || !!errors}>
          {t("studio.register")}
        </button>
      </div>

      {status && (
        <div className={`studio__status studio__status--${status.kind}`} role={status.kind === "err" ? "alert" : "status"}>
          <span>{status.text}</span>
          <button className="btn btn--ghost btn--sm" onClick={() => setStatus(null)}>{t("common.close")}</button>
        </div>
      )}

      <div className="studio__body">
        <aside className="palette" hidden={!showPalette}>
          <input
            className="input palette__search"
            placeholder={t("studio.searchSkills")}
            aria-label={t("studio.searchSkills")}
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
                <span className="palette__name">
                  {s.name}
                  {s.instances > 1 && (
                    <span className="palette__instances" title={t("studio.replicasHelp")}>
                      ×{s.instances}
                    </span>
                  )}
                </span>
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
          onContextMenu={(e) => editable && openMenu(e, surfaceMenu(e))}
          onDoubleClick={(e) => {
            if (!editable) return;
            setQuick({ vx: e.clientX, vy: e.clientY, world: toWorld(e.clientX, e.clientY) });
          }}
          onWheel={(e) => {
            const r = surfaceRef.current?.getBoundingClientRect();
            if (!r) return;
            const mx = e.clientX - r.left;
            const my = e.clientY - r.top;
            const k = Math.min(2.2, Math.max(0.2, view.k * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
            setView({ k, x: mx - (mx - view.x) * (k / view.k), y: my - (my - view.y) * (k / view.k) });
          }}
        >
          <div className={`canvas-world ${view.k < 0.42 ? "canvas-world--far" : ""}`} style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.k})` }}>
            {/* Frames sit under the notes, which sit under the graph. Both are
                annotation; neither may cover what it annotates. */}
            {groups.map((gr) => (
              <div
                key={gr.id}
                className={`gframe gframe--${gr.color}`}
                style={{ left: gr.x, top: gr.y, width: gr.w, height: gr.h }}
                onContextMenu={(e) => editable && openMenu(e, groupMenu(gr.id))}
                onPointerDown={(e) => {
                  if (!editable) return;
                  e.stopPropagation();
                  const w = toWorld(e.clientX, e.clientY);
                  gesture.current = {
                    kind: "group-move", id: gr.id,
                    // Resolved once, here: a node crossing the boundary
                    // mid-drag must not join or leave halfway through.
                    refs: contained(gr, graph.nodes, sizeOfNode),
                    lastX: w.x, lastY: w.y,
                  };
                  surfaceRef.current?.setPointerCapture(e.pointerId);
                }}
              >
                <input
                  className="gframe__label"
                  value={gr.label}
                  placeholder={t("studio.framePlaceholder")}
                  disabled={!editable}
                  onPointerDown={(e) => e.stopPropagation()}
                  onChange={(e) => editGroups((all) => patchGroup(all, gr.id, { label: e.target.value }))}
                />
                {editable && (
                  <div
                    className="gframe__resize"
                    onPointerDown={(e) => {
                      e.stopPropagation();
                      const w = toWorld(e.clientX, e.clientY);
                      gesture.current = { kind: "group-resize", id: gr.id, lastX: w.x, lastY: w.y };
                      surfaceRef.current?.setPointerCapture(e.pointerId);
                    }}
                  />
                )}
              </div>
            ))}

            {/* Behind everything: a note labels a region, and a label that can
                cover what it labels is one you have to move to work. */}
            {notes.map((n) => (
              <StickyNote
                key={n.id}
                note={n}
                editable={editable}
                selected={false}
                placeholder={t("studio.notePlaceholder")}
                onSelect={() => setSel(EMPTY_SELECTION)}
                onText={(text) => editNotes((all) => patchNote(all, n.id, { text }))}
                onMenu={(e) => openMenu(e, noteMenu(n.id))}
                onMoveStart={(e) => {
                  const w = toWorld(e.clientX, e.clientY);
                  gesture.current = { kind: "note-move", id: n.id, lastX: w.x, lastY: w.y };
                  surfaceRef.current?.setPointerCapture(e.pointerId);
                }}
                onResizeStart={(e) => {
                  const w = toWorld(e.clientX, e.clientY);
                  gesture.current = { kind: "note-resize", id: n.id, lastX: w.x, lastY: w.y };
                  surfaceRef.current?.setPointerCapture(e.pointerId);
                }}
              />
            ))}

            <svg className="canvas-edges" aria-hidden>
              {graph.edges.map((e) => {
                const a = anchor(e.fromRef, e.fromPort, "out");
                const b = anchor(e.toRef, e.toPort, "in");
                if (!a || !b) return null;
                const bad = findings.some((f) => f.edgeId === e.id && f.severity === "error");
                const key = `${e.fromRef}.${e.fromPort}->${e.toRef}.${e.toPort}`;
                const holding = activity.holding.includes(key);
                const hot = activity.hot.includes(key);
                const d = curve(a, b, lanes.get(e.id) ?? 0);
                const cls = [
                  "cedge",
                  isSelected(sel, "edge", e.id) ? "cedge--sel" : "",
                  bad ? "cedge--bad" : "",
                  e.gate === "human-approval" ? "cedge--gated" : "",
                  e.speculative ? "cedge--spec" : "",
                  e.qos === "realtime" ? "cedge--realtime" : "",
                  (activity.edges[key] ?? 0) > 0 ? "cedge--used" : "",
                  hot ? "cedge--hot" : "",
                  holding ? "cedge--holding" : "",
                ].join(" ");
                return (
                  <g key={e.id}>
                    <path className={cls} d={d} />
                    {hot && [0, 1, 2].map((i) => (
                      <circle
                        key={i}
                        className="cedge__particle"
                        r={3}
                        style={{ offsetPath: `path("${d}")`, animationDelay: `${-i * 0.3}s` }}
                      />
                    ))}
                    <path
                      className="cedge__hit"
                      d={d}
                      onPointerDown={(ev) => {
                        ev.stopPropagation();
                        setSel(select(sel, "edge", e.id, ev.shiftKey));
                      }}
                      onContextMenu={(ev) => editable && openMenu(ev, edgeMenu(e.id, ev))}
                      onDoubleClick={(ev) => {
                        if (!editable) return;
                        ev.stopPropagation();
                        setQuick({
                          vx: ev.clientX, vy: ev.clientY,
                          world: toWorld(ev.clientX, ev.clientY), onEdge: e.id,
                        });
                      }}
                    />
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

            {/* What an edge declares and what it has carried, at its midpoint.
                HTML rather than SVG text: a chip is a label with a tooltip. */}
            {graph.edges.map((e) => {
              const a = anchor(e.fromRef, e.fromPort, "out");
              const b = anchor(e.toRef, e.toPort, "in");
              if (!a || !b) return null;
              const key = `${e.fromRef}.${e.fromPort}->${e.toRef}.${e.toPort}`;
              const crossed = activity.edges[key] ?? 0;
              const holding = activity.holding.includes(key);
              const problems = findings.filter((f) => f.edgeId === e.id && f.severity !== "info");
              const gated = e.gate === "human-approval";
              if (!gated && !e.qos && !e.speculative && !crossed && !problems.length && !e.deadline_ms) return null;
              return (
                <div
                  key={`chip-${e.id}`}
                  className={`echip ${holding ? "echip--holding" : ""}`}
                  style={{ left: edgeMid(a, b, lanes.get(e.id) ?? 0).x, top: edgeMid(a, b, lanes.get(e.id) ?? 0).y }}
                  onPointerDown={(ev) => { ev.stopPropagation(); setSel({ nodes: [], edges: [e.id] }); setSide("auto"); }}
                >
                  {problems.length > 0 && (
                    <span className={`echip__i echip__i--${problems.some((f) => f.severity === "error") ? "err" : "warn"}`} title={problems.map((f) => f.message).join("\n")}>
                      <IconAlert size={10} />
                    </span>
                  )}
                  {gated && (
                    <span className="echip__i echip__i--gate" title={t(holding ? "studio.edge.holding" : "studio.edge.gate")}>
                      <IconLock size={10} />{holding ? t("studio.edge.held") : t("studio.edge.gateShort")}
                    </span>
                  )}
                  {e.speculative && <span className="echip__i echip__i--spec" title={t("studio.inspector.speculativeHelp")}>{t("studio.edge.spec")}</span>}
                  {e.qos && <span className={`echip__i echip__i--${e.qos}`} title={t(`studio.edge.qos.${e.qos}`)}>{e.qos}</span>}
                  {e.deadline_ms ? <span className="echip__i" title={t("studio.inspector.deadlineHelp")}>{e.deadline_ms}ms</span> : null}
                  {crossed > 0 && <span className="echip__i echip__i--n" title={t("studio.edge.crossed", { count: crossed })}>{fmtCompact(crossed)}</span>}
                </div>
              );
            })}

            {graph.nodes.map((n) => (
              <div
                key={n.ref}
                className={[
                  "livenode",
                  `livenode--${activity.nodes[n.ref] ?? "idle"}`,
                  n.disabled ? "livenode--off" : "",
                  pins[n.ref] ? "livenode--pinned" : "",
                ].join(" ")}
                onContextMenu={(e) => editable && openMenu(e, nodeMenu(n.ref))}
                onDoubleClick={(e) => {
                  if (!editable || n.isClient) return;
                  e.stopPropagation();
                  setRenaming({ ref: n.ref, draft: n.ref });
                }}
              >
                <GraphNode
                  node={n}
                  ports={portsByRef.get(n.ref) ?? { ingress: [], egress: [] }}
                  selected={isSelected(sel, "node", n.ref)}
                  dropTargets={dropTargets}
                  candidates={candidateCount(n, skills)}
                  findings={findings.filter((f) => f.nodeRef === n.ref)}
                  count={activity.counts[n.ref] ?? 0}
                  stream={activity.text[n.ref]}
                  usage={activity.usage[n.ref]}
                  onPointerDown={onNodePointerDown}
                  onSelect={() => undefined}
                  onPortDown={onPortDown}
                  onPortUp={onPortUp}
                />
                {renaming?.ref === n.ref && (
                  <input
                    className="input noderename"
                    style={{ left: n.x + 8, top: n.y + 8, width: NODE_W - 16 }}
                    autoFocus
                    value={renaming.draft}
                    onChange={(ev) => setRenaming({ ref: n.ref, draft: ev.target.value })}
                    onPointerDown={(ev) => ev.stopPropagation()}
                    onBlur={() => setRenaming(null)}
                    onKeyDown={(ev) => {
                      if (ev.key === "Escape") { ev.stopPropagation(); setRenaming(null); return; }
                      if (ev.key !== "Enter") return;
                      const to = renaming.draft.trim();
                      // Refused rather than silently corrected: a ref the
                      // kernel would reject, or one already taken, means the
                      // author has to see the name they typed and change it.
                      const taken = graph.nodes.some((o) => o.ref === to && o.ref !== n.ref);
                      if (!to || taken || !/^[a-z0-9_-]+$/.test(to)) {
                        setStatus({ kind: "err", text: t("studio.badRef", { ref: to }) });
                        return;
                      }
                      if (to !== n.ref) {
                        edit((g) => rename(g, n.ref, to));
                        setSel({ nodes: [to], edges: [] });
                      }
                      setRenaming(null);
                    }}
                  />
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
              {t("studio.tabRun")}
            </button>
          </div>

          {side === "converse" ? (
            <ConversePanel preferGraph={talkable} onSession={setPanelSession} />
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

      {menu && <ContextMenu x={menu.x} y={menu.y} items={menu.items} onClose={() => setMenu(null)} />}

      {quick && (
        <QuickAdd
          x={quick.vx}
          y={quick.vy}
          skills={catalog}
          throughOnly={!!quick.onEdge}
          title={t(quick.onEdge ? "studio.menu.insertHere" : "studio.menu.addSkill")}
          onClose={() => setQuick(null)}
          onPick={(skill) => { placeSkill(skill, quick.world, quick.onEdge); setQuick(null); }}
        />
      )}

      {showKeys && <Shortcuts onClose={() => setShowKeys(false)} />}

      {showHistory && graph.graphId && (
        <GraphHistory
          graphId={graph.graphId}
          currentDigest={digest}
          onClose={() => setShowHistory(false)}
          onRestore={(ir, n) => {
            edit(() => fromIR(ir));
            setSel(EMPTY_SELECTION);
            // Loaded, not rolled back. Registering it is a separate, deliberate
            // act that the node's history will record as its own entry.
            setStatus({ kind: "ok", text: t("studio.history.restored", { n }) });
          }}
        />
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
