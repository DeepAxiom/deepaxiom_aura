# Deep Axiom

### Automation that never stops running — and can prove what it did.

n8n, Zapier and Make fire a trigger, run a chain of steps once, and finish. That
fits a nightly sync. It falls apart the moment the work is *live*: a
conversation, a video feed, a database changing under you, a model answering
token by token.

Deep Axiom keeps the connection open. Skills — LLMs, speech, databases, business
APIs — are wired into graphs that run as typed, causal, back-pressured streams.
Three properties hold it together:

- **Streaming** — the connection is the unit of work, not the run. 0.42 ms per hop.
- **Concurrency** — throughput *rises* with load. 1,000 concurrent sessions, measured.
- **Auditable** — every effect authorized by policy, signed for by a named
  human, and sealed into a hash-chained ledger that verifies offline, anchored
  where a third party can already be watching. In the kernel, not in your graph.

One binary. 23 MB. No account, no cloud, no Postgres, no broker, no cluster.

[Full guide](GUIDE.md) · [Versión en español](README-ES.md) · [Roadmap](ROADMAP.md) ·
**v0.3.0 — pre-1.0, pre-production**

---

## 60 seconds

```bash
git clone https://github.com/deepaxiom/aura && cd aura/kernel
go build -o aura ./cmd/aura     # Go 1.25+, no CGO, no external services
./aura up                       # UI, local LLM, state store, ledger — all of it
```

```bash
aura chat "explain backpressure"    # streams token by token
aura do "watch the orders table and text me when a refund over $500 lands"
```

That second command reads the live catalogue, picks skills, compiles a graph and
runs it — with the write gated because it acts on the world. The graph stays up:
Postgres pushes row changes as they commit, and nothing polls.

---

## 1 · Streaming

|  | Batch tools | Deep Axiom |
|---|---|---|
| Unit of work | A run: starts, executes, tears down | A **connection** that stays open |
| Getting data | Poll every 5 minutes | The source **pushes**, as it happens |
| An LLM answering | Wait for the whole reply | Token by token; downstream reacts mid-sentence |
| Audio / video | Not really supported | Paced PCM, partial transcripts, working barge-in |
| A lost packet | Stalls everything behind it (TCP) | Stalls only its own lane (QUIC) |
| Cancelling | Best effort | Kernel guarantee — a cancelled chain's output goes nowhere |

Text, audio, documents and events all travel as the same typed envelope. A node
serves **QUIC (WebTransport)** on the same port number as its TCP listener, and
C3's three QoS classes become three real transport primitives:

| Class | On the wire |
|---|---|
| `realtime` | QUIC datagram — cannot stall, or be stalled by, another frame |
| `reliable` | One ordered stream — FIFO per edge |
| `bulk` | Its own stream per transfer |

That closed a gap the runtime carried quietly: dropping the oldest frame in a
queue answers a *slow consumer* and does nothing for a *lossy network*, because
TCP redelivers in order underneath. It also gives `bulk` a meaning for the first
time. A peer that cannot reach UDP keeps the WebSocket path unchanged.

**Deadlines are absolute and inherited** — a hop may tighten one, never extend
it. **Speculative execution** runs downstream work on partial output and is
refused at wiring time on any edge into a `motor` skill.

---

## 2 · Concurrency

Every envelope is durably logged before it is acknowledged. That used to mean
one transaction per event and a queue at a single write lock — 200 concurrent
sessions did *less* total work than one. Group commit (the deal PostgreSQL and
RocksDB have made for decades) keeps each caller waiting for its own durability,
but everyone else already waiting joins the same transaction.

Measured end to end on one developer machine, Go client and Go skill so the
harness is not the limit:

| Concurrent sessions | Before | After |
|---|---|---|
| 1 | 1,556 msg/s · p50 0.47 ms | 1,504 msg/s · p50 0.58 ms |
| 50 | 365 msg/s · p50 96 ms | 1,631 msg/s · p50 19 ms |
| 200 | 344 msg/s · p50 489 ms | **1,908 msg/s · p50 63 ms** |
| 400 *(8 replicas)* | — | **11,458 msg/s · p50 0.0 ms** |
| 1,000 *(8 replicas)* | *would not connect* | **28,202 msg/s · p50 0.51 ms** |

