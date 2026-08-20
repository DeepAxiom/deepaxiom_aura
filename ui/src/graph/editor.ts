/**
 * The editing model: every way a graph changes, as pure functions over state.
 *
 * React-free and DOM-free on purpose. Undo, multi-select, duplication and
 * paste are the operations with real edge cases — a ref renamed while it is
 * selected, a paste that collides with an existing name, an undo across a
 * delete that took edges with it — and a model that returns data instead of
 * calling setState can be asserted on directly. The view above this file
 * decides *when* an operation runs; every rule about *what* it does is here.
 *
 * One invariant runs through all of it: **a graph is never left referring to a
 * node that is not in it.** Delete takes its edges, rename carries them, and
 * paste rewrites them onto the copies. Anything that breaks that produces IR
 * the kernel refuses at instantiation, which is the worst place to find out.
 */

// Explicit .ts: node:test resolves these at runtime, and only the value
// import needs it — the type ones are stripped before Node ever sees them.
import type { CanvasEdge, CanvasGraph, CanvasNode } from "./model";
import { NODE_W, resolvePorts, suggestRef } from "./model.ts";
import type { SkillManifest } from "../api/types";

/* ── selection ───────────────────────────────────────────────────── */

export interface Selection {
  nodes: string[];
  edges: string[];
}

export const EMPTY_SELECTION: Selection = { nodes: [], edges: [] };

export function isSelected(sel: Selection, kind: "node" | "edge", id: string): boolean {
  return (kind === "node" ? sel.nodes : sel.edges).includes(id);
}

/** Click semantics: plain replaces the selection, additive toggles one item. */
export function select(
  sel: Selection,
  kind: "node" | "edge",
  id: string,
  additive: boolean,
): Selection {
  const key = kind === "node" ? "nodes" : "edges";
  if (!additive) return { ...EMPTY_SELECTION, [key]: [id] } as Selection;
  const current = sel[key];
  const next = current.includes(id) ? current.filter((x) => x !== id) : [...current, id];
  return { ...sel, [key]: next };
}

/** Everything whose box intersects the rectangle, in world coordinates. */
export function selectWithin(
  graph: CanvasGraph,
  skills: SkillManifest[],
  rect: { x: number; y: number; w: number; h: number },
  nodeSize: (n: CanvasNode) => { w: number; h: number },
): Selection {
  const x2 = rect.x + rect.w;
  const y2 = rect.y + rect.h;
  const nodes = graph.nodes
    .filter((n) => {
      const { w, h } = nodeSize(n);
      return n.x < x2 && n.x + w > rect.x && n.y < y2 && n.y + h > rect.y;
    })
    .map((n) => n.ref);
  // An edge counts as selected when both ends are: a box drawn around two
  // nodes means "these two and what joins them", never a dangling edge whose
  // other end is outside the box.
  const edges = graph.edges
    .filter((e) => nodes.includes(e.fromRef) && nodes.includes(e.toRef))
    .map((e) => e.id);
  void skills;
  return { nodes, edges };
}

/* ── history ─────────────────────────────────────────────────────── */

export interface History {
  past: CanvasGraph[];
  future: CanvasGraph[];
}

export const EMPTY_HISTORY: History = { past: [], future: [] };

/** How many steps back an author can go. Deep enough to undo a bad idea. */
const HISTORY_LIMIT = 50;

/**
 * Record the state *before* a change.
 *
 * Called with the outgoing graph, not the incoming one, because undo restores
 * what was there — and pushing the new state would make the first undo a no-op,
 * which is the classic off-by-one in every hand-rolled history.
 */
export function commit(history: History, before: CanvasGraph): History {
  const past = [...history.past, before].slice(-HISTORY_LIMIT);
  // Any new edit abandons the redo branch. Keeping it would let an author redo
  // their way into a graph that never existed.
  return { past, future: [] };
}

export function undo(history: History, current: CanvasGraph): { history: History; graph: CanvasGraph } | null {
  if (!history.past.length) return null;
  const graph = history.past[history.past.length - 1];
  return {
    graph,
    history: { past: history.past.slice(0, -1), future: [current, ...history.future] },
  };
}

