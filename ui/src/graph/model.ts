/**
 * The canvas's model layer: everything about a graph that is *not* React.
 *
 * The C2 IR is the only graph format the executor knows, and it is frozen. This
 * file is the translation between it and what a canvas needs — which is strictly
 * more: an IR node has no position, no size, and no idea which ports it has
 * (that lives in the C1 manifest of whatever its `resolve` resolves to).
 *
 * Two rules shape everything here:
 *
 *   1. **The IR we emit is spec-pure.** No `x`, no `y`, no `_canvas` key. A
 *      graph drawn here must be byte-comparable with one written by hand, or
 *      the editor becomes a dialect. Positions live in localStorage keyed by
 *      graph id, and a graph opened on a machine that has never seen it gets
 *      laid out from its topology instead.
 *   2. **Validation mirrors the kernel, and never outranks it.** Everything in
 *      `validate` is a rule the executor enforces at instantiation anyway. The
 *      canvas runs them early so an author sees the problem while drawing
 *      rather than at session start — but the kernel remains the authority, so
 *      a rule it applies silently (adding an omitted motor gate in `local`
 *      mode) is reported here as information, not as an error.
 */

import type { GraphIR, SkillManifest, SkillType } from "../api/types";

/** Canvas geometry. Node boxes are fixed-width so edges land predictably. */
export const NODE_W = 210;
export const NODE_HEAD = 46;
export const PORT_ROW = 22;
export const PORT_PAD = 8;

/** A node as the canvas holds it: the IR node plus where it sits. */
export interface CanvasNode {
  ref: string;
  resolve?: string;
  use?: string;
  constraints?: Record<string, unknown>;
  x: number;
  y: number;
  /** True for the `client` pseudo-node, which is not a skill and cannot be deleted. */
  isClient?: boolean;
}

/** An edge as the canvas holds it: exactly C2's edge, split into its parts. */
export interface CanvasEdge {
  id: string;
  fromRef: string;
  fromPort: string;
  toRef: string;
  toPort: string;
  gate?: "human-approval" | "none";
  qos?: "reliable" | "realtime" | "bulk";
  speculative?: boolean;
  deadline_ms?: number;
  priority?: number;
}

export interface CanvasGraph {
  graphId: string;
  nodes: CanvasNode[];
  edges: CanvasEdge[];
  contextBudget?: number;
  waves?: string[][];
  origin: GraphIR["origin"];
}

/** A port as drawn: name, schema, and which side of the box it lives on. */
export interface PortSpec {
  name: string;
  schema: string;
}

export interface ResolvedPorts {
  ingress: PortSpec[];
  egress: PortSpec[];
  type?: SkillType;
  /** The manifest we matched, if any — absent means unresolved at draw time. */
  manifest?: SkillManifest;
  /**
   * The port set is a floor, not a ceiling: this node accepts port names
   * nobody declared ahead of time. True only for `client` — see CLIENT_PORTS.
   */
  open?: boolean;
}

/**
 * The `client` pseudo-node's ports (C2: "the pseudo-node `client` represents
 * the external user/application connection").
 *
 * Its direction reads backwards on purpose and it trips everyone once: from the
 * *graph's* point of view `client.text_out` is a source (the user sent it) and
 * `client.text_in` is a sink (the user receives it). So the node's egress list
 * holds `text_out` and its ingress list holds `text_in` — the opposite of the
 * names, because the names are written from the client's point of view and the
 * canvas draws from the graph's.
 *
 * **This list is a floor, not a ceiling.** C2 calls the client's ports
 * "extensible to other schemas", and the planner exercises that: it mints a
 * `client.step<N>_out` per step and declares that port's schema in the
 * session's `inputs[]`, which a registered graph does not carry. So a port
 * named here is one we can label; a port this graph names and we cannot is
 * still real. `open` is what stops the validator from calling it an error.
 */
