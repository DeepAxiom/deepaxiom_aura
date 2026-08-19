/**
 * Credential resolution.
 *
 * A node binds loopback and mints a bearer token by default, and `/ws/skill` is
 * behind it like every other route — so this is what stands between the six-line
 * integration in the README and a skill that retries forever against its own
 * node. It went out unimplemented once; these pin the behaviour down.
 */

import assert from "node:assert/strict";
import test from "node:test";

import { createNode } from "../dist/index.js";

/** Reach the private resolution without exporting it just for a test. */
const authorizedUrl = (node) => node.authorizedUrl();
const setToken = (node, token) => {
  node.token = token;
};

function node(options = {}) {
  return createNode({ org: "acme", app: "shop", ...options });
}

test("an explicit token wins over the environment", async () => {
  const previous = process.env.AURA_TOKEN;
  process.env.AURA_TOKEN = "from-env";
  try {
    const n = node({ token: "explicit" });
    assert.equal(await n.resolveToken(), "explicit");
  } finally {
    if (previous === undefined) delete process.env.AURA_TOKEN;
    else process.env.AURA_TOKEN = previous;
  }
});

test("AURA_TOKEN is read when no token is passed", async () => {
  const previous = process.env.AURA_TOKEN;
  process.env.AURA_TOKEN = "  from-env  ";
  try {
    assert.equal(await node().resolveToken(), "from-env", "and it is trimmed");
  } finally {
    if (previous === undefined) delete process.env.AURA_TOKEN;
    else process.env.AURA_TOKEN = previous;
  }
});

test('an explicit "" means send nothing, for a --no-auth node', async () => {
  const previous = process.env.AURA_TOKEN;
  process.env.AURA_TOKEN = "from-env";
  try {
    const n = node({ token: "" });
    assert.equal(await n.resolveToken(), "", "an empty token is a choice, not an absence");
  } finally {
    if (previous === undefined) delete process.env.AURA_TOKEN;
    else process.env.AURA_TOKEN = previous;
  }
});

test("the token rides as a query parameter, since WebSocket takes no headers", () => {
  const n = node({ url: "ws://localhost:9080/ws/skill" });
  setToken(n, "abc123");
  assert.equal(authorizedUrl(n), "ws://localhost:9080/ws/skill?token=abc123");
});

test("a URL that already has a query gets the parameter appended", () => {
  const n = node({ url: "ws://localhost:9080/ws/skill?x=1" });
  setToken(n, "abc");
  assert.equal(authorizedUrl(n), "ws://localhost:9080/ws/skill?x=1&token=abc");
});

test("a URL that already carries a token is left alone", () => {
  const url = "ws://localhost:9080/ws/skill?token=already";
  const n = node({ url });
  setToken(n, "other");
  assert.equal(authorizedUrl(n), url, "an explicitly built endpoint still wins");
});

test("no token means the URL is untouched", () => {
  const url = "ws://localhost:9080/ws/skill";
  const n = node({ url });
  setToken(n, "");
  assert.equal(authorizedUrl(n), url, "a --no-auth node must not receive an empty token=");
});

test("the token is percent-encoded", () => {
  const n = node({ url: "ws://localhost:9080/ws/skill" });
  setToken(n, "a+b/c=");
  assert.equal(authorizedUrl(n), "ws://localhost:9080/ws/skill?token=a%2Bb%2Fc%3D");
});
