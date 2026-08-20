/**
 * The editing tools: splice, align, distribute, notes, frames, bypass.
 *
 * Split from editor.test.ts because these are the operations with arithmetic
 * in them, and arithmetic is what a test is actually good for. The invariant
 * from editor.ts still governs every one of them: a graph is never left
 * referring to a node that is not in it.
 */

import assert from "node:assert/strict";
import { test } from "node:test";

import { align, distribute, insertOn, toggleDisabled } from "../src/graph/editor.ts";
import { bypass, fromIR, toIR, validate } from "../src/graph/model.ts";
import {
  NOTE_COLORS, NOTE_MIN, addNote, cycleColor, moveNote, patchNote, removeNote, resizeNote,
  type Note,
} from "../src/graph/notes.ts";
import {
  GROUP_MIN, contained, cycleGroupColor, frame, moveGroup, patchGroup, removeGroup, resizeGroup,
} from "../src/graph/groups.ts";
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
  {
    // Reads statuses. Exists so a bypass can be shown breaking rule 2 between
    // two real skills — `send` converts text to status, and switching a
    // converter off is precisely when the schemas stop lining up.
    id: "example/logical/watch@1.0.0", version: "1.0.0", protocol: "1", name: "watch",
    description: "", capability: "logical.status.watch", type: "logical", format: "source",
    ports: {
      ingress: [{ name: "status_in", schema: "std/status@1" }],
      egress: [{ name: "text_out", schema: "std/text@1" }],
    },
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

/* ── bypass ──────────────────────────────────────────────────────── */

/** a -> mid -> b, so switching `mid` off has something to reconnect. */
function chain() {
  return fromIR(ir(
    [
      { ref: "a", resolve: "logical.echo" },
      { ref: "mid", resolve: "logical.echo" },
      { ref: "b", resolve: "logical.echo" },
    ],
    [
      { from: "client.text_out", to: "a.text_in" },
      { from: "a.text_out", to: "mid.text_in" },
      { from: "mid.text_out", to: "b.text_in" },
    ],
  ));
}

test("a disabled node is skipped and the path through it survives", () => {
  const g = toggleDisabled(chain(), ["mid"]);
  const out = bypass(g);

  assert.ok(!out.nodes.some((n) => n.ref === "mid"), "the node is not in the graph that runs");
  assert.ok(
    out.edges.some((e) => e.fromRef === "a" && e.toRef === "b"),
    "turning a node off must not break the chain \u2014 that would just be a slow delete",
  );
  assert.ok(
    !out.edges.some((e) => e.fromRef === "mid" || e.toRef === "mid"),
    "no edge may still name a node the graph no longer contains",
  );
});

test("a bypass joins every input to every output", () => {
  // Two in, two out: four paths ran through this node and all four survive.
  const g = toggleDisabled(fromIR(ir(
    [
      { ref: "i1", resolve: "logical.echo" }, { ref: "i2", resolve: "logical.echo" },
      { ref: "mid", resolve: "logical.echo" },
      { ref: "o1", resolve: "logical.echo" }, { ref: "o2", resolve: "logical.echo" },
    ],
    [
      { from: "client.text_out", to: "i1.text_in" },
      { from: "i1.text_out", to: "mid.text_in" },
      { from: "i2.text_out", to: "mid.text_in" },
      { from: "mid.text_out", to: "o1.text_in" },
      { from: "mid.text_out", to: "o2.text_in" },
    ],
  )), ["mid"]);
  const joined = bypass(g).edges.filter((e) => e.viaDisabled === "mid");
  assert.equal(joined.length, 4);
  assert.deepEqual(
    joined.map((e) => `${e.fromRef}->${e.toRef}`).sort(),
    ["i1->o1", "i1->o2", "i2->o1", "i2->o2"],
  );
});

test("a disabled node with nothing downstream drops its edges rather than inventing one", () => {
  const g = toggleDisabled(fromIR(ir(
    [{ ref: "a", resolve: "logical.echo" }, { ref: "sink", resolve: "logical.echo" }],
    [
      { from: "client.text_out", to: "a.text_in" },
      { from: "a.text_out", to: "sink.text_in" },
    ],
  )), ["sink"]);
  const out = bypass(g);
  assert.ok(!out.edges.some((e) => e.toRef === "sink"));
  assert.equal(out.edges.length, 1, "only the client hop is left");
});

test("the IR never mentions a disabled node", () => {
  const doc = toIR(toggleDisabled(chain(), ["mid"]));
  assert.ok(!doc.nodes.some((n) => n.ref === "mid"));
  assert.ok(!JSON.stringify(doc).includes("mid"), "not in a node, not in an edge, not anywhere");
});

test("bypass fields never reach the wire", () => {
  const edge = toIR(toggleDisabled(chain(), ["mid"])).edges.find((e) => e.from === "a.text_out")!;
  assert.ok(!("viaDisabled" in (edge as Record<string, unknown>)));
  assert.ok(!("id" in (edge as Record<string, unknown>)));
});

test("a bypass that wires two incompatible ports is reported against the node you switched off", () => {
  // eco -> send -> watch is sound: text into send, status out of it, status
  // into watch. `send` is the converter, so switching it off leaves text
  // wired straight into a status port — nobody's problem until the moment
  // the author flips that switch, and the finding has to point at the switch.
  const g = toggleDisabled(fromIR(ir(
    [
      { ref: "eco", resolve: "logical.echo" },
      { ref: "send", resolve: "motor.notify.send" },
      { ref: "watch", resolve: "logical.status.watch" },
    ],
    [
      { from: "client.text_out", to: "eco.text_in" },
      { from: "eco.text_out", to: "send.text_in" },
      { from: "send.status_out", to: "watch.status_in" },
    ],
  )), ["send"]);

  // The graph is clean with the converter in place — otherwise this test
  // proves nothing about bypassing.
  const before = validate(fromIR(ir(
    [
      { ref: "eco", resolve: "logical.echo" },
      { ref: "send", resolve: "motor.notify.send" },
      { ref: "watch", resolve: "logical.status.watch" },
    ],
    [
      { from: "client.text_out", to: "eco.text_in" },
      { from: "eco.text_out", to: "send.text_in", gate: "none" },
      { from: "send.status_out", to: "watch.status_in" },
    ],
  )), skills, "local");
  assert.deepEqual(before.filter((f) => f.severity === "error"), []);

  const found = validate(g, skills, "local").filter((f) => f.severity === "error");
  assert.ok(found.length > 0, "an unusable graph must not validate clean");
  assert.ok(
    found.every((f) => !f.edgeId),
    "a synthesised edge has no box on the canvas; pointing at it gives an unclickable finding",
  );
  assert.ok(found.some((f) => f.nodeRef === "send"), "the node the author switched off");
});

test("toggling a mixed selection sets them all the same way", () => {
  // Flipping each independently is a click nobody can predict.
  let g = toggleDisabled(chain(), ["a"]);
  g = toggleDisabled(g, ["a", "b"]);
  assert.deepEqual(
    g.nodes.filter((n) => !n.isClient).map((n) => !!n.disabled),
    [true, false, true],
    "b was on, so the click turned everything off",
  );
  g = toggleDisabled(g, ["a", "b"]);
  assert.deepEqual(g.nodes.filter((n) => n.ref === "a" || n.ref === "b").map((n) => !!n.disabled), [false, false]);
});

test("the client cannot be switched off", () => {
  const g = toggleDisabled(chain(), ["client"]);
  assert.equal(g.nodes.find((n) => n.isClient)!.disabled, undefined);
});

test("a graph with nothing disabled is returned untouched", () => {
  const g = chain();
  assert.equal(bypass(g), g, "same reference: no allocation on the common path");
});

/* ── frames ──────────────────────────────────────────────────────── */

const node = (ref: string, x: number, y: number) =>
  ({ ref, resolve: "logical.echo", x, y }) as ReturnType<typeof fromIR>["nodes"][number];

test("a frame encloses what it was drawn around, with room for its label", () => {
  const nodes = [node("a", 100, 100), node("b", 400, 300)];
  const { groups, id } = frame([], nodes, size, "retry path")!;
  const g = groups[0];
  assert.equal(g.id, id);
  assert.ok(g.x < 100 && g.y < 100, "the frame starts above and left of its contents");
  assert.ok(g.x + g.w > 600 && g.y + g.h > 400, "and ends past the far corner");
  assert.ok(g.y < 100 - 28, "the label band must not sit on top of the first node's header");
  assert.equal(g.label, "retry path");
});

test("framing nothing produces nothing", () => {
  assert.equal(frame([], [], size), null);
});

test("a frame holds what is wholly inside it, not what merely overlaps", () => {
  // Half a node over the edge is an author mid-drag; moving the frame must
  // not drag along the thing they were pulling out of it.
  const box = { x: 0, y: 0, w: 500, h: 500 };
  const nodes = [
    node("inside", 100, 100),
    node("straddling", 420, 100),
    node("outside", 900, 900),
  ];
  assert.deepEqual(contained(box, nodes, size), ["inside"]);
});

test("a frame resized smaller lets go of what no longer fits", () => {
  const nodes = [node("a", 50, 50), node("b", 600, 50)];
  const { groups } = frame([], nodes, size, "")!;
  assert.deepEqual(contained(groups[0], nodes, size).sort(), ["a", "b"]);

  const shrunk = resizeGroup(groups, groups[0].id, -500, 0);
  assert.deepEqual(contained(shrunk[0], nodes, size), ["a"], "membership is geometry, never a stored list");
});

test("resize has a floor", () => {
  const { groups } = frame([], [node("a", 0, 0)], size, "")!;
  const tiny = resizeGroup(groups, groups[0].id, -9999, -9999);
  assert.equal(tiny[0].w, GROUP_MIN);
  assert.equal(tiny[0].h, GROUP_MIN);
});

test("moving a frame does not touch the others", () => {
  const first = frame([], [node("a", 0, 0)], size, "one")!;
  const second = frame(first.groups, [node("b", 900, 900)], size, "two")!;
  const moved = moveGroup(second.groups, first.id, 40, 60);
  assert.deepEqual([moved[0].x - first.groups[0].x, moved[0].y - first.groups[0].y], [40, 60]);
  assert.equal(moved[1].x, second.groups[1].x);
});

test("patching a frame cannot change its identity", () => {
  const { groups, id } = frame([], [node("a", 0, 0)], size, "")!;
  const out = patchGroup(groups, id, { label: "ingest", id: "hijacked" } as Partial<typeof groups[0]>);
  assert.equal(out[0].id, id);
  assert.equal(out[0].label, "ingest");
});

test("deleting a frame is not deleting its nodes", () => {
  // The frame is annotation. Removing it from the list is the whole operation;
  // nothing here can reach the graph.
  const { groups, id } = frame([], [node("a", 0, 0)], size, "")!;
  assert.deepEqual(removeGroup(groups, id), []);
});

test("frame colour cycles and wraps", () => {
  let { groups, id } = frame([], [node("a", 0, 0)], size, "")!;
  const first = groups[0].color;
  for (let i = 0; i < 5; i++) groups = cycleGroupColor(groups, id);
  assert.equal(groups[0].color, first);
});

test("a bypass never emits the same edge twice", () => {
  // The shipped `chat` graph in miniature: one node whose two egress ports
  // both end at client.text_in. The cross product names that pair twice, and
  // two identical edges would deliver every message down both.
  const g = toggleDisabled(fromIR(ir(
    [{ ref: "fit", resolve: "sensorial.hardware.modelfit" }],
    [
      { from: "client.text_out", to: "fit.command_in" },
      { from: "fit.result_out", to: "client.text_in" },
      { from: "fit.status_out", to: "client.text_in" },
    ],
  )), ["fit"]);

  const edges = toIR(g).edges.map((e) => `${e.from}->${e.to}`);
  assert.deepEqual(edges, ["client.text_out->client.text_in"]);
  assert.equal(new Set(edges).size, edges.length, "no duplicate endpoints");
});
