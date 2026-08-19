/**
 * The canvas model, tested against the real C2 conformance vectors.
 *
 * The first test in this UI, and deliberately of this layer: everything the
 * canvas can get *wrong* in a way that reaches the kernel lives in
 * `src/graph/model.ts` — the IR it serialises and the rules it claims to check
 * early. Pointer maths and CSS are verified by looking at them; a graph that
 * round-trips into something subtly different is not.
 *
 * The vectors are read from `spec/conformance/vectors/graph-ir/`, the same
 * files the kernel's own conformance suite runs, so a change to the frozen
 * contract fails here too instead of drifting quietly.
 *
 *     npm test
 *
 * Runs on node:test with type stripping — no jest, no vitest, no transform
 * pipeline for one file's worth of assertions.
 */

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { test } from "node:test";

import { autoLayout, fromIR, toIR, validate } from "../src/graph/model.ts";
import type { GraphIR, SkillManifest } from "../src/api/types.ts";

const here = dirname(fileURLToPath(import.meta.url));
const VECTORS = join(here, "..", "..", "spec", "conformance", "vectors", "graph-ir");

const vector = (name: string): GraphIR => JSON.parse(readFileSync(join(VECTORS, name), "utf8")) as GraphIR;

/** A catalogue with one skill of each type the invariants care about. */
const skills: SkillManifest[] = [
  {
    id: "example/logical/echo@1.0.0",
    version: "1.0.0",
    protocol: "1",
    name: "echo",
    description: "",
    capability: "logical.echo",
    type: "logical",
    format: "source",
    ports: {
      ingress: [{ name: "text_in", schema: "std/text@1" }],
      egress: [{ name: "text_out", schema: "std/text@1" }],
    },
  },
  {
    id: "example/motor/send@1.0.0",
    version: "1.0.0",
    protocol: "1",
    name: "send",
    description: "",
    capability: "motor.notify.send",
    type: "motor",
    format: "source",
    ports: { ingress: [{ name: "text_in", schema: "std/text@1" }], egress: [] },
  },
];

/** Build IR inline for the rule tests, so each states only what it exercises. */
function ir(nodes: GraphIR["nodes"], edges: GraphIR["edges"], id = "t"): GraphIR {
  return { ir: "1", graph_id: id, origin: { kind: "declared" }, nodes, edges };
}

test("round-trips the conformance vectors byte for byte", () => {
  for (const name of ["valid-minimal.json", "valid-gate-and-waves.json"]) {
    const original = vector(name);
    assert.deepEqual(
      toIR(fromIR(original)),
      original,
      `${name} did not survive a canvas round-trip`,
    );
  }
});

test("materialises `client` on the canvas but never writes it back to nodes[]", () => {
  const g = fromIR(vector("valid-minimal.json"));
  assert.ok(
    g.nodes.some((n) => n.isClient),
    "client should be drawable — C2 implies it from the edges that name it",
  );
  assert.ok(
    !toIR(g).nodes.some((n) => n.ref === "client"),
    "client is a pseudo-node; a kernel handed it would try to resolve a capability by that name",
  );
});

test("C2 rule 5 — an ungated edge into a motor skill, by mode", () => {
  const g = fromIR(ir([{ ref: "send", resolve: "motor.notify.send" }], [{ from: "client.text_out", to: "send.text_in" }]));

  const local = validate(g, skills, "local");
  assert.ok(
    local.some((f) => f.rule === 5 && f.severity === "info"),
    "local mode repairs the omission, so the canvas informs rather than blocks",
  );

  const published = validate(g, skills, "published");
  assert.ok(
    published.some((f) => f.rule === 5 && f.severity === "error"),
    "published mode refuses the session, so the canvas must refuse to register",
  );
});

test("C2 rule 5 — waiving the gate on a motor edge is a warning, not silence", () => {
  const g = fromIR(
    ir([{ ref: "send", resolve: "motor.notify.send" }], [{ from: "client.text_out", to: "send.text_in", gate: "none" }]),
  );
  assert.ok(validate(g, skills, "local").some((f) => f.rule === 5 && f.severity === "warn"));
});

test("C2 rule 6 — the speculation invariant", () => {
  const intoMotor = fromIR(
    ir(
      [{ ref: "send", resolve: "motor.notify.send" }],
      [{ from: "client.text_out", to: "send.text_in", gate: "none", speculative: true } as GraphIR["edges"][number]],
    ),
  );
  assert.ok(
    validate(intoMotor, skills, "local").some((f) => f.rule === 6 && f.severity === "error"),
    "a discarded effect is not discarded",
  );

  const withGate = fromIR(
    ir(
      [{ ref: "eco", resolve: "logical.echo" }],
      [
        {
          from: "client.text_out",
          to: "eco.text_in",
          gate: "human-approval",
          speculative: true,
        } as GraphIR["edges"][number],
      ],
    ),
  );
  assert.ok(
    validate(withGate, skills, "local").some((f) => f.rule === 6 && f.severity === "error"),
    "asking a human to decide and running before the decision cannot both hold",
  );
});

test("C2 rule 2 — port schemas must be compatible", () => {
  const mismatch = fromIR(
    ir([{ ref: "eco", resolve: "logical.echo" }], [{ from: "client.audio_out", to: "eco.text_in" }]),
  );
  assert.ok(validate(mismatch, skills, "local").some((f) => f.rule === 2 && f.severity === "error"));

  const fine = fromIR(ir([{ ref: "eco", resolve: "logical.echo" }], [{ from: "client.text_out", to: "eco.text_in" }]));
  assert.ok(!validate(fine, skills, "local").some((f) => f.rule === 2));
});

test("an unresolvable capability is reported before the kernel refuses it", () => {
  const g = fromIR(ir([{ ref: "ghost", resolve: "cognitive.nothing.here" }], [{ from: "client.text_out", to: "ghost.text_in" }]));
  assert.ok(validate(g, skills, "local").some((f) => f.rule === 1 && f.severity === "warn"));
});

test("layout terminates on a cyclic graph and puts the client on the left", () => {
  const g = fromIR(
    ir(
      [{ ref: "eco", resolve: "logical.echo" }],
      [
        { from: "client.text_out", to: "eco.text_in" },
        { from: "eco.text_out", to: "client.text_in" },
      ],
      "loop",
    ),
  );
  const laid = autoLayout(g.nodes, g.edges);
  assert.equal(laid.length, g.nodes.length, "a chat graph loops back to the client; layout must still terminate");
  const client = laid.find((n) => n.isClient)!;
  const skill = laid.find((n) => !n.isClient)!;
  assert.ok(client.x < skill.x, "the client is a source and belongs left of what it feeds");
});

test("optional edge fields are omitted rather than written as null", () => {
  const g = fromIR(ir([{ ref: "eco", resolve: "logical.echo" }], [{ from: "client.text_out", to: "eco.text_in" }]));
  const edge = toIR(g).edges[0] as Record<string, unknown>;
  for (const key of ["gate", "qos", "speculative", "deadline_ms", "priority"]) {
    assert.ok(!(key in edge), `${key} should be absent, not null — a round-trip must not inflate a hand-written graph`);
  }
});