A CPU profile found the rest, and it was not where anyone guessed: JSON encoding
was 1% of CPU, while **SQLite's file I/O was 54%**. Two fixes came out of it.
Raising SQLite's WAL checkpoint threshold cut `FlushFileBuffers` from 38% of all
CPU to 13%. Then the causal event log moved out of SQLite entirely.

**The event log is not a table.** It is written once per envelope and never
updated or deleted from, and read back as one session in order — the best case
for a log and the worst for a B-tree paying for updates that never happen. It is
now an append-only segment file with a CRC per record, group-committed writes,
and an index rebuilt from the file on startup; a torn tail from a power cut is
detected and truncated rather than read as data. On the same workload at matched
durability it sustained 1.5M events/sec against SQLite's 27k. The shape is the
one [Tidehunter](https://arxiv.org/abs/2602.01873) argues for: treat the log as
permanent storage, and compaction stops existing because nothing is relocated.

The effect ledger deliberately stays in SQLite. A bug in the event log loses
replay history; a bug in the ledger loses evidence.

Two things beyond the batching: session bookkeeping joined the same batch, and
resolution now spreads sessions across replicas of a skill — ten copies used to
leave nine idle. A panic while routing now fails one session instead of the node.

**What this is not.** Per-node numbers on one machine; a node is still one
process with no failover. Twitch scale means tens of thousands of connections
per machine across hundreds of machines — and platforms at that scale do not
seal every event into a hash chain. This does, deliberately. That is the cost of
pillar 3, and it is the one this runtime will not trade away.
`kernel/cmd/loadgen/` reproduces the table.

---

## 3 · Auditable

None of this lives in your graph:

- **The approval gate is a kernel invariant.** In agent libraries the interrupt
  lives in the code you wrote, so code that forgets it has no gate. Here the
  executor applies it at the one point every delivery passes through, driven by
  node policy. A graph may ask for *more* scrutiny than policy requires, never less.
- **The entry names the human who approved it, and they signed it.** Not "a
  human approved" — *which* human, provably. The operator signs a statement
  bound to that one delivery with a key the node has never held, so the
  approver cannot deny it afterward and the node cannot fabricate one. That is
  the half of *"who authorized this"* every audit trail skips, because the
  usual answer — the node's own word — is worth nothing when the node is what
  is under review.
- **Every effect is attested, not logged.** Sealed into a hash-chained record
  committed to an RFC 6962 Merkle head the node signs, which a third party can
  counter-sign. `aura verify` recomputes chain, tree and signatures from the
  database file alone, with no kernel running.
- **And the third party is accountable too.** A witness publishes its own
  append-only log, signs its head, and signs its answer to *how far have you
  vouched for this node* — so telling one party one thing and another something
  else stops being undetectable and becomes two signed statements that cannot
  both be true. `aura witness audit` follows one and refuses it if it has
  rewritten anything. Nodes anchor at a free public witness by default and can
  point anywhere else with one flag; a receipt is worth what it is worth to
  whoever is already following the same anchor.
- **It cites the model that argued for it.** A skill running inference attests to
  engine, model, revision, quantization, sampling parameters and seed — bound
  into every effect that output caused. A silent model swap changes hashes
  already committed to an append-only chain.
- **`aura undo`** reverses an effect through a declared compensation port. The
  undo is itself gated and sealed.

**Already running agents?** `aura guard` puts the MCP servers your agent already
calls behind this same checkpoint — one line in the config you have, no rewrite.
A tool is gated unless its server proves it only reads *and* you chose to
believe it. It holds exactly as far as your control over the agent's config
does; there is no network enforcement.
[Details](GUIDE.md#guarding-an-agents-tools).

**And when config control is not enough**, stop trying to make the bypass
impossible and make it useless. `aura secret set` puts the credential in the
kernel instead of the agent's environment, and it is released only against the
receipt of an effect that just passed the checkpoint — the right capability,
delivered not denied, seconds old. Skipping the gate no longer avoids scrutiny;
it gets you a 401. What it does not do is stop a skill from keeping a
credential it legitimately received. What it removes is the standing, ambient
token that was available for every call an agent ever made, gated or not.
[Details](GUIDE.md#the-credential-broker).

**Shipping a new model?** `aura regress` replays your recorded sessions against
it and diffs the *effects*, not the transcripts — so "the wording changed" and
"it stopped issuing the refund" are no longer the same result. An eval suite
scores outputs against a rubric and cannot see an act that stopped happening;
this can, because C5 already binds the model revision to the act.
[Details](GUIDE.md#regression-testing-against-the-ledger).

---

## Connecting what you already have

```bash
aura connect --openapi ./crm.yaml   # every operation becomes a skill
```

Read-only by default, with dry-run and per-operation promotion before anything
writes. Webhooks come in, `sensorial` skills wrap systems that push, and
inversion-of-control skills dial *out* so they run behind NAT with no inbound
ports. At the borders: an **MCP server** (every skill is a tool for Claude Code
or Cursor, and `tools/call` streams), an A2A discovery card, and OpenTelemetry
export of the causal tree.

An MCP tool call comes back with the receipts for what it did — capability,
decision, outcome, and the human who signed for it — in `_meta`, so the evidence
travels with the action instead of in a log somebody has to correlate later.
That shape is written up as
[a proposal to MCP](spec/proposals/mcp-effect-receipts.md): one optional field,
no change to the transport, and the alternative is every vendor inventing its
own namespace and an auditor reconciling five formats by timestamp.

The nine skills in [`skills/`](skills/) are **worked examples, not a
catalogue** — hence the `example/` org. A skill is any process that speaks the
channel protocol and declares a manifest: Python, TypeScript, Go, Rust, Wasm, or
a wrapper around software you already run. Copy [`skills/echo/`](skills/echo/),
about 100 lines. The SDK is Apache-2.0, so a skill you write and sell carries no
copyleft obligation, ever.

---

## Where it honestly stands

| | |
|---|---|
| **Tested** | Streaming envelopes with per-edge QoS over WebSocket and QUIC · the ledger and offline verification · the gate as a kernel invariant · **signed approver identity sealed into the entry** · **the credential broker (a secret only against a valid receipt)** · **the witness's own published log, and a monitor that catches one rewriting it** · cancellation · session resume · deterministic replay · **effect-level regression across sessions** · typed ports given compiled decoding grammars · the MCP border both ways · `aura guard` · Wasm skills in a real sandbox · Postgres CDC |
| **Hand-verified** | Voice with barge-in · the planner (`aura do`) · `aura why` · OpenTelemetry export · ML-BOM |
| **Not there yet** | No multi-device view of one live session · no failover if the node dies · **no process isolation for `format: source` skills** — a skill runs with the privileges of whoever started it |

`internal/` sits at 72% test coverage, `cmd/aura` at 9.5%, the UI has none. A
59-check conformance suite runs the kernel over the wire, and a separate CI job
proves the ledger detects tampering by editing a real database behind a real
binary's back. Read [Security model](GUIDE.md#security-model) before you expose
a port, and [Milestone status](GUIDE.md#milestone-status) for the full line
between tested and hand-verified. This is pre-production; treat it that way.

---

**License.** The spec and SDKs are Apache-2.0 — build a conformant kernel, write
and sell skills, no copyleft obligation ever. That includes the parts a second
implementation would need most: the ledger contract, the witness protocol, and a
59-check conformance suite to demonstrate compliance with. A verifier is worth
less the fewer things it can verify, and an anchor is worth less the fewer
parties present to it — neither is a good thing to own. The kernel and UI are
AGPLv3, with a commercial license available instead. Running `aura`
unmodified — your laptop, your servers, inside your company — triggers no AGPL
obligation. See [LICENSE.md](LICENSE.md) · Contributions:
[CONTRIBUTING.md](CONTRIBUTING.md).
