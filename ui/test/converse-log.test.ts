/**
 * The conversation fold.
 *
 * Worth its own file because merging Chat and Voice into one panel merged two
 * streaming disciplines that look alike and are not: an ASR transcript revises
 * itself, a model's tokens continue. These tests are what stops a later
 * simplification from collapsing the two into one branch.
 */

import assert from "node:assert/strict";
import { test } from "node:test";

import { fold, spoken, type Line, type Vocab } from "../src/converse/log.ts";
import type { Envelope } from "../src/api/types.ts";

const vocab: Vocab = {
  sessionReady: (session, graph) => `ready ${session} ${graph}`,
  approve: "Approve?",
  unknownError: "error",
};

let n = 0;
function env(e: Partial<Envelope>): Envelope {
  return { v: "1", id: `e${++n}`, kind: "data", ...e } as Envelope;
}

const run = (envs: Partial<Envelope>[], start: Line[] = []) =>
  envs.reduce<Line[]>((acc, e) => fold(acc, env(e), vocab), start);

test("a model's tokens append into one growing reply", () => {
  const lines = run([
    { kind: "data", port: "text_in", payload: { text: "Ranked" } },
    { kind: "data", port: "text_in", payload: { text: " by" } },
    { kind: "data", port: "text_in", payload: { text: " fit", final: true } },
  ]);
  assert.equal(lines.length, 1, "three tokens are one reply, not three bubbles");
  assert.equal(lines[0].kind, "aura");
  assert.equal((lines[0] as { text: string }).text, "Ranked by fit");
  assert.equal((lines[0] as { open: boolean }).open, false, "final closes the bubble");
});

test("an ASR transcript replaces its own hypothesis instead of appending", () => {
  const lines = run([
    { kind: "data", port: "transcript_in", payload: { text: "recomm", final: false } },
    { kind: "data", port: "transcript_in", payload: { text: "recommend", final: false } },
    { kind: "data", port: "transcript_in", payload: { text: "recommend a model", final: true } },
  ]);
  assert.equal(lines.length, 1);
  assert.equal(
    (lines[0] as { text: string }).text,
    "recommend a model",
    "appending here would render 'recommrecommendrecommend a model'",
  );
  assert.equal((lines[0] as { partial?: boolean }).partial, false);
});

test("a settled utterance does not absorb the next one", () => {
  const lines = run([
    { kind: "data", port: "transcript_in", payload: { text: "first", final: true } },
    { kind: "data", port: "transcript_in", payload: { text: "second", final: false } },
  ]);
  assert.equal(lines.length, 2, "replacement is scoped to the hypothesis still in flight");
});

test("audio chunks are played, never written down", () => {
  const before: Line[] = [];
  const after = fold(before, env({ kind: "data", port: "audio_in", payload: { pcm_b64: "AAAA" } }), vocab);
  assert.equal(after, before, "same reference: a caller can skip the render entirely");
});

test("a non-text payload is shown as JSON rather than guessed at", () => {
  const lines = run([{ kind: "data", port: "result_in", payload: { ranked: [{ model: "qwen" }] } }]);
  assert.equal(lines[0].kind, "result");
  assert.match((lines[0] as { json: string }).json, /"ranked"/);
});

test("done closes an open reply and is a no-op otherwise", () => {
  const open = run([{ kind: "data", port: "text_in", payload: { text: "hi" } }]);
  const closed = fold(open, env({ kind: "done" }), vocab);
  assert.equal((closed[0] as { open: boolean }).open, false);
  assert.equal(fold(closed, env({ kind: "done" }), vocab), closed, "nothing open, nothing to close");
});

test("a gate becomes a card that names the request it answers", () => {
  const lines = run([{ kind: "confirm_request", payload: { question: "Send the email?" } }]);
  assert.equal(lines[0].kind, "gate");
  const gate = lines[0] as { requestId: string; key: string };
  assert.equal(gate.requestId, gate.key, "the reply has to name the request; losing the id strands the session");
});

test("an error carries its detail through", () => {
  const lines = run([{ kind: "error", payload: { detail: "no skill satisfies cognitive.llm.chat" } }]);
  assert.equal((lines[0] as { text: string }).text, "no skill satisfies cognitive.llm.chat");
});

test("what you type is recorded locally, because the kernel will not echo it", () => {
  const lines = spoken([], "rank which models fit");
  assert.equal(lines.length, 1);
  assert.equal(lines[0].kind, "user");
});

test("a typed turn and the reply to it stay two lines", () => {
  const lines = run(
    [{ kind: "data", port: "text_in", payload: { text: "sure" } }],
    spoken([], "hello"),
  );
  assert.deepEqual(lines.map((l) => l.kind), ["user", "aura"]);
});
