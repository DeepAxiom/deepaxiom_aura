/**
 * The editing tools added to reach n8n: splice, align, distribute, notes.
 *
 * Split from editor.test.ts because these are the operations with arithmetic
 * in them, and arithmetic is what a test is actually good for. The invariant
 * from editor.ts still governs every one of them: a graph is never left
 * referring to a node that is not in it.
 */

import assert from "node:assert/strict";
import { test } from "node:test";

import { align, distribute, insertOn } from "../src/graph/editor.ts";
import { fromIR } from "../src/graph/model.ts";
import {
  NOTE_COLORS, NOTE_MIN, addNote, cycleColor, moveNote, patchNote, removeNote, resizeNote,
  type Note,
} from "../src/graph/notes.ts";
import type { GraphIR, SkillManifest } from "../src/api/types.ts";

const skills: SkillManifest[] = [
  {
    id: "example/logical/echo@1.0.0", version: "1.0.0", protocol: "1", name: "echo",
    description: "", capability: "logical.echo", type: "logical", format: "source",
    ports: {
      ingress: [{ name: "text_in", schema: "std/text@1" }],
      egress: [{ name: "text_out", schema: "std/text@1" }],
    },
  },
  {
    id: "example/motor/send@1.0.0", version: "1.0.0", protocol: "1", name: "send",
    description: "", capability: "motor.notify.send", type: "motor", format: "source",
    ports: {
      ingress: [{ name: "text_in", schema: "std/text@1" }],
      egress: [{ name: "status_out", schema: "std/status@1" }],
    },
  },
  {
    // A sink: nothing leaves it, so nothing can be spliced through it.
    id: "example/motor/tts@1.0.0", version: "1.0.0", protocol: "1", name: "tts",
    description: "", capability: "motor.tts.speak", type: "motor", format: "source",
    ports: { ingress: [{ name: "text_in", schema: "std/text@1" }], egress: [] },
  },
];

function ir(nodes: GraphIR["nodes"], edges: GraphIR["edges"], id = "t"): GraphIR {
  return { ir: "1", graph_id: id, origin: { kind: "declared" }, nodes, edges };
}

const size = () => ({ w: 200, h: 100 });

/* ── splice ──────────────────────────────────────────────────────── */

test("dropping a skill on an edge rewires both halves through it", () => {
  const g = fromIR(ir(
    [{ ref: "a", resolve: "logical.echo" }],
    [{ from: "client.text_out", to: "a.text_in" }],
  ));
  const out = insertOn(g, skills, g.edges[0].id, skills[0])!;
  assert.ok(out, "an echo has both an ingress and an egress; it can sit in a chain");

  assert.equal(out.graph.edges.length, 2, "one edge became two, not three");
  assert.ok(
    !out.graph.edges.some((e) => e.id === g.edges[0].id),
    "the original edge must go, or the old path still carries traffic alongside the new one",
  );
  const head = out.graph.edges.find((e) => e.toRef === out.ref)!;
  const tail = out.graph.edges.find((e) => e.fromRef === out.ref)!;
  assert.equal(head.fromRef, "client");
  assert.equal(tail.toRef, "a");
  assert.equal(tail.toPort, "text_in", "the downstream end keeps the port it always had");
});

test("a splice cannot strand a node with only one side wired", () => {
  const g = fromIR(ir(
    [{ ref: "a", resolve: "logical.echo" }],
    [{ from: "client.text_out", to: "a.text_in" }],
  ));
  // tts has no egress: there is no "through" to splice into.
  assert.equal(insertOn(g, skills, g.edges[0].id, skills[2]), null);
});

test("splicing carries the upstream hop's delivery fields but re-decides the gate", () => {
  const g = fromIR(ir(
    [{ ref: "m", resolve: "motor.notify.send" }],
    [{ from: "client.text_out", to: "m.text_in", gate: "human-approval", deadline_ms: 500, priority: 3 }],
  ));
  const out = insertOn(g, skills, g.edges[0].id, skills[0])!;
  const head = out.graph.edges.find((e) => e.toRef === out.ref)!;
  const tail = out.graph.edges.find((e) => e.fromRef === out.ref)!;

  assert.equal(head.deadline_ms, 500, "the deadline described that hop and still does");
  assert.equal(head.priority, 3);
  assert.equal(
    head.gate, undefined,
    "the new destination is an echo; asking a human to approve reaching it is noise",
  );
  assert.equal(
    tail.gate, "human-approval",
    "rule 5 belongs to the destination, and the motor skill is now downstream of the splice",
  );
});

test("splicing in front of a motor skill gates the new hop", () => {
  const g = fromIR(ir(
    [
      { ref: "a", resolve: "logical.echo" },
      { ref: "m", resolve: "motor.notify.send" },
    ],
    [{ from: "a.text_out", to: "m.text_in" }],
  ));
  // Insert the motor skill itself: the head now ends at something that acts.
  const out = insertOn(g, skills, g.edges[0].id, skills[1])!;
  const head = out.graph.edges.find((e) => e.toRef === out.ref)!;
  assert.equal(head.gate, "human-approval");
});

test("splicing an edge that is not there changes nothing", () => {
  const g = fromIR(ir([{ ref: "a", resolve: "logical.echo" }], [{ from: "client.text_out", to: "a.text_in" }]));
  assert.equal(insertOn(g, skills, "no-such-edge", skills[0]), null);
});

/* ── align ───────────────────────────────────────────────────────── */

