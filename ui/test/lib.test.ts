/**
 * The pure logic behind Pulse, the session waterfall, routing and the command
 * palette. Each of these decides what a person reads off the screen, so each
 * is asserted on directly rather than through a rendered component.
 */

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  NOT_REPORTED, fmtAgo, fmtBytes, fmtCompact, fmtInt, fmtMs, fmtPct, fmtUptime, shortId, shortSchema,
} from "../src/lib/format.ts";
import { filterCommands, score } from "../src/lib/palette.ts";
import { DEFAULT_ROUTE, buildRoute, parseRoute, sameRoute } from "../src/lib/route.ts";
import {
  type NodeStats, cacheHitRatio, mailboxFill, pushBounded, rate, rateSeries, ratesBetween, ratio,
  replicaScale, replicaViews, sparkSegments, speculationHitRate,
} from "../src/lib/stats.ts";
import { edgeCrossing, filterTrace, foldTrace, traceIdOf, visibleRows } from "../src/lib/trace.ts";
import type { Envelope } from "../src/api/types.ts";

/* ── format ─────────────────────────────────────────────────────── */

test("a value the node did not report is never drawn as zero", () => {
  for (const f of [fmtInt, fmtCompact, fmtBytes, fmtPct, fmtUptime, fmtMs]) {
    assert.equal(f(null), NOT_REPORTED);
    assert.equal(f(undefined), NOT_REPORTED);
    assert.equal(f(Number.NaN), NOT_REPORTED);
  }
  assert.equal(fmtInt(0), "0");
});

test("numbers format at the precision a person reads", () => {
  assert.equal(fmtInt(1234567), "1,234,567");
  assert.equal(fmtCompact(999), "999");
  assert.equal(fmtCompact(1234), "1.2k");
  assert.equal(fmtCompact(3_400_000), "3.4M");
  assert.equal(fmtCompact(123_456), "123k");
  assert.equal(fmtBytes(512), "512 B");
  assert.equal(fmtBytes(145223), "142 KiB");
  assert.equal(fmtBytes(1536), "1.5 KiB");
  assert.equal(fmtPct(0.853), "85%");
  assert.equal(fmtPct(0.853, 1), "85.3%");
  assert.equal(fmtUptime(75), "1m 15s");
  assert.equal(fmtUptime(93784), "1d 2h");
  assert.equal(fmtUptime(0), "0s");
  assert.equal(fmtMs(0.4), "0.4 ms");
  assert.equal(fmtMs(12.4), "12 ms");
  assert.equal(fmtMs(1240), "1.24 s");
  assert.equal(fmtMs(125_000), "2m 05s");
  assert.equal(fmtAgo(1000, 91_000), "2m");
  assert.equal(fmtAgo(0, 91_000), NOT_REPORTED);
});

test("ids and schemas shorten without losing what identifies them", () => {
  assert.equal(shortId("sha256:467441b0495f49e0da77c0438f6bb7e1"), "467441…b7e1");
  assert.equal(shortId("abc"), "abc");
  assert.equal(shortSchema("std/text@1"), "text@1");
  assert.equal(shortSchema("plain"), "plain");
  assert.equal(shortSchema(undefined), "");
});

/* ── stats ──────────────────────────────────────────────────────── */

const base: NodeStats = {
  at: 10_000, uptime_s: 10, sessions_live: 2,
  expertise: [
    { id: "b/x", capability: "c.b", type: "logical", replicas: [1], sessions: 1 },
    { id: "a/x", capability: "c.a", type: "cognitive", replicas: [3, 2, 3], sessions: 8 },
  ],
  tokens: { input_tokens: 7200, output_tokens: 960, cached_input_tokens: 6144, inferences: 8 },
  mailbox: { enabled: true, depth: 1024, queued: 256, waits: 0, dropped: 0 },
  speculation: { attempts: 10, hits: 6, misses: 2, refused: 2 },
  store: { commits: 100, rows: 400, batch_mean: 4, batch_largest: 9, commit_ms_mean: 0.3 },
};

test("ratios are null when there is nothing to divide by", () => {
  assert.equal(ratio(1, 0), null);
  assert.equal(ratio(undefined, 4), null);
  assert.equal(ratio(1, 4), 0.25);
  assert.equal(cacheHitRatio(base), 6144 / 7200);
  assert.equal(cacheHitRatio({ ...base, tokens: undefined }), null);
  assert.equal(cacheHitRatio({ ...base, tokens: { input_tokens: 0, output_tokens: 0, cached_input_tokens: 0, inferences: 0 } }), null);
  assert.equal(cacheHitRatio(null), null);
});