export const CLIENT_PORTS: ResolvedPorts = {
  open: true,
  ingress: [
    { name: "text_in", schema: "std/text@1" },
    { name: "confirm_in", schema: "std/confirmation@1" },
  ],
  egress: [
    { name: "text_out", schema: "std/text@1" },
    { name: "audio_out", schema: "std/audio-chunk@1" },
  ],
};

/**
 * Resolve a node's ports from the live catalogue.
 *
 * `use` names an exact package and wins when present; `resolve` is a capability
 * demand and may match several skills, in which case the first is drawn and the
 * kernel makes the real choice at instantiation. That difference is surfaced in
 * the UI rather than hidden: drawing one candidate's ports as though they were
 * settled would mislead an author whose capability has two providers with
 * different port names.
 */
export function resolvePorts(node: CanvasNode, skills: SkillManifest[]): ResolvedPorts {
  if (node.isClient) return CLIENT_PORTS;

  let m: SkillManifest | undefined;
  if (node.use) {
    m = skills.find((s) => s.id === node.use || s.id.startsWith(node.use + "@"));
  }
  if (!m && node.resolve) {
    m = skills.find((s) => s.capability === node.resolve);
  }
  if (!m) return { ingress: [], egress: [] };

  return {
    ingress: m.ports?.ingress ?? [],
    egress: m.ports?.egress ?? [],
    type: m.type,
    manifest: m,
  };
}

/**
 * Every node's ports, resolved against the live catalogue **and this graph**.
 *
 * The graph is not decoration here. An open node (`client`) draws whatever
 * ports the edges actually name, so a planner's `step1_out` gets a row of its
 * own instead of quietly borrowing another port's anchor and drawing the edge
 * to the wrong place.
 *
 * One builder, used by the validator and by the canvas, so the boxes you see
 * and the findings you read can never disagree about what a port is.
 */
export function portMap(g: CanvasGraph, skills: SkillManifest[]): Map<string, ResolvedPorts> {
  const m = new Map<string, ResolvedPorts>();
  for (const n of g.nodes) {
    const base = resolvePorts(n, skills);
    if (!base.open) {
      m.set(n.ref, base);
      continue;
    }
    // Extend an open node with the ports this graph names. Schema is left
    // empty: it lives in the session's inputs, which we genuinely do not have.
    const ingress = [...base.ingress];
    const egress = [...base.egress];
    for (const e of g.edges) {
      if (e.fromRef === n.ref && !egress.some((p) => p.name === e.fromPort)) {
        egress.push({ name: e.fromPort, schema: "" });
      }
      if (e.toRef === n.ref && !ingress.some((p) => p.name === e.toPort)) {
        ingress.push({ name: e.toPort, schema: "" });
      }
    }
    m.set(n.ref, { ...base, ingress, egress });
  }
  return m;
}

/** How many candidate skills satisfy a capability demand. */
export function candidateCount(node: CanvasNode, skills: SkillManifest[]): number {
  if (node.isClient) return 1;
  if (node.use) return skills.filter((s) => s.id === node.use || s.id.startsWith(node.use + "@")).length;
  if (node.resolve) return skills.filter((s) => s.capability === node.resolve).length;
  return 0;
}

/** Height of a node box, driven by whichever port column is longer. */
export function nodeHeight(ports: ResolvedPorts): number {
  const rows = Math.max(ports.ingress.length, ports.egress.length, 1);
  return NODE_HEAD + rows * PORT_ROW + PORT_PAD * 2;
}

/** Absolute y of one port row, relative to the node's own origin. */
export function portOffsetY(index: number): number {
  return NODE_HEAD + PORT_PAD + index * PORT_ROW + PORT_ROW / 2;
}

/* ── IR ⇄ canvas ─────────────────────────────────────────────────── */

const POS_KEY = "aura.canvas.positions";

type PositionBook = Record<string, Record<string, { x: number; y: number }>>;

