# Roadmap

AURA is being built to be the operating system for autonomous robots: the
layer that takes what a machine perceives, runs the models that decide, and
authorises what it does — fast enough to be in the loop, with every decision
traceable to what argued for it, and with nothing acting on the world that the
node did not check first.

This file is the distance between that sentence and the code. It is ordered by
what a robot needs first, not by what is easiest. What already exists is in
[Milestone status](GUIDE.md#milestone-status); this is only what does not.

[Versión en español](ROADMAP-ES.md)

---

## Where the line is today

Said first, because everything below depends on it.

**What the kernel is good for now** is the decision layer of a machine: streams
in, models reasoning over them, effects out through a gate, all of it recorded.
Latencies are those of a Go process routing JSON envelopes over sockets —
sub-millisecond per hop on loopback, uncontended. That is the right tier for
"should the arm pick this up", "is this person in the way", "reroute", and the
wrong one for the servo loop that moves the arm.

**What it is not, and is not going to be,** is a hard-real-time controller. A
1 kHz control loop belongs on the robot's own controller, where it is today.
The claim is narrower and more useful: nothing that controller is *asked to do*
arrives without having passed the node's policy, and nothing it did is missing
from the record.

**What has not happened yet:** no physical robot has been driven by this code
in anything this repository tests. Every item under "The robot itself" below is
the work of making that sentence false.

---

## The robot itself

Nothing here is research. It is the set of connections a machine needs before
the rest of this runtime is about it at all.

| | Why it comes first | Shape of the work |
|---|---|---|
| **A bridge to the robot's middleware** | A robot already has one — ROS 2 in most cases — and its sensors, actuators and controllers already speak it. Until AURA can subscribe to a topic and call an action, it is a decision layer with nothing to decide about. | Topics, services and actions projected as Expertise, the way `aura connect` projects an OpenAPI document: a topic is a `sensorial` stream, an action is a `motor` effect and is therefore gated and sealed like any other. Read-only by default, promotion per action. |
| **A stop that is a kernel invariant** | A gate asks before an effect. A robot also needs the opposite: one signal after which *no* `motor.*` delivery leaves the node, whatever is queued, whatever a graph says, until a named person lifts it. Today the nearest thing is a policy edit and a restart. | A node-wide halt in the executor, at the same point the gate is enforced, sealed in the ledger when it is raised and when it is lifted, raisable from a hardware input. It must hold with the UI down and the network gone. |
| **Sensor data that is not base64** | PCM and frames travel as base64 inside JSON: a third larger than they are, encoded and decoded on every hop. Tolerable for speech, not for cameras. | A binary frame for `realtime` edges — an additive C3 change, not yet specified — and a same-host path that does not copy through the kernel at all. C3 already says data need not transit the kernel when a direct route exists; only the relay is implemented. |
| **Time** | "Which frame was this decision made on" needs both to carry a clock the machine agrees on. Envelopes carry an id and a causal parent, not a capture time. | A capture timestamp on sensor envelopes, monotonic per source, and the hash of the frame a decision consumed kept even when the frame itself is not (the event log already elides `realtime` payloads to a digest; the digest has to become citable from an effect). |
| **Running on the machine** | Linux arm64 binaries are built and a Raspberry Pi smoke test exists. Nobody has measured a node on a Jetson with a camera and a model sharing its GPU. | A reference device, a measured budget for perception → decision → effect on it, and that number in this file. |

## Fast

"Ultra-fast" is a claim, and at the moment it is not a measured one. 0.4 removed
the places where time was being lost for no reason; it did not produce the
number.

| | State | What is missing |
|---|---|---|
| **A latency budget, measured** | Per-session dispatch queues, load-aware replicas, delta coalescing and a stable prompt prefix are in and tested for correctness. | A figure. `kernel/cmd/loadgen` drives an echo round-trip — no slow consumer, one envelope per message — so it shows none of it. A load generator with a streaming producer, a deliberately slow consumer and a p99 per hop is what turns "a slow session only slows itself" from a tested property into a measured one. |
| **Deadlines that do something** | Absolute, inherited deadlines exist: a hop may tighten one and never extend it, and a late delivery is dropped rather than sent. | The receiving Expertise is *expected* to degrade and nothing checks that it does. A deadline needs a second half: what runs instead when the budget is gone — a smaller model, a cached answer, a safe default. |
| **Priority that preempts** | Edges declare a priority and the kernel stamps it on every delivery. | Acting on it is left to the Expertise that receives it; the kernel itself preempts nothing. On a machine with one GPU, a safety-relevant chain has to be able to take the model away from a conversational one, and that has to be the node's decision rather than each Expertise's. |
| **A context budget in real tokens** | Expertise can report what an engine counted (C5 v1.2) and the kernel meters it. | The per-graph budget is still charged at bytes/4. Charging it from reported usage changes what an existing budget means, which is why it was not done in passing. |

## Every decision traceable

This is the part that exists most. The gate, the hash-chained ledger, signed
approval, inference attestation, witnessing, replay and regression are built
and tested. What a robot adds:

| | Why a machine needs it |
|---|---|
| **A recorder that survives the machine** | The evidence is in the data directory, and the data directory is on the robot. When the robot is what failed, that is the wrong place for it. The ledger can already be anchored at a witness; the *log* cannot be shipped anywhere. Continuous shipping of sealed segments to a second machine is also what would make failover survive losing the first one. |
| **Replay against recorded perception** | `aura replay` and `aura regress` re-run recorded sessions and diff the effects. For a robot the input is sensor data, most of which the log deliberately does not keep. Replaying a decision needs the frames it saw, opt-in and bounded. |
| **TEE chain verification** | Hardware evidence reaches `bound`: the quote is tied to one declaration and checkable offline. `verified` needs the manufacturer's chain, which is declared and refused rather than stubbed, because it has never run against real hardware. |
| **A trace that includes the controller** | A W3C trace context now rides every hop inside a node. It stops at the bridge to the robot's middleware, which does not exist yet. |

## Secure

| | State | What is missing |
|---|---|---|
| **Containment for code that is not yours** | `format: wasm` Expertise run in a real WASI sandbox. `format: source` does not: `--sandbox process` scrubs the environment and jails the working directory, and does not contain hostile code. | A microVM boundary, which is declared and refused at startup rather than degraded. Until it lands, the honest position is *run your own Expertise*, not *install a stranger's*. |
| **A credential for a viewer, and one for a browser** | A watch ticket lets one viewer observe one session and nothing else. | A frontend still cannot *drive* a session without the application's backend in the middle. The session-scoped credential for that is not built. |
| **Supply chain for what the robot runs** | Packages are signed and versions are immutable; model revisions are pinned and attested; pickle-format weights are refused. | Nothing verifies that the model a node was *told* to run is the one a fleet operator *approved*. A signed deployment manifest, checked at load, is the missing link between the registry and the attestation. |
| **Operating without a network** | A node needs nothing external to run. | Approval does. A machine in a field has to be able to act on a policy decided in advance, and the record has to say that is what happened — pre-authorisation with an expiry, sealed like any other approval. |

## Models: deployment and optimisation

A robot is mostly a model-deployment problem with motors attached. What exists
is a start: a catalogue and downloader (`model-manager`), a hardware-fit
estimator (`model-fit`), pinned and attested revisions, and — since 0.4 — every
first-party Expertise able to share one set of weights through one inference
server.

| | What is missing |
|---|---|
| **The node owning its inference server** | `AURA_INFERENCE_URL` points at a server; nothing starts it, supervises it or restarts it. Marking a model active does not change what that server has loaded, and admission (`--memory-budget`) does not know it exists. Verified against a fake endpoint only. |
| **Swapping a model without stopping the machine** | A change of weights is a restart of whatever holds them. What is needed is load-beside, shift, unload — with `aura regress` as the gate that decides whether the shift happens, because that is exactly the question it already answers. And a way back. |
| **Optimisation as a step, not a download** | `model-fit` chooses among quantisations someone else produced. Producing the right artefact for a given device — quantising, compiling for its accelerator — is not in this repository, and the attestation has no way to say an artefact was derived from a pinned revision rather than downloaded. |
| **More than one model on one accelerator** | Each Expertise that loads weights loads its own copy. Perception, language and speech on one small GPU need a scheduler that knows what is loaded, what it costs and which chain is waiting. |
| **Speech that is actually incremental** | An opt-in LocalAgreement policy gives a stable prefix. Measured on a CPU with a small model it did not make partials cheaper, and it made one language's finals worse. A streaming recogniser, rather than a batch one decoded repeatedly, is still the real fix. |

## Video

A robot's main sense. `media/` is a separate artefact with its own version
line, on purpose: encoding pins a core for minutes and approving an effect must
never queue behind it.

`aura-media` 0.2 publishes while it encodes — an asset is playable seconds
after the encode starts, with progress reported — takes a live stream in over
RTMP and puts out an adaptive HLS ladder with a DVR window that becomes an
ordinary recording when the stream ends, samples frames in the same decode as
the encode, and can be told which encoder to use. All of it was run end to end
against real ffmpeg on Linux. None of it was played in a real player, no
hardware encoder has encoded a frame, and the live latency was calculated, not
measured. [`media/README.md`](media/README.md) has the list.

| | What is missing |
|---|---|
| **A camera, live, at a latency a robot can use** | HLS is the right shape for an audience and the wrong one for a control loop: seconds of delay by design. Perception needs frames in tens of milliseconds, which means WebRTC or a raw transport, not a playlist. |
| **Frames to a model without touching a disk** | Frames are sampled to files and uploaded to a store. A perception Expertise needs them handed over in memory, on the binary path above. |
| **De-identification before anything is kept** | Faces and text out of what a machine records in a place where people are. It has a human who approves and an artefact to seal, so it is a gated effect and not a filter — and it is the capability that forces this subsystem to encode a *new* video rather than only transcode one. |
| **An object store that is not a directory** | The interface exists; an S3-compatible implementation does not. |
| **Distribution at scale** | Not a goal. Segments are static files; a CDN in front of them is the answer, and building one here would be building the wrong thing. |

## A fleet

| | State | What is missing |
|---|---|---|
| **A node that dies** | `aura up --standby` stands ready beside a live node and takes over when it dies: measured at tens of milliseconds on Linux when both run on one host, because the standby can see the process is gone instead of waiting out the lease. Sessions resume from the causal log. | Losing the *machine*. The standby needs the data directory, and the data directory is on the machine that died. That is the log-shipping item above, and it is the difference between failover and high availability. |
| **Many machines** | Nodes federate: a graph can resolve an Expertise on another node. | Nothing manages a fleet as one thing — rollout of a policy or a model to a hundred machines, with the record of which machine ran which version when. |
| **Many clients on one session** | Read-only watchers, each unable to slow the session, with a ticket per viewer and a replay of recent history on attach. | The same over QUIC, and a watcher count that scales past one node's sockets by relaying rather than by raising the limit. |

## Getting it into someone's hands

The engineering is ahead of the distribution, and that distance is the real
obstacle to anyone finding out whether any of the above is true.

| | Shape of the work |
|---|---|
| **Publish the SDKs** | `@deepaxiom/aura` and `aura-sdk` are packaged and unpublished. Hours: npm and PyPI, and a CI job that publishes on tag. |
| **Publish a container image** | Built and exercised on every push; pushed nowhere. Hours. |
| **A public witness** | Anchoring is the network effect, and running one costs almost nothing. Days: an instance, a URL, a retention policy someone will stand behind. |

## Standards work

| | State |
|---|---|
| **[Signed human approval](spec/proposals/draft-signed-human-approval.md)** | At `-01`, not submitted. It adds the `context` member — digests of what the approver was shown — which is what turns "a named person clicked" into "a named person consented to this", and which the kernel implements as C4 v1.7. |
| **[Effect receipts for MCP](spec/proposals/mcp-effect-receipts.md)** | Written, not proposed upstream. |

## Deliberately not doing

- **A hard-real-time kernel.** See the top of this file. The servo loop stays
  where it is; what reaches it is what this controls.
- **A hosted control plane.** The neutrality argument only works if the
  registry and the witness are things you can host yourself.
- **A learned policy router.** Policy is meant to be read at a glance by
  whoever audits it. A model deciding what gets gated is not that.
- **A video CDN.** Static segments and someone else's edge.
- **Embedding manufacturer TEE roots in the binary.** A root that cannot be
  rotated fails closed at the worst moment or open at the wrong one.