test("speculation hit rate ignores what was refused", () => {
  assert.equal(speculationHitRate(base), 0.75);
  assert.equal(speculationHitRate({ ...base, speculation: { attempts: 0, hits: 0, misses: 0, refused: 0 } }), null);
});

test("mailbox fill is not reported when dispatch is inline", () => {
  assert.equal(mailboxFill(base), 0.25);
  assert.equal(mailboxFill({ ...base, mailbox: { ...base.mailbox!, enabled: false } }), null);
  assert.equal(mailboxFill({ ...base, mailbox: { ...base.mailbox!, queued: 5000 } }), 1);
  assert.equal(mailboxFill({ ...base, mailbox: undefined }), null);
});

test("a rate needs two usable samples", () => {
  assert.equal(rate(100, 160, 1000, 3000), 30);
  assert.equal(rate(100, 160, 1000, 1000), null, "same instant");
  assert.equal(rate(160, 100, 1000, 2000), null, "a counter going down is a restart, not a negative rate");
  assert.equal(rate(undefined, 160, 1000, 2000), null);
});

test("rates between two polls, and a series across a window", () => {
  const next: NodeStats = {
    ...base, at: 12_000,
    tokens: { input_tokens: 7400, output_tokens: 1000, cached_input_tokens: 6200, inferences: 9 },
    store: { ...base.store!, commits: 110, rows: 460 },
  };
  const r = ratesBetween(base, next);
  assert.equal(r.tokensIn, 100);
  assert.equal(r.tokensOut, 20);
  assert.equal(r.commits, 5);
  assert.equal(r.rows, 30);
  const restarted: NodeStats = { ...next, at: 13_000, tokens: { input_tokens: 5, output_tokens: 1, cached_input_tokens: 0, inferences: 1 } };
  assert.deepEqual(rateSeries([base, next, restarted], (x) => x.tokensOut), [20, null]);
  assert.equal(ratesBetween(base, { ...next, tokens: undefined }).tokensIn, null);
});

test("the sample window is bounded and does not mutate", () => {
  const a = [1, 2, 3];
  const b = pushBounded(a, 4, 3);
  assert.deepEqual(b, [2, 3, 4]);
  assert.deepEqual(a, [1, 2, 3]);
  assert.deepEqual(pushBounded([], 1, 3), [1]);
});

test("replica rows: busiest first, with the spread routing is judged by", () => {
  const views = replicaViews(base);
  assert.deepEqual(views.map((v) => v.id), ["a/x", "b/x"]);
  assert.equal(views[0].spread, 1);
  assert.equal(views[1].spread, 0);
  assert.equal(replicaScale(views), 3);
  assert.equal(replicaScale([]), 1, "an idle node still has a scale");
  assert.deepEqual(replicaViews(null), []);
});

test("a sparkline breaks at a missing sample instead of dipping to zero", () => {
  const segs = sparkSegments([1, 2, null, 4], 30, 10);
  assert.equal(segs.length, 2);
  assert.equal(segs[0].split(" ").length, 2);
  assert.equal(segs[1].split(" ").length, 1);
  assert.deepEqual(sparkSegments([null, null], 30, 10), []);
  // Right-aligned in a fixed number of slots, so the line grows from the right.
  assert.ok(sparkSegments([5], 30, 10, 4)[0].startsWith("30.0,"));
});

/* ── trace fold ─────────────────────────────────────────────────── */

function env(id: string, cause: string | undefined, node: string, port: string, extra: Partial<Envelope> = {}): Envelope {
  return { v: "1", id, cause_id: cause, node, port, kind: "data", schema: "std/text@1", payload: { text: "", final: false }, ...extra };
}

const TP = "00-87363e9caaeb77838813c3a18b04d666-2e60b4b782f0a93a-01";

const session: Envelope[] = [
  env("in", undefined, "client", "text_out", { payload: { text: "hi", final: true }, trace: TP }),
  env("d", "in", "draft", "text_in", { payload: { text: "hi", final: true } }),
  env("t1", "d", "draft", "text_out", { payload: { text: "Hel", final: false } }),
  env("c1", "t1", "client", "text_in", { payload: { elided: true, bytes: 29, sha256: "x" } }),
  env("t2", "d", "draft", "text_out", { payload: { text: "lo", final: false } }),
  env("c2", "t2", "client", "text_in", { payload: { elided: true, bytes: 28, sha256: "y" } }),
  env("t3", "d", "draft", "text_out", {
    payload: { text: "", final: true },
    attest: { engine: "e", model: "m", usage: { input_tokens: 188, output_tokens: 76, cached_input_tokens: 180 } },
  }),
  env("full", "d", "draft", "full_out", { payload: { text: "Hello", final: true } }),
  env("gate", "full", "client", "confirm_in", { kind: "confirm_request", payload: { question: "Approve delivery to notify.text_in?" } }),
  env("ans", "gate", "client", "", { kind: "confirm_response", payload: { approve: true } }),
  env("ans", "gate", "client", "", { kind: "confirm_response", payload: { approve: true } }),
  env("n", "full", "notify", "text_in", { payload: { text: "Hello", final: true }, receipt: "sha256:abc" }),
];
const stamps = [1000, 1001, 1010, 1011, 1050, 1051, 1100, 1100, 1101, 1900, 1900, 1905];