function readPositions(): PositionBook {
  try {
    return JSON.parse(localStorage.getItem(POS_KEY) ?? "{}") as PositionBook;
  } catch {
    return {};
  }
}

/**
 * Remember where the author put things.
 *
 * Deliberately localStorage and deliberately not the IR: a position is a fact
 * about one person's screen, not about the graph, and putting it on the wire
 * would make two functionally identical graphs differ. The cost is that layout
 * does not travel between machines, which is the right thing to lose.
 */
export function savePositions(graphId: string, nodes: CanvasNode[]): void {
  try {
    const book = readPositions();
    book[graphId] = Object.fromEntries(nodes.map((n) => [n.ref, { x: n.x, y: n.y }]));
    localStorage.setItem(POS_KEY, JSON.stringify(book));
  } catch {
    /* a full or disabled localStorage costs layout memory, nothing else */
  }
}

/**
 * Lay out a graph that has no remembered positions.
 *
 * Longest-path layering: a node sits one column to the right of its furthest
 * upstream node, which puts sources on the left, sinks on the right, and makes
 * the common shape (client → cognitive → motor → client) read left to right on
 * the first open. Cycles are normal here — a chat graph loops back to the
 * client — so the walk is depth-bounded rather than assuming a DAG.
 */
export function autoLayout(nodes: CanvasNode[], edges: CanvasEdge[]): CanvasNode[] {
  const depth = new Map<string, number>();
  for (const n of nodes) depth.set(n.ref, 0);

  const incoming = new Map<string, string[]>();
  for (const e of edges) {
    if (e.fromRef === e.toRef) continue;
    incoming.set(e.toRef, [...(incoming.get(e.toRef) ?? []), e.fromRef]);
  }

  // Relax |nodes| times: enough for the longest simple path, and bounded so a
  // cycle terminates instead of spinning.
  for (let pass = 0; pass < nodes.length; pass++) {
    let moved = false;
    for (const n of nodes) {
      const preds = incoming.get(n.ref) ?? [];
      for (const p of preds) {
        // `client` never pushes anything right: it is both the first and the
        // last node in most graphs, and letting it layer would drag the whole
        // graph into one long diagonal.
        if (p === "client") continue;
        const want = (depth.get(p) ?? 0) + 1;
        if (want > (depth.get(n.ref) ?? 0)) {
          depth.set(n.ref, want);
          moved = true;
        }
      }
    }
    if (!moved) break;
  }

  const byColumn = new Map<number, CanvasNode[]>();
  for (const n of nodes) {
    const d = n.isClient ? 0 : (depth.get(n.ref) ?? 0) + 1;
    byColumn.set(d, [...(byColumn.get(d) ?? []), n]);
  }

  const COL_W = NODE_W + 110;
  const ROW_H = 190;
  const out: CanvasNode[] = [];
  for (const [col, group] of [...byColumn.entries()].sort((a, b) => a[0] - b[0])) {
    group.forEach((n, i) => {
      out.push({ ...n, x: 60 + col * COL_W, y: 60 + i * ROW_H });
    });
  }
  return out;
}

