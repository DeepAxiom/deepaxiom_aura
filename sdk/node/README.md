# @deepaxiom/aura

TypeScript client for the [AURA runtime](../../README.md). Two things:

- **`createNode()`** — expose functions your app already has as skills.
- **`openSession()`** — drive a graph from a frontend or a script.

No runtime dependencies: it uses the WebSocket built into Node 20+ and the
browser. Types are generated from `spec/schemas/`, the same frozen JSON Schemas
the kernel and the conformance suite use, so they cannot drift from the wire.

## Expose your app

A declarative connector is for systems you cannot change (see "Connecting
existing software" in the [README](../../README.md)). This is the other
half: when the code is yours, the cheapest integration is the app
announcing itself. There is no spec to write and no adapter to maintain — the
function that already exists becomes the skill.

```ts
import { createNode } from "@deepaxiom/aura";

const aura = createNode({ org: "acme", app: "shop" });

aura.expose("get-order", ({ id }) => db.orders.find(id), {
  summary: "Fetch one order by id",
  params: ["id"],
});

aura.expose("create-order", (input) => db.orders.create(input), {
  summary: "Create an order from a customer name and a total",
  write: true,          // motor.* — the kernel gates every edge into it
});

await aura.start();
```

That registers `sensorial.api.shop.get_order` and `motor.api.shop.create_order`.
They resolve in graphs, show up as MCP tools, and are readable by the planner —
an exposed function is deliberately indistinguishable from a projected API
operation.

### What `write: true` costs you

Nothing at call time, and everything you want at review time. A write becomes a
`motor.*` capability, and the kernel puts a human-approval gate on every edge
that reaches it — including in graphs whose author forgot to ask for one. There
is no way to expose a write that silently runs.

### Handler contract

```ts
type Handler = (input: Record<string, unknown>, ctx: HandlerContext) => unknown;
```

`input` merges the request's `params` and `body`, which is what a planner
actually produces. Reach for `ctx.raw` if you need the untouched
`std/api-request@1` payload.

Whatever you return is wrapped as `{ ok: true, status: 200, body: <return> }`.
A thrown error becomes `{ ok: false, status: 500, error }` — the caller asked a
question and gets an answer it can act on, and your skill stays connected.

`ctx.cancelled` is true once the caller has abandoned the chain. Checking it is
optional: the kernel already refuses to route anything from a cancelled chain,
so honouring it only saves you the work.

`ctx.config` holds the current values of any `config` you declared, and the
kernel updates it live when someone changes one.

## Drive a graph

```ts
import { openSession } from "@deepaxiom/aura";

const session = await openSession("chat", {
  baseUrl: "http://localhost:9080",
  onEnvelope: (env) => {
    if (env.kind === "data") render(env.payload);
    if (env.kind === "confirm_request") askUser(env.id);
  },
});

const id = session.sendText("hello");
session.cancel(id);                    // abandon it; nothing further arrives
session.respondGate(requestId, true);  // approve a human gate
```

`send()` returns the envelope id because that is what `cancel` names — without
it you cannot abandon what you just asked for.

## Types

```ts
import type { Envelope, Manifest, GraphIR } from "@deepaxiom/aura";
```

`src/generated/types.ts` is produced by `npm run generate` from
`spec/schemas/*.json`. Do not edit it. CI runs `npm run check:generated`, so a
schema change either updates this package or fails the build.

## Build

```bash
npm install
npm run generate     # refresh types from spec/schemas
npm run build        # tsc -> dist/
npm test
```
