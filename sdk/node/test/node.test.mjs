// Tests for createNode() — the path a full-stack app actually uses, and the one
// that had none.
//
// `envelope.test.mjs` covered 42 lines of framing while node.ts's 304 lines of
// registration, manifest derivation and dispatch went untested. That is exactly
// backwards: framing is the part a reader can verify by eye, and the manifest
// derivation is the part that decides whether `write: true` becomes a gated
// motor capability or silently does not.
//
// A fake WebSocket server stands in for the kernel, so these run with no binary
// and no network. The end-to-end check against a real node lives in CI.

import assert from "node:assert/strict";
import { test } from "node:test";
import { WebSocketServer } from "ws";
import { createNode } from "../dist/index.js";

// fakeKernel accepts skill connections, answers each `register` with an ack, and
// records what it was sent.
function fakeKernel() {
  const wss = new WebSocketServer({ port: 0 });
  const registrations = [];
  const sockets = [];
  let onFrame = null;

  wss.on("connection", (ws) => {
    sockets.push(ws);
    ws.on("message", (raw) => {
      const env = JSON.parse(raw.toString());
      if (env.kind === "register") {
        registrations.push(JSON.parse(JSON.stringify(env.payload)));
        ws.send(JSON.stringify({
          v: "1", id: "ack-" + registrations.length, cause_id: env.id,
          kind: "status", payload: { state: "registered", skill: env.payload.id },
        }));
        return;
      }
      if (onFrame) onFrame(env, ws);
    });
  });

  return {
    url: () => `ws://127.0.0.1:${wss.address().port}/ws/skill`,
    registrations,
    sockets,
    set onFrame(fn) { onFrame = fn; },
    close: () => new Promise((r) => wss.close(r)),
  };
}

// waitFor polls until a condition holds, so tests do not depend on a fixed sleep.
async function waitFor(fn, ms = 4000) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (fn()) return;
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error("condition never became true");
}

function nodeFor(kernel, opts = {}) {
  return createNode({ org: "acme", app: "shop", url: kernel.url(), log: () => {}, ...opts });
}

test("expose registers one skill per function", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("get-order", () => ({}), { params: ["id"] });
  aura.expose("create-order", () => ({}), { write: true });
  await aura.start();
  await waitFor(() => k.registrations.length === 2);

  const caps = k.registrations.map((m) => m.capability).sort();
  assert.deepEqual(caps, ["motor.api.shop.create_order", "sensorial.api.shop.get_order"]);
  await aura.stop();
  await k.close();
});

// The one line in an integration that is about safety. If `write: true` did not
// produce a motor capability, the executor would never gate the call and the
// whole promise of the integration would be silently absent.
test("write:true becomes a motor capability, and its absence does not", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("refund", () => ({}), { write: true });
  aura.expose("lookup", () => ({}));
  await aura.start();
  await waitFor(() => k.registrations.length === 2);

  const byCap = Object.fromEntries(k.registrations.map((m) => [m.capability, m]));
  const motor = byCap["motor.api.shop.refund"];
  const sensorial = byCap["sensorial.api.shop.lookup"];

  assert.ok(motor, "a write was not exposed as a motor capability");
  assert.equal(motor.type, "motor");
  assert.ok(sensorial, "a read was not exposed as a sensorial capability");
  assert.equal(sensorial.type, "sensorial");
  await aura.stop();
  await k.close();
});

test("the derived manifest is one the kernel will accept", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("get-order", () => ({}), { params: ["id"] });
  await aura.start();
  await waitFor(() => k.registrations.length === 1);

  const m = k.registrations[0];
  // These are C1's mandatory fields. A manifest missing one is refused at the
  // handshake, which a caller sees as "my app connected and nothing happened".
  assert.match(m.id, /^[a-z0-9-]+\/[a-z0-9-]+\/[a-z0-9-]+$/, `id ${m.id} is not org/cat/name`);
  assert.match(m.version, /^\d+\.\d+\.\d+$/);
  assert.equal(m.protocol, "1");
  assert.ok(m.name && m.description, "name and description are mandatory (the planner reads them)");
  assert.equal(m.format, "source");
  assert.ok(m.ports.ingress.length > 0, "a skill with no ingress port can never be delivered to");
  assert.ok(m.ports.egress.length > 0);
  for (const p of [...m.ports.ingress, ...m.ports.egress]) {
    assert.match(p.name, /^[a-z0-9_]+$/, `port ${p.name} is not a legal C1 port name`);
    assert.match(p.schema, /^[a-z0-9-]+\/[a-z0-9_-]+@\d+$/, `schema ${p.schema} is malformed`);
  }
  await aura.stop();
  await k.close();
});