/** Build the canvas model from a registered graph's IR. */
export function fromIR(ir: GraphIR): CanvasGraph {
  const refs = new Set(ir.nodes.map((n) => n.ref));
  const nodes: CanvasNode[] = ir.nodes.map((n) => ({
    ref: n.ref,
    resolve: n.resolve,
    use: n.use,
    constraints: n.constraints,
    x: 0,
    y: 0,
  }));

  // `client` is implied by any edge naming it and is never listed in nodes[].
  const usesClient = ir.edges.some((e) => e.from.startsWith("client.") || e.to.startsWith("client."));
  if (usesClient && !refs.has("client")) {
    nodes.unshift({ ref: "client", x: 0, y: 0, isClient: true });
  }

  const edges: CanvasEdge[] = ir.edges.map((e, i) => {
    const [fromRef, fromPort] = splitPortRef(e.from);
    const [toRef, toPort] = splitPortRef(e.to);
    const raw = e as GraphIR["edges"][number] & {
      speculative?: boolean;
      deadline_ms?: number;
      priority?: number;
    };
    return {
      id: `e${i}-${fromRef}.${fromPort}-${toRef}.${toPort}`,
      fromRef,
      fromPort,
      toRef,
      toPort,
      gate: e.gate as CanvasEdge["gate"],
      qos: e.qos as CanvasEdge["qos"],
      speculative: raw.speculative,
      deadline_ms: raw.deadline_ms,
      priority: raw.priority,
    };
  });

  const saved = readPositions()[ir.graph_id];
  const placed = saved
    ? nodes.map((n) => ({ ...n, ...(saved[n.ref] ?? {}) }))
    : autoLayout(nodes, edges);
  // A node added to the IR after the last save has no remembered spot; give the
  // whole graph a fresh layout rather than dropping it at the origin.
  const missing = saved && nodes.some((n) => !saved[n.ref]);

  return {
    graphId: ir.graph_id,
    nodes: missing ? autoLayout(nodes, edges) : placed,
    edges,
    contextBudget: (ir as GraphIR & { context_budget?: number }).context_budget,
    waves: ir.waves,
    origin: ir.origin,
  };
}

export function splitPortRef(s: string): [string, string] {
  const dot = s.indexOf(".");
  return dot < 0 ? [s, ""] : [s.slice(0, dot), s.slice(dot + 1)];
}

/**
 * Serialise back to C2 IR.
 *
 * The `client` pseudo-node is dropped from `nodes[]` — C2 defines it as
 * implied by the edges that name it, and a kernel handed `{"ref":"client"}`
 * would try to resolve a capability by that name. Optional fields are omitted
 * rather than written as null, so a round-trip through the canvas does not
 * inflate a hand-written graph with keys its author did not choose.
 */
export function toIR(g: CanvasGraph): GraphIR {
  const ir: GraphIR & { context_budget?: number } = {
    ir: "1",
    graph_id: g.graphId,
    origin: g.origin ?? { kind: "declared" },
    nodes: g.nodes
      .filter((n) => !n.isClient)
      .map((n) => ({
        ref: n.ref,
        ...(n.use ? { use: n.use } : {}),
        ...(n.resolve && !n.use ? { resolve: n.resolve } : {}),
        ...(n.constraints && Object.keys(n.constraints).length ? { constraints: n.constraints } : {}),
      })),
    edges: g.edges.map((e) => ({
      from: `${e.fromRef}.${e.fromPort}`,
      to: `${e.toRef}.${e.toPort}`,
      ...(e.gate ? { gate: e.gate } : {}),
      ...(e.qos ? { qos: e.qos } : {}),
      ...(e.speculative ? { speculative: e.speculative } : {}),
      ...(typeof e.deadline_ms === "number" ? { deadline_ms: e.deadline_ms } : {}),
      ...(typeof e.priority === "number" ? { priority: e.priority } : {}),
    })),
  };
  if (g.waves?.length) ir.waves = g.waves;
  if (typeof g.contextBudget === "number") ir.context_budget = g.contextBudget;
  return ir;
}

/* ── validation ──────────────────────────────────────────────────── */

export type Severity = "error" | "warn" | "info";

export interface Finding {
  severity: Severity;
  /** C2 rule number this comes from, for the author to go read. */
  rule?: number;
  message: string;
  edgeId?: string;
  nodeRef?: string;
}

const REF_RE = /^[a-z0-9_-]+$/;
const PORT_RE = /^[a-z0-9_]+$/;

/** Schema compatibility per C2 rule 2: same ref and same major. */
function schemaCompatible(a: string, b: string): boolean {
  if (!a || !b) return true; // unknown on either side is not a conflict to report
  const norm = (s: string) => {
    const at = s.lastIndexOf("@");
    return at < 0 ? [s, ""] : [s.slice(0, at), s.slice(at + 1)];
  };
  const [an, av] = norm(a);
  const [bn, bv] = norm(b);
  if (an !== bn) return false;
  return av.split(".")[0] === bv.split(".")[0];
}