export function redo(history: History, current: CanvasGraph): { history: History; graph: CanvasGraph } | null {
  if (!history.future.length) return null;
  const graph = history.future[0];
  return {
    graph,
    history: { past: [...history.past, current], future: history.future.slice(1) },
  };
}

/* ── mutations ───────────────────────────────────────────────────── */

/** Remove nodes and edges, and every edge left dangling by a removed node. */
export function remove(graph: CanvasGraph, sel: Selection): CanvasGraph {
  // `client` is implied by C2 rather than declared, so it is not an author's
  // to delete — a graph with no client is a graph nothing can talk to.
  const refs = new Set(
    sel.nodes.filter((ref) => !graph.nodes.find((n) => n.ref === ref)?.isClient),
  );
  const ids = new Set(sel.edges);
  return {
    ...graph,
    nodes: graph.nodes.filter((n) => !refs.has(n.ref)),
    edges: graph.edges.filter(
      (e) => !ids.has(e.id) && !refs.has(e.fromRef) && !refs.has(e.toRef),
    ),
  };
}

/** Rename a node, carrying every edge that names it. */
export function rename(graph: CanvasGraph, from: string, to: string): CanvasGraph {
  if (from === to) return graph;
  return {
    ...graph,
    nodes: graph.nodes.map((n) => (n.ref === from ? { ...n, ref: to } : n)),
    edges: graph.edges.map((e) => ({
      ...e,
      fromRef: e.fromRef === from ? to : e.fromRef,
      toRef: e.toRef === from ? to : e.toRef,
    })),
  };
}

export function moveBy(graph: CanvasGraph, refs: string[], dx: number, dy: number): CanvasGraph {
  const set = new Set(refs);
  return {
    ...graph,
    nodes: graph.nodes.map((n) => (set.has(n.ref) ? { ...n, x: n.x + dx, y: n.y + dy } : n)),
  };
}

export interface Clipboard {
  nodes: CanvasNode[];
  edges: CanvasEdge[];
}

/** Copy the selection, plus the edges wholly inside it. */
export function copy(graph: CanvasGraph, sel: Selection): Clipboard | null {
  const nodes = graph.nodes.filter((n) => sel.nodes.includes(n.ref) && !n.isClient);
  if (!nodes.length) return null;
  const refs = new Set(nodes.map((n) => n.ref));
  const edges = graph.edges.filter((e) => refs.has(e.fromRef) && refs.has(e.toRef));
  return { nodes, edges };
}

/**
 * Paste, offset, with fresh refs where the old ones are taken.
 *
 * Edges are rewritten onto the new refs rather than copied verbatim. Copying
 * them as-is would wire the pasted nodes into the originals — which looks
 * plausible on screen and is almost never what anyone meant.
 */
export function paste(
  graph: CanvasGraph,
  clip: Clipboard,
  offset = 40,
): { graph: CanvasGraph; selection: Selection } {
  const taken = new Set(graph.nodes.map((n) => n.ref));
  const remap = new Map<string, string>();

  const nodes = clip.nodes.map((n) => {
    const ref = suggestRef(n.ref, taken);
    taken.add(ref);
    remap.set(n.ref, ref);
    return { ...n, ref, x: n.x + offset, y: n.y + offset };
  });

  const stamp = Date.now().toString(36);
  const edges = clip.edges.map((e, i) => ({
    ...e,
    id: `e${stamp}-${i}-paste`,
    fromRef: remap.get(e.fromRef) ?? e.fromRef,
    toRef: remap.get(e.toRef) ?? e.toRef,
  }));

  return {
    graph: { ...graph, nodes: [...graph.nodes, ...nodes], edges: [...graph.edges, ...edges] },
    selection: { nodes: nodes.map((n) => n.ref), edges: edges.map((e) => e.id) },
  };
}

