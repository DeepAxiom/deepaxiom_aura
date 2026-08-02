import assert from "node:assert/strict";
import test from "node:test";

import { envelope, newId, newSessionId, PROTOCOL_MAJOR } from "../dist/index.js";

test("newId is 26 Crockford base32 characters", () => {
  const id = newId();
  assert.equal(id.length, 26, "10 timestamp + 16 random");
  assert.match(id, /^[0-9A-HJKMNP-TV-Z]{26}$/, "Crockford base32 excludes I, L, O and U");
});

test("newId is unique across many calls", () => {
  const seen = new Set();
  for (let i = 0; i < 5000; i++) seen.add(newId());
  assert.equal(seen.size, 5000);
});

// C3 calls the id lexicographically sortable, which is what lets an event log
// be ordered by id alone. The timestamp prefix is what makes that true.
test("newId timestamp prefixes are monotonic", async () => {
  const first = newId().slice(0, 10);
  await new Promise((r) => setTimeout(r, 5));
  const second = newId().slice(0, 10);
  assert.ok(second >= first, `${second} should sort at or after ${first}`);
});

test("envelope fills in protocol and id", () => {
  const env = envelope({ kind: "data" });
  assert.equal(env.v, PROTOCOL_MAJOR);
  assert.equal(env.kind, "data");
  assert.equal(env.id.length, 26);
});

test("envelope keeps the fields it is given", () => {
  const env = envelope({ kind: "cancel", cause_id: "abc", session: "sess-1" });
  assert.equal(env.cause_id, "abc");
  assert.equal(env.session, "sess-1");
});

test("newSessionId looks like a kernel session id", () => {
  assert.match(newSessionId(), /^sess-[0-9a-f]{12}$/);
});