/**
 * Run the C2 rules the kernel will run, early.
 *
 * `mode` matters for exactly one rule: an omitted gate on an edge into a motor
 * skill is repaired in `local`/`site` and refuses the session in `published`
 * (rule 5). Reporting it as an error everywhere would train an author to ignore
 * errors; reporting it as info everywhere would let a published graph fail at
 * the worst moment.
 */
export function validate(
  g: CanvasGraph,
  skills: SkillManifest[],
  mode: string,
): Finding[] {
  const out: Finding[] = [];
  const byRef = new Map(g.nodes.map((n) => [n.ref, n]));
  const portsOf = portMap(g, skills);

  if (!g.graphId.trim()) {
    out.push({ severity: "error", message: "The graph needs an id before it can be registered." });
  }
  if (g.nodes.filter((n) => !n.isClient).length === 0) {
    out.push({ severity: "error", message: "A graph needs at least one node besides `client`." });
  }
  if (g.edges.length === 0) {
    out.push({ severity: "error", message: "A graph needs at least one edge." });
  }

  const seenRef = new Set<string>();
  for (const n of g.nodes) {
    if (!REF_RE.test(n.ref)) {
      out.push({
        severity: "error",
        nodeRef: n.ref,
        message: `Ref "${n.ref}" must match [a-z0-9_-]+.`,
      });
    }
    if (seenRef.has(n.ref)) {
      out.push({ severity: "error", nodeRef: n.ref, message: `Duplicate ref "${n.ref}".` });
    }
    seenRef.add(n.ref);

    if (!n.isClient && !n.resolve && !n.use) {
      out.push({
        severity: "error",
        nodeRef: n.ref,
        message: `"${n.ref}" declares neither a capability (resolve) nor a package (use).`,
      });
    }
    if (!n.isClient) {
      const count = candidateCount(n, skills);
      if (count === 0) {
        out.push({
          severity: "warn",
          rule: 1,
          nodeRef: n.ref,
          message: `Nothing in the live catalogue satisfies "${n.use ?? n.resolve}". The kernel refuses a graph it cannot resolve or degrade.`,
        });
      } else if (count > 1 && n.resolve) {
        out.push({
          severity: "info",
          nodeRef: n.ref,
          message: `${count} skills satisfy "${n.resolve}". The kernel picks at instantiation; the ports drawn are one candidate's.`,
        });
      }
    }
  }

  for (const e of g.edges) {
    const from = byRef.get(e.fromRef);
    const to = byRef.get(e.toRef);
    if (!from) {
      out.push({ severity: "error", edgeId: e.id, message: `Edge source "${e.fromRef}" is not a node in this graph.` });
      continue;
    }
    if (!to) {
      out.push({ severity: "error", edgeId: e.id, message: `Edge target "${e.toRef}" is not a node in this graph.` });
      continue;
    }
    if (!PORT_RE.test(e.fromPort) || !PORT_RE.test(e.toPort)) {
      out.push({ severity: "error", edgeId: e.id, message: "Port names must match [a-z0-9_]+." });
    }

    const fromPorts = portsOf.get(e.fromRef)!;
    const toPorts = portsOf.get(e.toRef)!;
    const src = fromPorts.egress.find((p) => p.name === e.fromPort);
    const dst = toPorts.ingress.find((p) => p.name === e.toPort);

    // An open node cannot be missing a port: `portMap` already gave it every
    // port this graph names, and the ones it cannot name are still real.
    if (!fromPorts.open && fromPorts.egress.length && !src) {
      out.push({
        severity: "error",
        edgeId: e.id,
        message: `"${e.fromRef}" has no egress port "${e.fromPort}".`,
      });
    }
    if (!toPorts.open && toPorts.ingress.length && !dst) {
      out.push({
        severity: "error",
        edgeId: e.id,
        message: `"${e.toRef}" has no ingress port "${e.toPort}".`,
      });
    }

    // C2 rule 2, exactly as the kernel applies it: schemas are compared only
    // when **both** ends are declared skills. The kernel skips any edge that
    // touches `client` (executor/session.go, `if fromRef != ClientRef`) because
    // the client's schema arrives with the session's inputs, not the graph.
    //
    // Mirroring that boundary is the whole point. A validator stricter than the
    // kernel paints a working graph red — the shipped `chat` graph wires
    // `llm.status_out` (std/status@1) into `client.text_in` (std/text@1) and
    // runs fine — and a findings bar that cries wolf is one authors learn to
    // stop reading.
    const clientEdge = !!fromPorts.open || !!toPorts.open;
    if (!clientEdge && src && dst && !schemaCompatible(src.schema, dst.schema)) {
      out.push({
        severity: "error",
        rule: 2,
        edgeId: e.id,
        message: `Schema mismatch: ${src.schema} → ${dst.schema}. C2 rule 2 validates compatibility at instantiation.`,
      });
    }

    const destIsMotor = toPorts.type === "motor";

    // Rule 5 — the motor-gate invariant.
    if (destIsMotor && !e.gate) {
      if (mode === "published") {
        out.push({
          severity: "error",
          rule: 5,
          edgeId: e.id,
          message: `This edge acts on the world and declares no gate. In published mode the kernel refuses the session rather than repairing it.`,
        });
      } else {
        out.push({
          severity: "info",
          rule: 5,
          edgeId: e.id,
          message: `The kernel will add a human-approval gate here: the destination acts on the world. Declare it — or "none" — to make the choice visible.`,
        });
      }
    }
    if (destIsMotor && e.gate === "none") {
      out.push({
        severity: "warn",
        rule: 5,
        edgeId: e.id,
        message: `Gate waived on an edge that acts on the world. Node policy decides whether that is honoured, and the ledger records that the graph excused it.`,
      });
    }

    // Rule 6 — the speculation invariant.
    if (e.speculative && destIsMotor) {
      out.push({
        severity: "error",
        rule: 6,
        edgeId: e.id,
        message: `Speculation into a motor skill is refused: a discarded effect is not discarded.`,
      });
    }
    if (e.speculative && e.gate === "human-approval") {
      out.push({
        severity: "error",
        rule: 6,
        edgeId: e.id,
        message: `Speculative and human-approval on one edge are incompatible: asking a human to decide and running before the decision cannot both hold.`,
      });
    }

    if (typeof e.deadline_ms === "number" && e.deadline_ms < 0) {
      out.push({ severity: "error", rule: 7, edgeId: e.id, message: "deadline_ms cannot be negative." });
    }
  }

  // Duplicate edges: same source port to same destination port twice.
  const seenEdge = new Set<string>();
  for (const e of g.edges) {
    const key = `${e.fromRef}.${e.fromPort}->${e.toRef}.${e.toPort}`;
    if (seenEdge.has(key)) {
      out.push({ severity: "warn", edgeId: e.id, message: `Duplicate edge ${key}.` });
    }
    seenEdge.add(key);
  }

  if (typeof g.contextBudget === "number" && g.contextBudget < 0) {
    out.push({ severity: "error", rule: 8, message: "context_budget cannot be negative." });
  }

  return out;
}

/** A ref that does not collide, derived from a capability or package name. */
export function suggestRef(seed: string, taken: Set<string>): string {
  const base =
    seed
      .split(/[./@]/)
      .filter(Boolean)
      .pop()
      ?.replace(/[^a-z0-9_-]/gi, "-")
      .toLowerCase() || "node";
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) {
    const candidate = `${base}-${i}`;
    if (!taken.has(candidate)) return candidate;
  }
}