/** Add one skill as a node at a point, wired to nothing. */
export function addSkill(
  graph: CanvasGraph,
  skill: SkillManifest,
  at: { x: number; y: number },
): { graph: CanvasGraph; ref: string } {
  const taken = new Set(graph.nodes.map((n) => n.ref));
  const ref = suggestRef(skill.capability || skill.id, taken);

  // Place it clear of what is already there. Skills added in a row from the
  // palette all land at the centre of the view, and a node dropped on top of
  // another looks like the click did nothing. The step is a node width rather
  // than a token nudge, so two nodes are visibly two nodes.
  let { x, y } = at;
  const overlaps = (nx: number, ny: number) =>
    graph.nodes.some((n) => Math.abs(n.x - nx) < NODE_W * 0.8 && Math.abs(n.y - ny) < 90);
  let guard = 0;
  while (overlaps(x, y) && guard++ < 64) {
    x += NODE_W + 40;
    // Wrap to a new row rather than marching off to the right forever.
    if (x > at.x + (NODE_W + 40) * 3) {
      x = at.x;
      y += 150;
    }
  }

  return {
    graph: { ...graph, nodes: [...graph.nodes, { ref, resolve: skill.capability, x, y }] },
    ref,
  };
}

/**
 * Connect two ports, applying the one rule the kernel would apply anyway.
 *
 * C2 rule 5: an edge into a skill that acts on the world carries a
 * human-approval gate. Writing it at draw time rather than letting the kernel
 * add it silently is what turns an invisible default into a line the author
 * can see and a reviewer can grep for.
 */
export function connect(
  graph: CanvasGraph,
  skills: SkillManifest[],
  from: { ref: string; port: string },
  to: { ref: string; port: string },
): { graph: CanvasGraph; edgeId: string } | null {
  if (from.ref === to.ref && from.port === to.port) return null;
  const exists = graph.edges.some(
    (e) =>
      e.fromRef === from.ref && e.fromPort === from.port &&
      e.toRef === to.ref && e.toPort === to.port,
  );
  if (exists) return null;

  const id = `e${Date.now().toString(36)}-${from.ref}.${from.port}-${to.ref}.${to.port}`;
  const target = graph.nodes.find((n) => n.ref === to.ref);
  const edge: CanvasEdge = {
    id, fromRef: from.ref, fromPort: from.port, toRef: to.ref, toPort: to.port,
  };
  if (target && resolvePorts(target, skills).type === "motor") edge.gate = "human-approval";

  return { graph: { ...graph, edges: [...graph.edges, edge] }, edgeId: id };
}

/* ── splicing ────────────────────────────────────────────────────── */

/**
 * Drop a skill onto an existing edge so it sits between the two ends.
 *
 * n8n's most-used editing gesture, and the one that turns a drawn graph into
 * an edited one: you have `a → b` and you want `a → filter → b` without
 * deleting the connection, placing a node, and drawing two new ones.
 *
 * Returns null when the skill cannot sit in a chain — something with no
 * ingress, or no egress, has no "through" to splice into. Better to refuse
 * than to leave a node wired on one side and dangling on the other, which
 * looks connected and is not.
 */
export function insertOn(
  graph: CanvasGraph,
  skills: SkillManifest[],
  edgeId: string,
  skill: SkillManifest,
): { graph: CanvasGraph; ref: string } | null {
  const edge = graph.edges.find((e) => e.id === edgeId);
  if (!edge) return null;

  const inPort = skill.ports?.ingress?.[0]?.name;
  const outPort = skill.ports?.egress?.[0]?.name;
  if (!inPort || !outPort) return null;

  const from = graph.nodes.find((n) => n.ref === edge.fromRef);
  const to = graph.nodes.find((n) => n.ref === edge.toRef);
  if (!from || !to) return null;

  // Midway between the two ends, which is where the edge the author clicked
  // actually runs. Placed before addSkill so its overlap walk can push the
  // node clear if something is already sitting there.
  const at = { x: (from.x + to.x) / 2, y: (from.y + to.y) / 2 };
  const placed = addSkill(graph, skill, at);
  const ref = placed.ref;

  const stamp = Date.now().toString(36);
  const head: CanvasEdge = {
    // The upstream half keeps the original edge's delivery semantics: a
    // deadline or a priority was a statement about that hop, and dropping it
    // on splice would silently change how the graph behaves under load.
    ...edge,
    id: `e${stamp}-${ref}-head`,
    toRef: ref,
    toPort: inPort,
  };
  const tail: CanvasEdge = {
    id: `e${stamp}-${ref}-tail`,
    fromRef: ref,
    fromPort: outPort,
    toRef: edge.toRef,
    toPort: edge.toPort,
  };
  // Rule 5 belongs to the destination, so it moves to whichever half now ends
  // at a motor skill rather than staying on the half that inherited the fields.
  delete head.gate;
  if (resolvePorts({ ref, resolve: skill.capability, x: 0, y: 0 }, skills).type === "motor") {
    head.gate = "human-approval";
  }
  if (edge.gate) tail.gate = edge.gate;

  return {
    ref,
    graph: {
      ...placed.graph,
      edges: [...placed.graph.edges.filter((e) => e.id !== edgeId), head, tail],
    },
  };
}