test("the event log folds into a causal tree with streams collapsed", () => {
  const t = foldTrace(session, stamps);
  assert.equal(t.events, 11, "the duplicated confirm_response is one event");
  assert.equal(t.duration, 905);
  assert.deepEqual(t.traceIds, ["87363e9caaeb77838813c3a18b04d666"]);
  assert.deepEqual(t.receipts, ["sha256:abc"]);
  assert.deepEqual(t.usage, { input_tokens: 188, output_tokens: 76, cached_input_tokens: 180 });

  const shape = t.rows.map((r) => `${"  ".repeat(r.depth)}${r.kind} ${r.node}.${r.port} x${r.count}`);
  assert.deepEqual(shape, [
    "data client.text_out x1",
    "  data draft.text_in x1",
    "    data draft.text_out x3",
    "      data client.text_in x2",
    "    data draft.full_out x1",
    "      confirm_request client.confirm_in x1",
    "        confirm_response client. x1",
    "      data notify.text_in x1",
  ]);

  const stream = t.rows[2];
  assert.equal(stream.text, "Hello");
  assert.equal(stream.t0, 10);
  assert.equal(stream.t1, 100);
  assert.equal(stream.usage?.output_tokens, 76);
  assert.deepEqual(stream.ids, ["t1", "t2", "t3"]);
  assert.equal(t.rows[3].elidedBytes, 57);
  assert.equal(t.rows[5].note, "Approve delivery to notify.text_in?");
  assert.equal(t.rows[6].note, "approve");
  assert.equal(t.rows[7].receipt, "sha256:abc");
  assert.equal(t.rows[0].traceId, "87363e9caaeb77838813c3a18b04d666");
});

test("work logged after a cancel is marked suppressed, and only in that chain", () => {
  const events: Envelope[] = [
    env("a", undefined, "client", "text_out"),
    env("a1", "a", "llm", "text_in"),
    env("a2", "a1", "llm", "text_out", { payload: { text: "one", final: false } }),
    env("b", undefined, "client", "text_out"),
    { v: "1", id: "x", cause_id: "a", kind: "cancel" },
    env("a3", "a1", "llm", "text_out", { payload: { text: "two", final: false } }),
    env("b1", "b", "llm", "text_in"),
  ];
  const t = foldTrace(events);
  const by = (id: string) => t.rows.find((r) => r.ids.includes(id))!;
  assert.equal(by("a2").suppressed, false, "emitted before the cancel");
  assert.equal(by("a3").suppressed, true);
  assert.notEqual(by("a2").key, by("a3").key, "suppressed output is not folded into the delivered stream");
  assert.equal(by("b1").suppressed, false, "another chain is untouched");
  assert.equal(by("x").kind, "cancel");
  assert.equal(t.duration, 0, "no timestamps: order is known, timing is not");
});

test("errors, orphans and an empty log", () => {
  const t = foldTrace([
    env("a", "missing-cause", "llm", "text_out"),
    env("e", "a", "client", "text_in", { kind: "error", payload: { detail: "denied by user", state: "error" } }),
  ], [5, 9]);
  assert.equal(t.rows[0].depth, 0, "a cause outside the log makes a root, not a dropped row");
  assert.equal(t.rows[1].error, "denied by user");
  assert.equal(t.errors, 1);
  assert.deepEqual(foldTrace([]), { rows: [], duration: 0, events: 0, errors: 0, usage: null, traceIds: [], receipts: [] });
});

