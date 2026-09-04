// A stand-in for an existing full-stack backend, and the whole integration.
//
// What this file demonstrates is what it does *not* do: no framework change, no
// new database, no rewrite, no gate-handling code. Two functions that already
// existed get announced to a running node, and one of them is marked
// `write: true`.
//
// From that one flag, the kernel classifies `refund-order` as a `motor`
// capability, applies a human-approval gate on every edge that reaches it — even
// in a graph whose author never asked for one — and seals the effect into a
// hash-chained ledger with a portable receipt. None of that appears below,
// because none of it is this file's job.
//
// Run it against a node:
//
//   AURA_WS_URL=ws://localhost:9080/ws/skill AURA_TOKEN=<a scoped token> node app.mjs
//
// See GUIDE.md, "When the app is yours".

import { createNode } from "@deepaxiom/aura";

// ── the app we already have ──────────────────────────────────────────
const orders = new Map([
  ["1001", { id: "1001", customer: "ACME", total: 240.5, status: "paid" }],
  ["1002", { id: "1002", customer: "Globex", total: 980.0, status: "pending" }],
]);

// ── the integration ─────────────────────────────────────────────────
const aura = createNode({
  org: "acme",
  app: "shop",
  url: process.env.AURA_WS_URL,
  token: process.env.AURA_TOKEN,
});

// A read. No gate: it does not act on the world, so friction here would be
// friction for nothing.
aura.expose("get-order", ({ id }) => orders.get(String(id)) ?? { error: "no such order" }, {
  params: ["id"],
});

// A write. `write: true` is the only line in this file that is about safety, and
// it is a declaration rather than an implementation — the executor is what
// enforces it, so a graph that forgets the gate still has one.
aura.expose(
  "refund-order",
  ({ id }) => {
    const order = orders.get(String(id));
    if (!order) return { error: "no such order" };
    order.status = "refunded";
    return { refunded: order.id, amount: order.total };
  },
  { params: ["id"], write: true },
);

await aura.start();
console.log("exposed: sensorial.api.shop.get_order, motor.api.shop.refund_order");
