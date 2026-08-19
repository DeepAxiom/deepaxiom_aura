/**
 * The editing model.
 *
 * Every test here is an operation that can leave a graph referring to a node
 * it no longer contains — delete, rename, paste — because that is the failure
 * this model exists to prevent, and the kernel only reports it at session
 * instantiation, which is the worst place to learn about it.
 */

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  EMPTY_HISTORY,
  EMPTY_SELECTION,
  commit,
  connect,
  copy,
  paste,
  redo,
  remove,
  rename,
  select,
  selectWithin,
  undo,
} from "../src/graph/editor.ts";
import type { CanvasGraph } from "../src/graph/model.ts";
import type { SkillManifest } from "../src/api/types.ts";

const skills: SkillManifest[] = [
  {
    id: "example/logical/echo@1", version: "1", protocol: "1", name: "echo", description: "",
    capability: "logical.echo", type: "logical", format: "source",
    ports: { ingress: [{ name: "text_in", schema: "std/text@1" }], egress: [{ name: "text_out", schema: "std/text@1" }] },
  },
  {
    id: "example/motor/send@1", version: "1", protocol: "1", name: "send", description: "",
    capability: "motor.notify.send", type: "motor", format: "source",
    ports: { ingress: [{ name: "text_in", schema: "std/text@1" }], egress: [] },
  },
];

function graph(): CanvasGraph {
  return {
    graphId: "g",
    origin: { kind: "declared" },
    nodes: [
      { ref: "client", x: 0, y: 0, isClient: true },
      { ref: "eco", resolve: "logical.echo", x: 300, y: 0 },
      { ref: "send", resolve: "motor.notify.send", x: 600, y: 0 },
    ],
    edges: [
      { id: "e1", fromRef: "client", fromPort: "text_out", toRef: "eco", toPort: "text_in" },
      { id: "e2", fromRef: "eco", fromPort: "text_out", toRef: "send", toPort: "text_in" },
    ],
  };
}

const size = () => ({ w: 210, h: 100 });

/* ── selection ───────────────────────────────────────────────────── */

test("a plain click replaces the selection, a modified one toggles", () => {
  let sel = select(EMPTY_SELECTION, "node", "eco", false);
  assert.deepEqual(sel.nodes, ["eco"]);

  sel = select(sel, "node", "send", false);
  assert.deepEqual(sel.nodes, ["send"], "plain click replaces");

  sel = select(sel, "node", "eco", true);
  assert.deepEqual(sel.nodes.sort(), ["eco", "send"], "additive adds");

  sel = select(sel, "node", "eco", true);
  assert.deepEqual(sel.nodes, ["send"], "additive on a selected item removes it");
});

test("a box selects the nodes it covers and only the edges wholly inside", () => {
  const g = graph();
  const sel = selectWithin(g, skills, { x: 250, y: -50, w: 600, h: 200 }, size);
  assert.deepEqual(sel.nodes.sort(), ["eco", "send"]);
  assert.deepEqual(sel.edges, ["e2"], "e1 has an end outside the box");
});

/* ── history ─────────────────────────────────────────────────────── */

test("undo restores what was there, not what replaced it", () => {
  const before = graph();
  const after = remove(before, { nodes: ["send"], edges: [] });
  const h = commit(EMPTY_HISTORY, before);

  const back = undo(h, after);
  assert.ok(back);
  assert.equal(back.graph.nodes.length, 3, "the first undo must not be a no-op");
});

test("redo returns to the edit, and a new edit abandons the branch", () => {
  const before = graph();
  const after = remove(before, { nodes: ["send"], edges: [] });
  const back = undo(commit(EMPTY_HISTORY, before), after)!;

  const forward = redo(back.history, back.graph)!;
  assert.equal(forward.graph.nodes.length, 2);

  const fresh = commit(back.history, back.graph);
  assert.equal(fresh.future.length, 0, "a new edit must not leave a redo into a graph that never existed");
});

test("undo and redo at the ends return null rather than throwing", () => {
  assert.equal(undo(EMPTY_HISTORY, graph()), null);
  assert.equal(redo(EMPTY_HISTORY, graph()), null);
});

/* ── mutations ───────────────────────────────────────────────────── */

test("deleting a node takes its edges with it", () => {
  const g = remove(graph(), { nodes: ["eco"], edges: [] });
  assert.deepEqual(g.nodes.map((n) => n.ref).sort(), ["client", "send"]);
  assert.equal(g.edges.length, 0, "both edges named eco");
});

test("client cannot be deleted", () => {
  const g = remove(graph(), { nodes: ["client"], edges: [] });
  assert.ok(g.nodes.some((n) => n.isClient), "C2 implies client; a graph without it can talk to nothing");
});

test("renaming a node carries every edge that names it", () => {
  const g = rename(graph(), "eco", "echo-1");
  assert.ok(g.nodes.some((n) => n.ref === "echo-1"));
  assert.equal(g.edges.filter((e) => e.fromRef === "echo-1" || e.toRef === "echo-1").length, 2);
  assert.equal(g.edges.filter((e) => e.fromRef === "eco" || e.toRef === "eco").length, 0);
});

test("paste rewires the copies onto themselves, not onto the originals", () => {
  const g = graph();
  const clip = copy(g, { nodes: ["eco", "send"], edges: [] })!;
  const { graph: out, selection } = paste(g, clip);

  assert.equal(out.nodes.length, 5, "two copies added");
  const fresh = selection.nodes;
  assert.equal(fresh.length, 2);
  assert.ok(!fresh.includes("eco"), "a colliding ref must be renamed");

  const pasted = out.edges.filter((e) => selection.edges.includes(e.id));
  assert.equal(pasted.length, 1);
  assert.ok(
    fresh.includes(pasted[0].fromRef) && fresh.includes(pasted[0].toRef),
    "the pasted edge must join the copies, not reach back into the originals",
  );
});

test("copying nothing but the client yields nothing to paste", () => {
  assert.equal(copy(graph(), { nodes: ["client"], edges: [] }), null);
});

test("connecting into a motor skill writes the gate C2 rule 5 would add", () => {
  const g: CanvasGraph = { ...graph(), edges: [] };
  const out = connect(g, skills, { ref: "eco", port: "text_out" }, { ref: "send", port: "text_in" })!;
  const edge = out.graph.edges.find((e) => e.id === out.edgeId)!;
  assert.equal(edge.gate, "human-approval", "an invisible default is worse than a visible line");
});

test("connecting into a non-motor skill adds no gate", () => {
  const g: CanvasGraph = { ...graph(), edges: [] };
  const out = connect(g, skills, { ref: "client", port: "text_out" }, { ref: "eco", port: "text_in" })!;
  assert.equal(out.graph.edges.find((e) => e.id === out.edgeId)!.gate, undefined);
});

test("a duplicate edge is refused rather than drawn twice", () => {
  const g = graph();
  assert.equal(connect(g, skills, { ref: "eco", port: "text_out" }, { ref: "send", port: "text_in" }), null);
});

test("a port cannot connect to itself", () => {
  const g = graph();
  assert.equal(connect(g, skills, { ref: "eco", port: "text_out" }, { ref: "eco", port: "text_out" }), null);
});