test("filtering keeps the ancestors of a match; collapsing hides descendants", () => {
  const t = foldTrace(session, stamps);
  const hit = filterTrace(t.rows, { query: "notify", kinds: [], errorsOnly: false });
  assert.deepEqual(hit.map((r) => `${r.node}.${r.port}`), [
    "client.text_out", "draft.text_in", "draft.full_out", "client.confirm_in", "notify.text_in",
  ]);
  assert.equal(filterTrace(t.rows, { query: "", kinds: ["confirm_response"], errorsOnly: false }).length, 5);
  assert.equal(filterTrace(t.rows, { query: "", kinds: [], errorsOnly: true }).length, 0);
  assert.equal(filterTrace(t.rows, { query: "", kinds: [], errorsOnly: false }).length, t.rows.length);

  const collapsed = visibleRows(t.rows, new Set([t.rows[1].key]));
  assert.deepEqual(collapsed.map((r) => r.key), ["in", "d"]);
});

test("trace ids come only from a well-formed traceparent", () => {
  assert.equal(traceIdOf(TP), "87363e9caaeb77838813c3a18b04d666");
  assert.equal(traceIdOf("garbage"), "");
  assert.equal(traceIdOf(undefined), "");
});

test("an edge crossing is an emission followed by the delivery it caused", () => {
  assert.equal(edgeCrossing(session[3], session[2]), "draft.text_out->client.text_in");
  assert.equal(edgeCrossing(session[2], session[1]), null, "same node: the Expertise working, not an edge");
  assert.equal(edgeCrossing(session[1], session[0]), "client.text_out->draft.text_in");
  assert.equal(edgeCrossing(session[0], undefined), null);
  assert.equal(edgeCrossing(session[8], session[7]), null, "a gate question is not traffic on an edge");
});

/* ── routing ────────────────────────────────────────────────────── */

test("routes round-trip, including ids with slashes", () => {
  assert.deepEqual(parseRoute("#/sessions/sess-1/01ABC"), { view: "sessions", args: ["sess-1", "01ABC"] });
  const built = buildRoute("expertise", "demo/cognitive/draft");
  assert.equal(built, "#/expertise/demo%2Fcognitive%2Fdraft");
  assert.deepEqual(parseRoute(built), { view: "expertise", args: ["demo/cognitive/draft"] });
  assert.equal(buildRoute("studio", undefined, ""), "#/studio");
  assert.equal(buildRoute("evidence", "entry", 7), "#/evidence/entry/7");
});

test("anything that is not a route lands on the default", () => {
  assert.deepEqual(parseRoute(""), DEFAULT_ROUTE);
  assert.deepEqual(parseRoute("#token=abc"), DEFAULT_ROUTE, "the node token is not a route");
  assert.deepEqual(parseRoute("#/nope/x"), DEFAULT_ROUTE);
  assert.deepEqual(parseRoute("#/skills"), { view: "expertise", args: [] }, "the old name still resolves");
  assert.deepEqual(parseRoute("#/sessions/%E0%A4%A"), { view: "sessions", args: ["%E0%A4%A"] }, "bad escapes do not throw");
  assert.ok(sameRoute(parseRoute("#/pulse"), DEFAULT_ROUTE));
  assert.ok(!sameRoute({ view: "sessions", args: ["a"] }, { view: "sessions", args: ["b"] }));
});

/* ── palette ────────────────────────────────────────────────────── */

const commands = [
  { id: "go.expertise", label: "Go to Expertise", group: "Navigate" },
  { id: "go.sessions", label: "Go to Sessions", group: "Navigate" },
  { id: "graph.echo", label: "Open graph echo", group: "Graphs", keywords: "studio canvas" },
  { id: "theme", label: "Switch to light theme", group: "Appearance" },
  { id: "verify", label: "Verify the ledger", group: "Evidence", keywords: "chain audit" },
];

test("palette: empty query lists everything in the order given", () => {
  assert.deepEqual(filterCommands(commands, "").map((c) => c.id), commands.map((c) => c.id));
  assert.equal(filterCommands(commands, "  ", 2).length, 2);
});

test("palette: word starts and contiguous runs win", () => {
  assert.equal(filterCommands(commands, "ses")[0].id, "go.sessions");
  assert.equal(filterCommands(commands, "exp")[0].id, "go.expertise");
  assert.equal(filterCommands(commands, "og")[0].id, "graph.echo");
  assert.equal(filterCommands(commands, "light")[0].id, "theme");
  assert.deepEqual(filterCommands(commands, "zzz"), []);
});

test("palette: keywords match, but below a label match", () => {
  assert.deepEqual(filterCommands(commands, "audit").map((c) => c.id), ["verify"]);
  assert.equal(filterCommands(commands, "canvas")[0].id, "graph.echo");
  assert.equal(score("abc", "xyz"), null);
  assert.ok((score("ver", "Verify the ledger") ?? 0) > (score("ver", "Go to Sessions never") ?? 0));
});
