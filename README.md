# AURA

### The operating system for autonomous robots.

Fast enough to be in the loop. Every decision traceable to what argued for it.
Nothing acts on the world that the node did not check first.

[Quick start](QUICKSTART.md) · [Full guide](GUIDE.md) · [Roadmap](ROADMAP.md) ·
[Milestone status](GUIDE.md#milestone-status) · [Versión en español](README-ES.md) ·
**v0.4.0 — pre-1.0, pre-production**

<!-- UI-HERO -->
<p align="center"><img src="docs/media/studio-live.gif" alt="The control plane: a graph running live, tokens streaming on the node that produces them, a gated edge into a motor Expertise" width="900"></p>
<!-- /UI-HERO -->

---

## What it is, and what it is today

A machine that acts on its own has three problems. Perceiving and moving have
had good answers for years — the robot's middleware and its controllers.
**Deciding** is newer: the models are large, share one accelerator, and are
replaced every few months. And **answering for what it did** is the one nobody
builds first, which is a mistake, because a system that was not designed to
emit that evidence cannot do so later without rebuilding how it executes.

AURA is the layer for the last two. One binary that runs the models that
decide, carries what they see and say as live typed streams, and stands between
every decision and the actuator.

**Read this before the rest.** Today it is the decision layer, with the
evidence built in. It has not driven a physical robot in anything this
repository tests, and the bridge to a robot's middleware is the
[first item on the roadmap](ROADMAP.md#the-robot-itself), not something that
exists. It is not a hard-real-time controller and will not become one: the
servo loop stays on the robot. What AURA controls is what that controller is
*asked* to do, and what the record says it did.

Everything below is built and tested unless it says otherwise.

---

## 1 · Fast

### A sensor does not finish

Batch tools fire a trigger, run a chain once and tear it down. That fits a
nightly sync and breaks the moment the work is *live* — a camera, a
conversation, a model answering token by token.

<p align="center"><img src="docs/media/mechanism-connection.svg" alt="A batch run starts, runs and is destroyed; a connection stays open and envelopes flow through the graph as they are produced" width="820"></p>

Text, audio, frames and events travel as the same typed envelope on a
connection that stays open. A node serves **QUIC (WebTransport)** on the same
port number as its TCP listener, so the three QoS classes are three real
transport primitives: `realtime` is a datagram that cannot stall or be stalled,
`reliable` is an ordered stream per edge, `bulk` has its own. A peer that
cannot reach UDP keeps the WebSocket path. **Deadlines are absolute and
inherited** — a hop can tighten one, never extend it — and a delivery that is
already late is dropped rather than computed for nobody.

### A slow session only slows itself

Every session an Expertise serves shares its one connection. The kernel used to
route each envelope inline, so one session whose consumer fell behind stalled
every other session on that socket. Each session now has its own queue.

<p align="center"><img src="docs/media/mechanism-mailbox.svg" alt="With inline routing a blocked session stalls the ones behind it on the same connection; with a queue per session only the blocked one waits" width="820"></p>

New sessions go to the replica carrying the fewest live ones, counted by the
kernel itself. And a client's `cancel` does not queue: it overtakes every token
still waiting behind it, and the kernel suppresses them — cancellation is a
guarantee here, not a request.

### Fewer envelopes for the same words

A model emits a token every few milliseconds, and each used to be its own
envelope — an id, two encodings, two log appends, a routing decision — for four
characters.

<p align="center"><img src="docs/media/mechanism-coalescing.svg" alt="Many single-token envelopes become a few larger ones carrying the same text; the first token is never delayed" width="820"></p>

The first token still leaves immediately; nothing waits more than 25 ms. On a
fast stream that was **about 50 envelopes instead of 120, with identical
text**, in an end-to-end run against a real node.

### What is measured, and what is not

| | |
|---|---|
| Kernel routing + durability, one node, echo over loopback | 1 session: 1,504 msg/s, p50 0.58 ms · 200 sessions: 1,908 msg/s, p50 63 ms (was 344 msg/s, p50 489 ms before group commit). `kernel/cmd/loadgen` reproduces it. |
| Causal event log | 1.5M events/s on append-only segments against SQLite's 27k at matched durability. |
| A dead node replaced by its standby | 21–118 ms from kill to answering again, Linux, same host. `scripts/failover_e2e.py`. |
| Per-session queues and coalescing under load | **Not measured.** Tested for correctness; the existing load generator has no slow consumer and one envelope per message, so it shows neither. |
| Perception → decision → effect on a robot | **Not measured.** There is no robot in the loop yet. |

---

## 2 · Every decision traceable

<p align="center"><img src="docs/media/mechanism-gate-ledger.svg" alt="A delivery into a motor Expertise is held at the kernel gate for a named person, then sealed into a hash-chained ledger" width="820"></p>

- **The gate is a kernel invariant.** In agent libraries the interrupt lives in
  code you wrote, so the code that forgets has no gate. Here the executor
  applies it at the one point every delivery passes, driven by the node's
  policy. A graph can ask for *more* scrutiny than policy requires, never less.
- **The entry names the human who approved it, and they signed it.** With a key
  the node has never held, over digests of what they were actually shown — so
  the record says a person consented to *this*, not that somebody clicked.
  Written up as an [Internet-Draft](spec/proposals/draft-signed-human-approval.md).
- **Every effect is sealed, not logged.** A hash chain, committed to an
  RFC 6962 Merkle head the node signs and a third party can counter-sign.
  `aura verify` recomputes all of it from the database file alone, with no
  kernel running.
- **It names the model that argued for it.** Engine, model, revision,
  quantisation, sampling parameters, seed — and now the tokens the engine
  counted — bound to every effect that output caused. A silent model swap
  is a measured problem [[10]](#refs); here it breaks hashes already
  committed to an append-only chain. It remains the Expertise's word: an
  assertion bound to its consequences, not proof of what executed.
- **It joins the trace you already have.** A W3C `traceparent` rides every hop
  of the chain a caller starts — carried by the kernel, so it survives an
  Expertise that has never heard of it — and `aura up --otlp` exports spans
  while the node runs. Whole sessions are sampled, never partial trees, and
  payloads are never exported.

<p align="center"><img src="docs/media/mechanism-trace.svg" alt="A caller's trace context is carried across every hop inside the node and back, so the node's spans join the caller's trace" width="820"></p>

When someone asks afterwards, they do not arrive with a hash. They arrive with
a date range:

```bash
aura audit --since 2026-07-01 --out q3.json   # what acted, who authorised it, under which policy
aura audit --verify q3.json                   # anyone, anywhere, no node running
aura why <session>                            # what happened, narrated from the causal log
aura regress                                  # replay recorded sessions against a new model; diff the *effects*
```

**The obligation is already written down.** [Regulation (EU) 2024/1689](https://artificialintelligenceact.eu/article/12/)
requires automatic event logging (Article 12) and effective oversight by
natural persons (Article 14) for high-risk AI systems; its application dates
were moved by [Regulation (EU) 2026/1744](https://eur-lex.europa.eu/eli/reg/2026/1744/oj)
to 2 December 2027 for Annex III systems and 2 August 2028 for systems embedded
in regulated products. The calendar moved; the text did not. ISO/IEC 42001
asks for the same chain of evidence today.

---

## 3 · Secure

- **What acts on the world is a type.** Every Expertise declares whether it
  perceives, reasons, remembers, transforms or acts (`motor`). Policy is
  deny-by-default for `motor.*`; speculation is refused at wiring on any edge
  into one.
- **An Expertise is not the operator.** A credential scoped to one capability
  can connect, register as that capability and redeem the receipts it is
  handed — and cannot register a graph, read the ledger or enrol an approver.
- **A secret is released against a receipt, not held ambiently.** Skipping the
  gate stops being a way to avoid scrutiny and becomes a way to get a 401.
- **A viewer gets a ticket for one session and nothing else.** Signed, short
  lived, good on one route.
- **`format: wasm` Expertise run in a real WASI sandbox.** `format: source`
  does **not**: `--sandbox process` scrubs the environment and jails the
  working directory, and does not contain hostile code. Until the microVM
  boundary lands, the honest position is *run your own Expertise*, not
  *install a stranger's*.
- **Pickle-format weights are refused; model revisions are pinned** before
  download, so what is attested is what was fetched.

Start here without adopting anything else — put the MCP servers an agent
already uses behind this node's policy, gate and ledger:

```bash
aura guard --config claude_desktop_config.json
```

It holds exactly as far as your control over the agent's configuration does;
there is no network enforcement. [Details](GUIDE.md#guarding-an-agents-tools).

---

## 4 · Models: deployment and optimisation

A robot is mostly a model-deployment problem with motors attached.

- **One contract for every model.** Language, speech, vision — any model an
  Expertise wraps — behind the same manifest and ports, local or remote.
  Swapping one changes placement, not the graph.
- **One set of weights for the whole node.** Point every first-party Expertise
  at one OpenAI-compatible server (`AURA_INFERENCE_URL`) and they share its
  slots, batching and prefix cache instead of each loading a copy.
- **A prompt whose prefix survives the conversation.** History is trimmed in
  blocks, so a KV cache or a provider's prompt cache stays valid between
  compactions instead of being invalidated on every turn.
- **Token cost as the engine counted it**, cache hits included, on the
  attestation and in `aura_tokens_total` — not a bytes/4 estimate.
- **A planner that cannot invent a capability**: its decoder is constrained by
  a grammar built from the live catalogue.
- **Fit before download.** `model-fit` estimates what a machine can run;
  `model-manager` fetches it with the revision pinned.

Not yet: the node owning and supervising that inference server, swapping a
model without a restart, producing an optimised artefact for a device rather
than choosing among downloaded ones, and scheduling several models on one
accelerator. The shared-server path is verified against a fake endpoint, not a
real one. [Roadmap](ROADMAP.md#models-deployment-and-optimisation).

---

## 5 · Video

[`media/`](media/) is a second artefact, `aura-media`, with its own version
line — encoding pins a core for minutes and approving an effect must never
queue behind it.

- **Playable while it encodes.** Segments are published as they close; an asset
  has an address seconds after the encode starts, with progress reported.
- **Live in, adaptive HLS out.** An RTMP stream becomes a ladder with a DVR
  window that turns into an ordinary recording when the stream ends; a
  broadcaster that drops and reconnects continues the same stream.
- **Frames for models from the same decode as the encode.**
- **An audience on the session beside it.** `GET /v1/watch` attaches read-only
  observers to a live session — each unable to slow it, none shown a gate —
  with a ticket per viewer and the recent history replayed on joining.

Run end to end against real ffmpeg on Linux. Not played in a real player; no
hardware encoder has encoded a frame; live latency is calculated, not measured.
For a robot the missing piece is the other direction — a camera to a model in
tens of milliseconds, which HLS will never be.
[What is and is not built](media/README.md).

---

## 6 · A node that dies is replaced

```bash
aura up --data /var/lib/aura                 # the node
aura up --data /var/lib/aura --standby       # takes over when the first dies
```

The standby opens nothing the live node is writing. On the same host it sees
that the leader's process is gone instead of waiting out a lease, and answers
as the *same node* — same identity, same ledger — in tens of milliseconds.
Expertise reconnect on their own; a client that reconnects continues its
session from the causal log. What it does not survive is losing the machine:
the standby needs the data directory.
[Details](GUIDE.md#what-a-node-does-not-survive).

---

<!-- UI-SECTION -->
## The control plane

It is in the binary — `aura up`, then open the port. Every screen below is the
real UI against a running node. The traffic comes from three small test
Expertise, labelled as tests on screen — the one that streams text replays a
fixed passage and is not a model. How each was captured is in
[`docs/media/README.md`](docs/media/README.md).

**Studio.** Build a graph and watch it run: envelopes move along the edges as
they are delivered, text appears on the node producing it, a gated edge into a
`motor` Expertise is visibly gated.

<p align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/studio-gate-dark.png"><source media="(prefers-color-scheme: light)" srcset="docs/media/studio-gate-light.png"><img src="docs/media/studio-gate-dark.png" alt="Studio with a delivery held at the gate on the edge into a motor Expertise" width="900"></picture></p>

**Pulse.** What the node is doing now: live sessions, tokens in and out as the
engines counted them, the prefix-cache hit rate, one bar per replica showing
the sessions it carries, dispatch queues, watchers.

<p align="center"><img src="docs/media/pulse-load.gif" alt="Pulse: live sessions, token rates, prefix cache, load per replica" width="900"></p>

**Sessions as traces.** Each session is its causal tree — what caused what,
when, on which port, costing how many tokens — with the trace id it joined.

<p align="center"><img src="docs/media/session-waterfall.gif" alt="A session shown as a causal waterfall" width="900"></p>

**Evidence.** The ledger, entry by entry: capability, decision, who approved,
under which policy, and whether the chain verifies.

<p align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="docs/media/evidence-ledger-dark.png"><source media="(prefers-color-scheme: light)" srcset="docs/media/evidence-ledger-light.png"><img src="docs/media/evidence-ledger-dark.png" alt="The effect ledger with its verification result" width="900"></picture></p>

---

<!-- /UI-SECTION -->

## Where it honestly stands

| | |
|---|---|
| **Tested** | Streaming envelopes with per-edge QoS over WebSocket and QUIC · per-session dispatch and load-aware replicas · the gate as a kernel invariant · signed approver identity over what they were shown · the ledger and offline verification · scoped credentials and the credential broker · witness log and its monitor · cancellation · session resume · standby takeover · deterministic replay and effect-level regression · period audit reports · trace context and live span export · reported token usage · read-only watchers with tickets and history · typed ports with compiled grammars · the MCP border both ways · Wasm Expertise in a real sandbox · Postgres CDC · verifiable backup and restore · signing-key rotation |
| **Hand-verified** | Voice with barge-in · the planner (`aura do`) · `aura why` · ML-BOM |
| **Tested against stand-ins only** | The shared inference server · the in-process KV state cache · the planner grammar (no llama.cpp in CI) · hardware video encoders (never run) |
| **Not yet** | A bridge to a robot's middleware · a node-wide stop · binary frames for sensor data · surviving the loss of the machine · containment for `format: source` · TEE evidence beyond `bound` · published SDKs and container image |

A 67-check conformance suite runs the kernel over the wire, and separate CI
jobs prove the ledger detects tampering by editing a real database behind a
real binary's back, that a scoped token cannot act as the operator, and that a
node started **with its default auth** can be driven end to end. Read
[Security model](GUIDE.md#security-model) before exposing a port, and
[the roadmap](ROADMAP.md) for what is missing and why it comes in that order.
This is pre-production; treat it as such.

**On the name.** What earlier releases called a *skill* is an **Expertise**: a
typed, running function that is expert at one thing — not a prompt handed to a
model. Every old identifier still works (`skill.yaml`, `/ws/skill`,
`/v1/skills`, `Skill`).

---

## A registry you host, and Expertise you own

Expertises are distributed through a **federable** registry — anyone hosts one with
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

**DeepAxiom Studio** is up at `https://studio.deepaxiom.com` as a first,
read-only cut: a web front to browse what the registry holds, download a
version, and a directory of who builds what — publishing from the browser and
accounts come next. It runs on the same VPS as the registry and
deliberately outside its trust boundary — it reads the registry only through
the public HTTP API above, never its disk, which is where the trust-on-first-use
bindings live. Not to be confused with the **Studio view** of the node's own
control plane, which is where you build and run graphs; the name is shared, the
thing is not.

**The SDK is Apache-2.0**, so an Expertise you write and sell carries no copyleft
obligation, ever. Given the isolation gap above, today's honest pitch is *publish
and host your own* rather than *install strangers' code* — the distribution,
signing and discovery are real and tested; the sandbox that would make a public
catalogue safe is not there yet.

**The ten Expertises in [`expertise/`](expertise/) are demos.** They exist to show the
shape of an Expertise and give a cold node something to run — not to be a catalogue,
and not to be depended on in production; hence the `example/` org in every
manifest. Copy the closest one and replace it. Start from
[`expertise/echo/`](expertise/echo/), about 100 lines.

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

**What this does not survive is losing the machine.** A node that dies is
replaced by a standby on the same host (section 6); a machine that dies takes
the data directory with it. Answer that honestly before you deploy: if this
machine is gone, does your app degrade or stop? If it stops, read
[the roadmap](ROADMAP.md) first.

**The SDKs are packaged and not published.** `@deepaxiom/aura` and `aura-sdk`
exist, build and are tested; there is no `npm install` for them yet, so today you
vendor them from the repo.

## Connecting what you already have

```bash
aura connect --openapi ./crm.yaml   # every operation becomes a skill
```

Read-only by default, with dry-run and per-operation promotion before anything
writes. Webhooks come in, `sensorial` Expertises wrap systems that push, and
inversion-of-control Expertises dial *out* so they run behind NAT with no inbound
ports. At the borders: an **MCP server** (every Expertise is a tool for Claude Code
or Cursor, and `tools/call` streams), an A2A discovery card, and OpenTelemetry
export of the causal tree.

An MCP tool call comes back with the receipts for what it did — capability,
decision, outcome, and the human who signed for it — in `_meta`, so the evidence
travels with the action instead of in a log somebody has to correlate later. That
shape is written up as [a proposal to MCP](spec/proposals/mcp-effect-receipts.md).

---

## 60 seconds

Signed binaries for Linux, macOS and Windows on amd64 and arm64 are on the
[v0.4.0 release](https://github.com/DeepAxiom/deepaxiom_aura/releases/tag/v0.4.0). Verify before you run — the
signature on `SHA256SUMS` proves who built the file, and only then is the sum
worth checking ([how](SECURITY.md#verifying-a-release)):

```bash
curl -LO https://github.com/DeepAxiom/deepaxiom_aura/releases/download/v0.4.0/aura-linux-amd64
curl -LO https://github.com/DeepAxiom/deepaxiom_aura/releases/download/v0.4.0/SHA256SUMS{,.sig,.pem}
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
no Postgres, no broker, no cluster. `aura up` is the whole runtime and no Expertises: Expertises are
separate processes that connect *to* it, so a fresh node is a working kernel
with an empty catalogue and four seeded graphs — `echo`, `chat`, `plan` and
`voice` — to point one at.

```bash
pip install -r expertise/llm-chat/requirements.txt
cd expertise/llm-chat && PYTHONPATH=../../sdk/python/src python main.py
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
and sell Expertises, no copyleft obligation ever. That includes the parts a second
implementation would need most: the ledger contract, the witness protocol, and a
67-check conformance suite to demonstrate compliance with. A verifier is worth
less the fewer things it can verify, and an anchor is worth less the fewer
parties present to it — neither is a good thing to own. The kernel and UI are
AGPLv3, with a commercial license available instead. Running `aura`
unmodified — your laptop, your servers, inside your company — triggers no AGPL
obligation. See [LICENSE.md](LICENSE.md) · Contributions:
[CONTRIBUTING.md](CONTRIBUTING.md).