function laid(points: [string, number, number][]): ReturnType<typeof fromIR> {
  const g = fromIR(ir(
    points.map(([ref]) => ({ ref, resolve: "logical.echo" })),
    [{ from: "client.text_out", to: points[0][0] + ".text_in" }],
  ));
  return {
    ...g,
    nodes: g.nodes.map((n) => {
      const p = points.find(([ref]) => ref === n.ref);
      return p ? { ...n, x: p[1], y: p[2] } : n;
    }),
  };
}

test("align left puts every selected node on the leftmost edge", () => {
  const g = laid([["a", 10, 0], ["b", 90, 50], ["c", 400, 90]]);
  const out = align(g, ["a", "b", "c"], "left", size);
  assert.deepEqual(out.nodes.filter((n) => !n.isClient).map((n) => n.x), [10, 10, 10]);
});

test("align right accounts for width, not just position", () => {
  const g = laid([["a", 0, 0], ["b", 100, 0]]);
  const out = align(g, ["a", "b"], "right", size);
  // Rightmost edge is 100 + 200 = 300, so both left edges land on 100.
  assert.deepEqual(out.nodes.filter((n) => !n.isClient).map((n) => n.x), [100, 100]);
});

test("align centre uses the mean, which is what every drawing tool does", () => {
  const g = laid([["a", 0, 0], ["b", 200, 0]]);
  const out = align(g, ["a", "b"], "cx", size);
  const xs = out.nodes.filter((n) => !n.isClient).map((n) => n.x);
  assert.deepEqual(xs, [100, 100]);
});

test("align leaves unselected nodes exactly where they were", () => {
  const g = laid([["a", 10, 0], ["b", 90, 50], ["c", 400, 90]]);
  const out = align(g, ["a", "b"], "left", size);
  assert.equal(out.nodes.find((n) => n.ref === "c")!.x, 400);
});

test("aligning fewer than two nodes is a no-op, not a move to nowhere", () => {
  const g = laid([["a", 10, 0]]);
  assert.equal(align(g, ["a"], "left", size), g);
  assert.equal(align(g, [], "left", size), g);
});

/* ── distribute ──────────────────────────────────────────────────── */

test("distribute evens the gaps and holds the two extremes still", () => {
  // Three 200-wide boxes between x=0 and x=1000: total span 1000, used 600,
  // so each of the two gaps is 200.
  const g = laid([["a", 0, 0], ["b", 100, 0], ["c", 800, 0]]);
  const out = distribute(g, ["a", "b", "c"], "x", size);
  const by = (r: string) => out.nodes.find((n) => n.ref === r)!.x;
  assert.equal(by("a"), 0, "the first node anchors the span");
  assert.equal(by("c"), 800, "so does the last");
  assert.equal(by("b"), 400);
});

test("distribute orders by position, not by selection order", () => {
  const g = laid([["a", 800, 0], ["b", 0, 0], ["c", 100, 0]]);
  const out = distribute(g, ["a", "b", "c"], "x", size);
  const by = (r: string) => out.nodes.find((n) => n.ref === r)!.x;
  assert.equal(by("b"), 0);
  assert.equal(by("a"), 800);
  assert.equal(by("c"), 400);
});

test("two nodes have one gap, and one gap is already even", () => {
  const g = laid([["a", 0, 0], ["b", 500, 0]]);
  assert.equal(distribute(g, ["a", "b"], "x", size), g);
});

/* ── notes ───────────────────────────────────────────────────────── */

test("a note is centred on the point you asked for", () => {
  const { notes, id } = addNote([], { x: 500, y: 300 });
  const n = notes[0];
  assert.equal(n.id, id);
  assert.equal(n.x + n.w / 2, 500);
  assert.equal(n.y + n.h / 2, 300);
});

test("patching a note cannot change its identity", () => {
  const { notes, id } = addNote([], { x: 0, y: 0 });
  const out = patchNote(notes, id, { text: "why this branch exists", id: "hijacked" } as Partial<Note>);
  assert.equal(out[0].id, id, "an id rewritten under the caller strands every reference to it");
  assert.equal(out[0].text, "why this branch exists");
});

test("resize has a floor, so a note can always be grabbed again", () => {
  const { notes, id } = addNote([], { x: 0, y: 0 });
  const out = resizeNote(notes, id, -9999, -9999);
  assert.equal(out[0].w, NOTE_MIN);
  assert.equal(out[0].h, NOTE_MIN);
});

test("colour cycles and wraps", () => {
  let { notes, id } = addNote([], { x: 0, y: 0 });
  const seen = [notes[0].color];
  for (let i = 0; i < NOTE_COLORS.length; i++) {
    notes = cycleColor(notes, id);
    seen.push(notes[0].color);
  }
  assert.equal(seen[0], seen[seen.length - 1], "a full cycle returns to where it started");
  assert.equal(new Set(seen).size, NOTE_COLORS.length, "and visits every colour once");
});

test("move and remove touch only the note named", () => {
  let notes: Note[] = [];
  const a = addNote(notes, { x: 0, y: 0 });
  notes = a.notes;
  const b = addNote(notes, { x: 100, y: 100 });
  notes = b.notes;

  const moved = moveNote(notes, a.id, 10, 20);
  assert.deepEqual(
    [moved[0].x - notes[0].x, moved[0].y - notes[0].y],
    [10, 20],
  );
  assert.equal(moved[1].x, notes[1].x, "the other note did not move");

  const left = removeNote(moved, a.id);
  assert.deepEqual(left.map((n) => n.id), [b.id]);
});