// Port names are part of the contract a graph author writes against. They were
// undocumented, and getting them wrong is a wiring error the kernel reports at
// session start rather than at registration.
test("ports are named request_in and response_out", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("get-order", () => ({}));
  await aura.start();
  await waitFor(() => k.registrations.length === 1);

  const m = k.registrations[0];
  assert.deepEqual(m.ports.ingress.map((p) => p.name), ["request_in"]);
  assert.deepEqual(m.ports.egress.map((p) => p.name), ["response_out"]);
  await aura.stop();
  await k.close();
});

test("a handler's return value comes back as an api-response", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("get-order", ({ id }) => ({ id, total: 240.5 }), { params: ["id"] });
  await aura.start();
  await waitFor(() => k.registrations.length === 1);

  const replies = [];
  k.onFrame = (env) => replies.push(env);
  k.sockets[0].send(JSON.stringify({
    v: "1", id: "req-1", session: "s1", node: "n", port: "request_in",
    kind: "data", schema: "std/api-request@1",
    payload: { op: "get-order", params: { id: "1002" } },
  }));

  await waitFor(() => replies.some((e) => e.kind === "data"));
  const data = replies.find((e) => e.kind === "data");
  assert.equal(data.payload.ok, true);
  assert.equal(data.payload.body.id, "1002");
  assert.equal(data.cause_id, "req-1", "the reply must cite the request, or causality breaks");
  await aura.stop();
  await k.close();
});

// A thrown handler must become an answer, not a dead skill. The caller asked a
// question and is entitled to something it can act on.
test("a thrown handler becomes an error response rather than a hang", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("boom", () => { throw new Error("the database is on fire"); });
  await aura.start();
  await waitFor(() => k.registrations.length === 1);

  const replies = [];
  k.onFrame = (env) => replies.push(env);
  k.sockets[0].send(JSON.stringify({
    v: "1", id: "req-2", session: "s1", node: "n", port: "request_in",
    kind: "data", schema: "std/api-request@1", payload: { op: "boom", params: {} },
  }));

  await waitFor(() => replies.length > 0);
  const reply = replies[0];
  const body = JSON.stringify(reply.payload);
  assert.ok(reply.payload.ok === false || reply.kind === "error",
    `a thrown handler produced ${body}; it must produce an answer`);
  assert.ok(body.includes("fire") || body.includes("error"),
    `the failure should say something about what happened; got ${body}`);
  await aura.stop();
  await k.close();
});

test("an async handler is awaited", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("slow", async () => {
    await new Promise((r) => setTimeout(r, 30));
    return { done: true };
  });
  await aura.start();
  await waitFor(() => k.registrations.length === 1);

  const replies = [];
  k.onFrame = (env) => replies.push(env);
  k.sockets[0].send(JSON.stringify({
    v: "1", id: "req-3", session: "s1", node: "n", port: "request_in",
    kind: "data", schema: "std/api-request@1", payload: { op: "slow", params: {} },
  }));

  await waitFor(() => replies.some((e) => e.kind === "data"));
  const data = replies.find((e) => e.kind === "data");
  assert.equal(data.payload.body.done, true,
    "an async handler's resolved value was not awaited; a Promise was serialised instead");
  await aura.stop();
  await k.close();
});

test("exposing nothing is refused rather than connecting silently", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  await assert.rejects(() => aura.start(),
    "a node with no exposed functions connected anyway; it can never be resolved");
  await k.close();
});

test("two functions with the same name are refused", async () => {
  const k = fakeKernel();
  const aura = nodeFor(k);
  aura.expose("get-order", () => ({}));
  assert.throws(() => aura.expose("get-order", () => ({})),
    "a duplicate name was accepted; one of the two could never be reached");
  await k.close();
});