/* ── tidying a selection ─────────────────────────────────────────── */

export type AlignEdge = "left" | "right" | "top" | "bottom" | "cx" | "cy";

export interface Size {
  w: number;
  h: number;
}

/**
 * Line up the selected nodes.
 *
 * Sizes come in from the caller because a node's height depends on how many
 * ports its manifest declares, and this module deliberately knows nothing
 * about the catalogue.
 */
export function align(
  graph: CanvasGraph,
  refs: string[],
  edge: AlignEdge,
  sizeOf: (n: CanvasNode) => Size,
): CanvasGraph {
  const picked = graph.nodes.filter((n) => refs.includes(n.ref));
  if (picked.length < 2) return graph;

  const boxes = picked.map((n) => ({ n, ...sizeOf(n) }));
  // Align to the extreme in the direction asked for, and to the mean when
  // centring — matching what every drawing tool does, so the result is the one
  // an author already expects before they click.
  const target = {
    left: Math.min(...boxes.map((b) => b.n.x)),
    right: Math.max(...boxes.map((b) => b.n.x + b.w)),
    top: Math.min(...boxes.map((b) => b.n.y)),
    bottom: Math.max(...boxes.map((b) => b.n.y + b.h)),
    cx: boxes.reduce((a, b) => a + b.n.x + b.w / 2, 0) / boxes.length,
    cy: boxes.reduce((a, b) => a + b.n.y + b.h / 2, 0) / boxes.length,
  }[edge];

  const moved = new Map(
    boxes.map((b) => {
      const at = { x: b.n.x, y: b.n.y };
      if (edge === "left") at.x = target;
      if (edge === "right") at.x = target - b.w;
      if (edge === "cx") at.x = target - b.w / 2;
      if (edge === "top") at.y = target;
      if (edge === "bottom") at.y = target - b.h;
      if (edge === "cy") at.y = target - b.h / 2;
      return [b.n.ref, at];
    }),
  );

  return {
    ...graph,
    nodes: graph.nodes.map((n) => (moved.has(n.ref) ? { ...n, ...moved.get(n.ref)! } : n)),
  };
}

/**
 * Even out the gaps along one axis, holding the two extremes still.
 *
 * Gaps rather than positions: spacing centres evenly makes boxes of different
 * heights look wrong, because the eye reads the whitespace between them and
 * not the distance between their middles.
 */
export function distribute(
  graph: CanvasGraph,
  refs: string[],
  axis: "x" | "y",
  sizeOf: (n: CanvasNode) => Size,
): CanvasGraph {
  const picked = graph.nodes.filter((n) => refs.includes(n.ref));
  // Two nodes have one gap, and one gap is already even.
  if (picked.length < 3) return graph;

  const span = (n: CanvasNode) => (axis === "x" ? sizeOf(n).w : sizeOf(n).h);
  const at = (n: CanvasNode) => (axis === "x" ? n.x : n.y);

  const ordered = [...picked].sort((a, b) => at(a) - at(b));
  const first = ordered[0];
  const last = ordered[ordered.length - 1];
  const total = at(last) + span(last) - at(first);
  const used = ordered.reduce((sum, n) => sum + span(n), 0);
  const gap = (total - used) / (ordered.length - 1);

  const moved = new Map<string, number>();
  let cursor = at(first);
  for (const n of ordered) {
    moved.set(n.ref, cursor);
    cursor += span(n) + gap;
  }

  return {
    ...graph,
    nodes: graph.nodes.map((n) =>
      moved.has(n.ref)
        ? { ...n, ...(axis === "x" ? { x: moved.get(n.ref)! } : { y: moved.get(n.ref)! }) }
        : n,
    ),
  };
}
