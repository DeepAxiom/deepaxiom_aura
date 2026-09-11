# Deep Axiom

### Deploy assistants and automations that run live — and can prove what they did.

[Download v0.3.0](https://github.com/DeepAxiom/deepaxiom_aura/releases/tag/v0.3.0) · [Quick start](QUICKSTART.md) · [Full guide](GUIDE.md) ·
[Versión en español](README-ES.md) · [Milestone status](GUIDE.md#milestone-status) ·
[Roadmap](ROADMAP.md) · **v0.3.0 — pre-1.0, pre-production**

---

## Start without adopting anything

Your agent already calls MCP servers. Put them behind a checkpoint with one
command — no runtime to stand up, no port to pick, no rewrite:

```bash
aura guard --config claude_desktop_config.json
```

```
  no node on port 9080 — running an embedded kernel
  ledger    every guarded call is sealed here

  TOOL               CAPABILITY                   ON CALL
  probe/read_thing   motor.mcp.probe.read_thing   human approval + sealed
  probe/write_thing  motor.mcp.probe.write_thing  human approval + sealed

  2 of 2 act on the world and are gated; the rest are read-only.
```

Every tool call the agent makes now passes a policy you control, stops for a
human if it acts on the world, and lands in a hash-chained ledger that verifies
offline. A tool is gated unless its server proves it only reads **and** you chose
to believe it — the default is strict because `readOnlyHint` is a claim by the
same server the call is about, and tool metadata is the documented attack surface
[[1]](#refs)[[2]](#refs): a large-scale study of the MCP ecosystem finds
descriptor-level poisoning to be the most prevalent client-side vulnerability,
and most clients validate it insufficiently.

The catch, stated up front: this holds exactly as far as your control over the
agent's config does. There is no network enforcement.
[Details](GUIDE.md#guarding-an-agents-tools).

---

## 1 · Deploy in real time

Once you want more than a checkpoint, the same binary is a runtime. Describe what
you need; it reads the live catalogue, picks skills, compiles a graph and leaves
it running:

```bash
aura do "watch the orders table and text me when a refund over $500 lands"
```

The write is gated because it acts on the world. The graph stays up: Postgres
pushes row changes as they commit, and nothing polls.

**The connection is the unit of work, not the run.** n8n, Zapier and Make fire a
trigger, run a chain once, and finish. That fits a nightly sync and falls apart
the moment the work is *live* — a conversation, a video feed, a database changing
under you, a model answering token by token.

|  | Batch tools | Deep Axiom |
|---|---|---|
| Unit of work | A run: starts, executes, tears down | A **connection** that stays open |
| Getting data | Poll every 5 minutes | The source **pushes**, as it happens |
| An LLM answering | Wait for the whole reply | Token by token; downstream reacts mid-sentence |
| Audio / video | Not really supported | Paced PCM, partial transcripts, working barge-in |
| A lost packet | Stalls everything behind it (TCP) | Stalls only its own lane (QUIC) |
| Cancelling | Best effort | Kernel guarantee — a cancelled chain's output goes nowhere |

Text, audio, documents and events travel as the same typed envelope. A node
serves **QUIC (WebTransport)** on the same port number as its TCP listener, so
the three QoS classes become three real transport primitives: `realtime` is a
datagram that cannot stall or be stalled, `reliable` is one ordered stream per
edge, `bulk` gets its own stream. A peer that cannot reach UDP keeps the
WebSocket path unchanged. **Deadlines are absolute and inherited** — a hop may
tighten one, never extend it.

A dropped socket resumes: the dedup window, causal indexes, pending gates and
per-hop counters rebuild from the event log, whether the client or the kernel was
what died. That is the property the stateful-dataflow literature calls failure
transparency [[3]](#refs), and it is the one an assistant that is *live* cannot
do without — there is no "re-run the job" for a conversation.

### Live means concurrent, and concurrency has to help

Every envelope is durably logged before it is acknowledged. That used to mean one
transaction per event queued at a single write lock — 200 concurrent sessions did
*less* total work than one. Group commit (the deal PostgreSQL and RocksDB have
made for decades) keeps each caller waiting for its own durability while everyone
already waiting joins the same transaction.

| Concurrent sessions | Before | After |
|---|---|---|
| 1 | 1,556 msg/s · p50 0.47 ms | 1,504 msg/s · p50 0.58 ms |
| 50 | 365 msg/s · p50 96 ms | 1,631 msg/s · p50 19 ms |
| 200 | 344 msg/s · p50 489 ms | **1,908 msg/s · p50 63 ms** |
| 1,000 *(8 replicas)* | *would not connect* | **28,202 msg/s · p50 0.51 ms** |

Read the table for the shape, not the absolute numbers, and know what it
measures: an echo skill over loopback, so it is the kernel's routing and
durability cost with no real work in the loop. Aggregate throughput divides
round-trips by wall-clock *including* dial time, which flatters the
high-concurrency rows. The honest claim is the middle row's: at 200 sessions the
same node went from 344 msg/s to 1,908 and p50 from 489 ms to 63, without
weakening durability. `kernel/cmd/loadgen/` reproduces it.

A CPU profile found the rest, and not where anyone guessed: JSON encoding was 1%
of CPU while **SQLite's file I/O was 54%**. The causal event log moved out of
SQLite into append-only segment files with a CRC per record and group-committed
writes — 1.5M events/sec against SQLite's 27k at matched durability, the shape
[Tidehunter](https://arxiv.org/abs/2602.01873) argues for. It rotates at 128 MiB;
`--event-log-max` reclaims whole segments oldest-first. The effect ledger stays
in SQLite deliberately: a bug in the event log loses replay history, a bug in the
ledger loses evidence.

---

## 2 · Auditable — because it is already specified

The obligation is written and dated. The date moved; the text did not.

**[Regulation (EU) 2024/1689](https://artificialintelligenceact.eu/article/12/)
— the EU AI Act — requires this of high-risk AI systems from 2 December 2027.**
That deadline was 2 August 2026 until
[Regulation (EU) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj), the
Digital Omnibus on AI, deferred it — Annex III stand-alone systems to 2 December
2027, Annex I embedded systems to 2 August 2028. In force since 27 July 2026.

**What the Omnibus moved was the calendar, not the requirement.** Articles 12 and
14 survive the amendment with their substance intact, and they are directly about
what a runtime has to emit:

- **Article 12** requires *automatic* recording of events over the system's
  lifetime, serving risk identification (Art. 79), post-market monitoring
  (Art. 72) and deployer oversight (Art. 26(5)). Deployers must retain those
  logs for at least six months.
- **Article 14** requires that the system be effectively overseen by **natural
  persons** while in use.

So this README will not tell you the sky is falling in a fortnight. The argument
for building the evidence path now is narrower and, we think, better: **a system
that was not designed to emit this evidence cannot be made to emit it later
without rebuilding how it executes.** An audit trail is a property of the
execution path, not a feature bolted to its side — which is the whole reason the
gate below is a kernel invariant rather than a library call. Retrofitting that
into a running product is the expensive version of this work, and the deferral is
the window in which the cheap version is still available.

**And one clock did not move.** **ISO/IEC 42001** (clause 9.2) wants the same
evidence chain for internal audit and is in force today, certifiable now, and
increasingly a procurement precondition rather than a regulatory one. The
harmonised standards that will operationalise Article 12 — prEN 18229-1,
ISO/IEC DIS 24970 — are still drafts, which means the shape of the evidence is
being decided during the deferral rather than settled before it.

Here is what that produces, and none of it lives in your graph:

- **The approval gate is a kernel invariant.** In agent libraries the interrupt
  lives in the code you wrote, so code that forgets it has no gate. Here the
  executor applies it at the one point every delivery passes through, driven by
  node policy. A graph may ask for *more* scrutiny than policy requires, never
  less — and where policy does let a graph waive a gate, the entry records that
  the *graph* excused it rather than the node, because a graph is a JSON document
  anyone reaching the control surface may register.
- **The entry names the human who approved it, and they signed it.** Article 14
  asks for oversight by a natural person; a log saying "a human approved" does
  not evidence one. The operator signs a statement bound to that one delivery
  with a key the node has never held, so the approver cannot deny it afterward
  and the node cannot fabricate one. That signature can also cover **what they
  were shown** — digests of the rendered document, hashed on the approver's
  machine, so the record says a person consented to *this* text and not merely
  that somebody clicked yes on an identifier. Set `require_approval_context` and
  an answer that binds nothing is a denial. That is the half of
  *"who authorized this"* every audit trail skips — including the IETF's own
  [agent audit-trail draft](https://datatracker.ietf.org/doc/draft-sharif-agent-audit-trail/),
  which records a pseudonymous operator id with no signature and signs records
  with the *agent's* key. We wrote the fix up as an
  [Internet-Draft](spec/proposals/draft-signed-human-approval.md).
  A caveat worth reading before trusting any gate, ours included: the reviewer is
  not an infinitely available oracle, and calibrating *which* actions to stop
  against a subjective, fatiguing human is an open problem [[4]](#refs). A gate
  that fires too often is a gate that gets rubber-stamped.
- **A skill is not the operator.** `aura token issue --capability motor.erp.write`
  mints a credential that may connect, register as *that* capability and spend
  the receipts it is handed — and cannot register a graph, read the ledger or
  enrol an approver. One ordered table decides what each scope reaches, deny by
  default.
- **A skill's credential is released against a receipt, not held ambiently.**
  `aura secret set` puts the credential in the kernel; it is released only
  against the receipt of an effect that just passed the checkpoint. Skipping the
  gate stops being a way to avoid scrutiny and becomes a way to get a 401. The
  same capability-sealed shape is what the confidential-computing-for-agents
  literature converges on [[5]](#refs)[[6]](#refs) — a compromised agent or a
  leaked prompt should never see a raw key.
- **Every effect is attested, not logged.** Sealed into a hash-chained record
  committed to an RFC 6962 Merkle head the node signs, which a third party can
  counter-sign. `aura verify` recomputes chain, tree and signatures from the
  database file alone, with no kernel running. Disks fill up: by default an
  effect the node cannot seal is delivered and the gap logged loudly, and a node
  where the ledger must be complete sets `on_seal_failure: refuse` instead. The
  startup banner says which is in force. Applying Certificate Transparency's
  construction to agent execution is a converging idea, not ours alone
  [[7]](#refs)[[8]](#refs).
- **And the third party is accountable too.** A witness publishes its own
  append-only log, signs its head, and signs its answer to *how far have you
  vouched for this node* — so telling one party one thing and another something
  else becomes two signed statements that cannot both be true. The underlying
  argument — that an authority which can be caught equivocating needs no trust —
  is a decade old and still the right one [[9]](#refs).
- **It cites the model that argued for it, and says how far to believe it.** A
  skill attests to engine, model, revision, quantization, sampling parameters and
  seed, bound into every effect that output caused. Silent model substitution is
  a documented, measurable problem in deployed LLM APIs [[10]](#refs), and this
  makes it break hashes already committed to an append-only chain. But it is
  still the skill's own word. Where hardware can narrow it, the TEE quote's nonce
  **is** the hash of that exact declaration — so the evidence is about *this*
  record rather than beside it, and editing the declaration afterwards breaks it.
  A verifier reports `none`, `bound` or `verified`, never a boolean. The cost is
  now tolerable — 4–8% throughput on H100 confidential compute, shrinking with
  batch size [[11]](#refs) — which is why the field is moving and why the field's
  own surveys are worth reading before believing any vendor's claim.
- **`aura undo`** reverses an effect through a declared compensation port. The
  undo is itself gated and sealed.

**When the auditor arrives**, they do not have an effect hash or a session id.
They have a date range:

```bash
aura audit --since 2026-07-01 --out q3.json   # what acted, who authorized it, under which policy
aura audit --verify q3.json                   # anyone, anywhere, no node running
```

The report carries the counts, the distinct policy documents in force, a
breakdown per capability and per signing operator, and a portable receipt per
gated effect. `aura bundle` exports one session as the retained materials a
second reader needs to re-derive an attribution rather than take it on faith —
trajectory, artifact provenance with hashes, model configuration — the four-part
shape an analysis of agent evaluation protocols argues for after finding that
most traces cannot support the conclusions drawn from them [[12]](#refs).
`aura bom` emits a CycloneDX 1.6 ML-BOM of the models and skills that actually
ran, built from the ledger rather than from configuration.

**Shipping a new model?** `aura regress` replays recorded sessions against it and
diffs the *effects*, not the transcripts — so "the wording changed" and "it
stopped issuing the refund" stop being the same result.

---

## 3 · Where it honestly stands

Read this before the next section, because the next section invites you to run
other people's code.

| | |
|---|---|
| **Tested** | Streaming envelopes with per-edge QoS over WebSocket and QUIC · the ledger and offline verification · the gate as a kernel invariant · **signed approver identity sealed into the entry, with what they were shown inside the signature** · **scoped skill credentials** · **the credential broker** · **the witness's own published log, and a monitor that catches one rewriting it** · cancellation · session resume · deterministic replay · **effect-level regression** · **period audit reports that verify standalone** · typed ports given compiled decoding grammars · the MCP border both ways · `aura guard` · Wasm skills in a real sandbox · Postgres CDC · event-log rotation and recovery · **verifiable backup and restore** · **signing-key rotation, with the retired key's checkpoints still verifying** |
| **Hand-verified** | Voice with barge-in · the planner (`aura do`) · `aura why` · OpenTelemetry export · ML-BOM |
| **Not there yet** | No multi-device view of one live session · **no failover if the node dies** — one process, and nothing starts a replacement (a restarted node *does* rebuild session state from the event log, and a lease keeps a second one from starting beside a live one) · TEE evidence reaches `bound`, never `verified` — vendor chain verification is declared and refused rather than stubbed · the SDKs are packaged but unpublished |

**The gap that matters most for what follows: a `format: source` skill is not
contained.** `--sandbox process` scrubs its environment, jails its working
directory and checks declared egress before launch, which stops accidental
credential leakage and casual filesystem wandering. It stops hostile code not at
all. `format: wasm` *is* genuinely sandboxed, in a real WASI sandbox. A real
boundary for source skills means a microVM, which is declared and refused at
startup rather than quietly downgraded.

`internal/` sits at 71.5% test coverage, `cmd/aura` at 8.8%, and the UI is
tested at its model layer — the graph model against the kernel's own C2
conformance vectors, plus the editing operations, the conversation fold and
every translation key — but not its components. A
59-check conformance suite runs the kernel over the wire, and separate CI jobs
prove the ledger detects tampering by editing a real database behind a real
binary's back, that a scoped token cannot act as the operator, that the
container comes up healthy on an empty volume and drains on SIGTERM, and that a
node started **with its default auth** can be driven end to end — that last one
exists because every other lane starts its node with `--no-auth`, and three
credential bugs shipped through the gap. Read
[Security model](GUIDE.md#security-model) before you expose a port, and [the roadmap](ROADMAP.md) for what is left and what it blocks.
This is pre-production; treat it that way.

---

## 4 · A registry you host, and skills you own

Skills are distributed through a **federable** registry — anyone hosts one with
`aura registry serve`, exactly like a container registry. That is what makes the
neutrality of the ecosystem something you can check rather than something we
promise.

```bash
aura registry serve                      # host a registry, on its own port
aura publish my-skill/                   # zip + sign (Ed25519) + upload
aura add --capability sensorial.ocr      # discover by capability, not by name
aura run acme/vision/invoice-ocr         # start it against the local node
```

Enforced and tested: **immutable versions** (republishing a version with
different content is rejected), **trust-on-first-use** (the first publish binds a
package id to its publisher key, and a later version signed by a different key
cannot hijack it), and a permissions review before anything lands.

**A public registry is up.** `https://registry.deepaxiom.com` serves the same
`r1` API as `aura registry serve`, from the same code. Point the CLI at it with
`--registry` or `AURA_REGISTRY`:

```bash
export AURA_REGISTRY=https://registry.deepaxiom.com
aura add acme/vision/invoice-ocr         # verifies hash + signature, reviews permissions, installs
curl -s https://registry.deepaxiom.com/r1/health   # {"api":"r1","ok":true,"registry":"aura"}

# publishing needs a publisher credential; it rides in the URL
AURA_REGISTRY=https://publisher:PASS@registry.deepaxiom.com aura publish my-skill/
```

Reads are open; writes are not. Every mutating method is behind basic auth at
the edge, because the registry's trust-on-first-use binds the *first* key that
publishes an id to it permanently, with no rotation and no administrative
override — left open, anyone could claim `deepaxiom/*/*` forever. A publisher
credential is issued on request (`info@deepaxiom.com`) until Studio accounts
can issue them per developer.

**DeepAxiom Studio is under construction** at `https://studio.deepaxiom.com`
([repository](https://github.com/DeepAxiom/deepaxiom_studio)): a web front to
browse what the registry holds, download a version, publish yours, and a
directory of who builds what. It runs on the same VPS as the registry and
deliberately outside its trust boundary — it reads the registry only through
the public HTTP API above, never its disk, which is where the trust-on-first-use
bindings live. Not to be confused with the **Studio view** of the node's own
control plane, which is where you build and run graphs; the name is shared, the
thing is not.

**The SDK is Apache-2.0**, so a skill you write and sell carries no copyleft
obligation, ever. Given the isolation gap above, today's honest pitch is *publish
and host your own* rather than *install strangers' code* — the distribution,
signing and discovery are real and tested; the sandbox that would make a public
catalogue safe is not there yet.

**The ten skills in [`skills/`](skills/) are demos.** They exist to show the
shape of a skill and give a cold node something to run — not to be a catalogue,
and not to be depended on in production; hence the `example/` org in every
manifest. Copy the closest one and replace it. Start from
[`skills/echo/`](skills/echo/), about 100 lines.

---

---

## Running it beside your app

The integration is six lines. The deployment is a process, and that distinction
is the one that decides whether this fits your stack:

```js
import { createNode } from "@deepaxiom/aura";
const aura = createNode({ org: "acme", app: "shop" });

aura.expose("get-order",    ({id}) => db.orders.find(id),   { params: ["id"] });
aura.expose("refund-order", ({id}) => db.orders.refund(id), { params: ["id"], write: true });

await aura.start();
```

`write: true` is the only line here about safety, and it is a declaration rather
than an implementation: the executor gates that call on every edge that reaches
it, including in a graph whose author never asked for one, and seals the effect
into the ledger. CI proves exactly that against a real binary — see
[`examples/expose-app/`](examples/expose-app/).

```bash
docker compose up      # the kernel, with a persistent ledger, beside your app
```

The container is distroless, non-root and CGO-free, and `STOPSIGNAL SIGTERM`
with an exec-form entrypoint is not boilerplate: without it the signal never
reaches the kernel, `docker stop` becomes a SIGKILL ten seconds later, and the
store's shutdown ordering is skipped on every deploy. The node drains within a
bounded window and says so.

**The data directory is not a cache.** It holds the node identity, the effect
ledger and the broker's encrypted secrets — and the broker's key is *derived*
from the identity, so a restore without `identity/` yields ciphertext nobody can
open. Back it up like a database — `aura backup` writes the whole directory as
one archive and refuses to produce one missing the identity; `aura restore`
recomputes the ledger from what it extracted and compares it against what the
archive claimed, so a restore that half-worked fails loudly instead of quietly.

Two endpoints an orchestrator needs:

| | |
|---|---|
| `GET /readyz` | Open, because a probe holds no credential. Stricter than `/healthz`: it answers only once the store and ledger are usable, so a rolling deploy does not send traffic to a node that is up but not working. |
| `GET /metrics` | Prometheus text, **authenticated** — the series include live session counts, ledger size and the pending approval queue. `aura_store_batch_mean` is the one for capacity: near the batch cap means the commit is your ceiling and more writers would not help. |

**What this does not survive is the node dying.** One process, no failover; the
event log survives, the live routing state does not. Answer that honestly before
you deploy: if AURA falls over, does your app degrade or stop? If it degrades,
this is deployable today. If it stops, read [the roadmap](ROADMAP.md) first.

**The SDKs are packaged and not published.** `@deepaxiom/aura` and `aura-sdk`
exist, build and are tested; there is no `npm install` for them yet, so today you
vendor them from the repo.

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
travels with the action instead of in a log somebody has to correlate later. That
shape is written up as [a proposal to MCP](spec/proposals/mcp-effect-receipts.md).

---

## 60 seconds

Signed binaries for Linux, macOS and Windows on amd64 and arm64 are on the
[v0.3.0 release](https://github.com/DeepAxiom/deepaxiom_aura/releases/tag/v0.3.0). Verify before you run — the
signature on `SHA256SUMS` proves who built the file, and only then is the sum
worth checking ([how](SECURITY.md#verifying-a-release)):

```bash
curl -LO https://github.com/DeepAxiom/deepaxiom_aura/releases/download/v0.3.0/aura-linux-amd64
curl -LO https://github.com/DeepAxiom/deepaxiom_aura/releases/download/v0.3.0/SHA256SUMS{,.sig,.pem}
cosign verify-blob --certificate-identity-regexp \
  "^https://github.com/DeepAxiom/deepaxiom_aura/.github/workflows/release.yml@.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig --certificate SHA256SUMS.pem SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
chmod +x aura-linux-amd64 && ./aura-linux-amd64 up   # kernel, UI, state store, ledger, witness client
```

Or the long way, from source:

```bash
git clone https://github.com/DeepAxiom/deepaxiom_aura && cd deepaxiom_aura/kernel
go build -o aura ./cmd/aura     # Go 1.25+, no CGO, no external services
./aura up
```

One binary, 18.6 MB — the stripped build on the release page and in the
container; an unstripped local `go build` is nearer 26. No account, no cloud,
no Postgres, no broker, no cluster. `aura up` is the whole runtime and no skills: skills are
separate processes that connect *to* it, so a fresh node is a working kernel
with an empty catalogue and four seeded graphs — `echo`, `chat`, `plan` and
`voice` — to point one at.

```bash
pip install -r skills/llm-chat/requirements.txt
cd skills/llm-chat && PYTHONPATH=../../sdk/python/src python main.py
aura chat "explain backpressure"    # streams token by token
```

---

<a id="refs"></a>

## References

Where this README makes a claim about the state of the art, here is what it
rests on. Several of these describe the same problem we do and solve it
differently — that is the point of listing them.

1. Kumar et al., *Model Context Protocol Threat Modeling and Analyzing Vulnerabilities to Prompt Injection with Tool Poisoning* — [arXiv:2603.22489](https://arxiv.org/abs/2603.22489). STRIDE/DREAD across the MCP components; tool metadata is the primary client-side attack surface.
2. *Parasites in the Toolchain: A Large-Scale Analysis of Attacks on the MCP Ecosystem* — [arXiv:2509.06572](https://arxiv.org/abs/2509.06572). Why `aura guard` treats an unannotated tool as acting on the world.
3. Silvestre et al., *Failure Transparency in Stateful Dataflow Systems* — [arXiv:2407.06738](https://arxiv.org/abs/2407.06738). The correctness property session resume is an instance of.
4. *Oversight Has a Capacity: Calibrating Agent Guards to a Subjective, Fatiguing Human* — [arXiv:2606.08919](https://arxiv.org/abs/2606.08919). The strongest argument against over-gating, and the reason policy decides rather than the graph.
5. *When Agents Handle Secrets: A Survey of Confidential Computing for Agentic AI* — [arXiv:2605.03213](https://arxiv.org/abs/2605.03213).
6. *CapSeal: Capability-Sealed Secret Mediation for Secure Agent Execution* — [arXiv:2604.16762](https://arxiv.org/abs/2604.16762). Independent convergence on the credential-broker shape.
7. *Right to History: A Sovereignty Kernel for Verifiable AI Agent Execution* — [arXiv:2602.20214](https://arxiv.org/abs/2602.20214). RFC 6962 logs plus capability boundaries, in a Rust kernel.
8. *Notarized Agents: Receiver-Attested Confidential Receipts for AI Agent Actions* — [arXiv:2606.04193](https://arxiv.org/abs/2606.04193). Receiver-side signing and witness-cosigned logs; a different cut at the same evidence problem.
9. Syta et al., *Keeping Authorities "Honest or Bust" with Decentralized Witness Cosigning* — [arXiv:1503.08768](https://arxiv.org/abs/1503.08768). The origin of the argument that a witness who can be caught equivocating needs no trust.
10. Cai et al., *Are You Getting What You Pay For? Auditing Model Substitution in LLM APIs* — [arXiv:2504.04715](https://arxiv.org/abs/2504.04715). Silent model swaps, measured in the wild.
11. *Confidential LLM Inference: Performance and Cost Across CPU and GPU TEEs* — [arXiv:2509.18886](https://arxiv.org/abs/2509.18886). The 4–8% figure, and where it comes from.
12. *Do Agent Benchmarks Measure Capability? Protocol Validity in the Age of Agentic AI* — [arXiv:2607.22368](https://arxiv.org/abs/2607.22368). The audit-bundle shape `aura bundle` implements.
13. Chursin et al., *Tidehunter: Large-Value Storage With Minimal Data Relocation* — [arXiv:2602.01873](https://arxiv.org/abs/2602.01873). Treat the log as permanent storage; compaction stops existing.

Non-arXiv, and load-bearing: [RFC 6962](https://www.rfc-editor.org/rfc/rfc6962)
(Certificate Transparency), [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032)
(Ed25519), [Regulation (EU) 2024/1689](https://artificialintelligenceact.eu/article/12/)
(the AI Act), and [Regulation (EU) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj)
(the Digital Omnibus on AI, which deferred the high-risk dates above and left
Articles 12 and 14 otherwise intact).

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
