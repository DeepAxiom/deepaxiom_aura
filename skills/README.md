# Reference skills — demos, not a catalogue

Everything in this directory is a **demo**. These nine skills exist to show how a
skill is written, not to be the set of skills Deep Axiom offers, and not to be
depended on in production. Their manifests carry the `example/` org for exactly
that reason: `example/motor/tts` is a demonstration of a `motor` skill, not a
product line.

Working is not the same as finished, and several of these do work — `postgres-cdc`
really does read a replication stream, `llm-chat` really does run a local GGUF.
What none of them carries is the retry policy, credential rotation, schema-change
handling and failure budget a deployment needs, because those are decisions your
deployment makes and an example cannot make them for you. Copy the closest one
and replace it; do not extend it and ship it.

A skill is any process that speaks C3 over a WebSocket and declares a C1
manifest. That is the whole contract. It can be Python, TypeScript, Go, Rust, a
Wasm module, or a wrapper around software you already run — anything from a
sentiment classifier to a warehouse robot controller to your company's billing
system. Nothing in the kernel knows or cares which.

So the useful question is not "which skills does this ship with" but "how long
does it take to write the one you need". These examples exist to answer that.

| Example | Why it is here |
|---|---|
| [`echo/`](echo/) | **Start here.** The smallest possible skill — no model, no network, no credentials. ~100 lines. |
| [`llm-chat/`](llm-chat/) | A `cognitive` skill that streams token by token and keeps per-session state. |
| [`asr/`](asr/) | A `sensorial` skill turning a live audio stream into partial transcripts. |
| [`tts/`](tts/) | A `motor` skill emitting a paced PCM stream, with a degradation chain when the preferred backend is missing. |
| [`sentence-chunker/`](sentence-chunker/) | A `logical` skill: pure stream transformation, no I/O at all. |
| [`memory-context/`](memory-context/) | A `memory` skill that survives restarts and trims to a token budget. |
| [`planner/`](planner/) | Reads the live catalogue and compiles a goal into an executable graph. |
| [`model-manager/`](model-manager/) | A `motor` skill that acts on the world — so every call through it is gated and sealed. |
| [`postgres-cdc/`](postgres-cdc/) | Wrapping an external system: logical replication becomes causal events. |

The five types (`sensorial`, `cognitive`, `motor`, `memory`, `logical`) are the
whole taxonomy — see [C1](../spec/c1-manifest.md). The type is not decoration:
`motor` is what makes the kernel demand approval before a call and seal the
result.

To write your own, copy [`echo/`](echo/) and change the manifest. The SDK is
Apache-2.0, so a skill you write and sell carries no copyleft obligation, ever.
See [Writing a skill](../GUIDE.md#writing-a-skill-the-sdk).
