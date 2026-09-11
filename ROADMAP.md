# Roadmap

What is left, in the order it blocks somebody. Short on purpose: the previous
version of this file was 95 KB and most of it described work already finished,
which is a changelog wearing a roadmap's name. What shipped is in
[Milestone status](GUIDE.md#milestone-status); this is only what has not.

[Versión en español](ROADMAP-ES.md)

---

## Blocking production

Nothing below is a research problem. They are the difference between "deployable
as an auxiliary service" and "deployable as something with an SLA".

| | Why it blocks | Shape of the work |
|---|---|---|
| **Node failover** | *Measured, and smaller than it read.* A node is one process, and nothing starts a replacement — The gap is narrower than it used to read: a session's state *is* recoverable — `resumeFromLog` rebuilds the dedup window, causal indexes, pending gates and per-hop counters whether the client or the kernel was what died — and the SDK reconnects on its own. A single writer is now enforced by a lease, so a standby cannot corrupt the ledger by starting beside a live node. What is left is the orchestration: something that notices the leader is gone and starts the replacement, and a decision about whether that replacement lives on the same machine or another one. | **Reconsider before building.** Recovery after a SIGKILL measures 10.04s, of which 10.03s is waiting out the dead holder's lease; the node itself is back in 3ms, even over a 4,000-entry ledger. So `--lease-ttl` already *is* the recovery-time knob, and a warm standby would spend real complexity skipping a start-up cost measured in milliseconds. What remains genuinely unaddressed is machine loss, which needs the data directory reachable from two hosts — the first thing in this runtime that would require storage it does not ship. See [the degradation contract](GUIDE.md#the-degradation-contract). |

## Blocking adoption

The engineering is ahead of the distribution by a wide margin, and that gap is
the actual problem.

| | Why it blocks | Shape of the work |
|---|---|---|
| **Publish the SDKs** | `@deepaxiom/aura` and `aura-sdk` are packaged and unpublished. A full-stack team following [the integration guide](GUIDE.md#when-the-app-is-yours) cannot `npm install` it — they vendor it from the repo. | Hours: npm and PyPI, plus a CI job that publishes on tag. |
| **Publish a container image** | The binaries are done: `v0.3.0` ships six targets with cosign-signed checksums and an SBOM, and the repository is public. The image is built and exercised on every push — it becomes healthy on a fresh volume, drains on SIGTERM and keeps its identity across a restart — but it is not pushed anywhere, so `docker compose up` still builds it locally. | Hours: a push step in the release job, and a registry to push to. |
| **A web front for the public registry** | `registry.deepaxiom.com` is up and empty. Publishing and installing work from the CLI, but there is nowhere to browse what exists, read a manifest before installing, or find who built a skill. | Being built as [DeepAxiom Studio](https://github.com/DeepAxiom/deepaxiom_studio) at `studio.deepaxiom.com`: React and Go on the registry's VPS, outside its trust boundary — it reads the registry through the public `r1` API only, never its disk. Browse, download, publish, and a developer directory. |
| **A public witness** | Anchoring is the network effect. A witness several independent nodes present to is worth more than two nodes anchoring each other, and running one costs almost nothing. | Days: an instance, a URL, and a retention policy someone stands behind. |

## Narrowing the honest gaps

Each of these is a place the documentation currently says "this is not
guaranteed", and closing it moves a line in [Milestone
status](GUIDE.md#milestone-status).

| | What it would change |
|---|---|
| **microVM sandbox for `format: source`** | Today `--sandbox process` scrubs the environment and jails the working directory; it does not contain hostile code. Until this lands, the marketplace pitch is *publish and host your own*, not *install strangers' code*. `format: wasm` is already genuinely sandboxed. |
| **TEE chain verification** | Hardware evidence reaches `bound` — the quote is tied to that exact declaration and checkable offline. `verified` requires a vendor chain check, which is declared and refused rather than stubbed, because it has never run against real hardware. |
| **Short-lived browser credentials** | A web frontend cannot hold the operator token. Today the app's own backend proxies. A session-scoped, short-lived credential would remove that hop. |
| **Multi-device view of one live session** | One session, one socket. The "many clients watching one conversation" shape does not exist. |
| **One active model, for the whole node** | `model-manager` marks a model active and `llm-chat` follows it; the planner's local backend still reads `AURA_MODEL_FILE` with its own default, so the switch moves half the system. Both also load their own copy of the weights, which a 4B model makes expensive on a small card. |

## Reading an image, and the two refusals that come with it

**Built:** `deepaxiom/cognitive/imaging-read` — a sample of frames from one
imaging study in, findings with normalised coordinates out, plus a correlation
against the report on file, a draft impression and what it could not tell. Two
new schemas, `std/image-study@1` and `std/imaging-finding@1`.

**Cognitive and never motor**, which is the whole arrangement: nothing in it
writes. The draft becomes a line in a record only through a `motor.*` effect
that a named clinician approves at the gate, and the C5 attestation emitted with
the answer is what lets that effect say on what basis it happened — which model,
which revision, over which prompt. Without it a ledger can say who signed and
not what they were shown.

Two things it refuses, and both are refusals a consumer will meet on day one:

- **The endpoint it reads through.** The Gemini API, with a key. It is not
  covered by Google's data processing agreement, and that is a deployment
  decision rather than this skill's -- what the skill owes is that the decision
  is visible: the host is the only one in the manifest's egress list, and the
  engine goes into the C5 attestation of every read, so "which way did this
  image leave" is answerable from the record rather than from somebody's
  memory.
- **Frames nobody de-identified.** An ultrasound carries the patient's name
  *burned into the pixels*, not only in the tags — DICOM even has an attribute
  that says so, `BurnedInAnnotation`. This skill cannot check pixels, so it
  requires the sender to state that they were cleaned, and declines otherwise.
  Being the place where nobody checked is worse than declining.

**What it does not do yet**, in the order it will be wanted:

| Missing | Why it matters |
|---|---|
| **Mask the burn-in** | Today the requirement is pushed to the sender, so a study whose pixels carry a name simply cannot be read. It belongs here: it is the same frame decode the media subsystem already does, and it has a person who approves and an artefact to seal. |
| **A local reader** | The Gemini API means the frames leave the deployment. A node inside a hospital's own network will want the read to stay there, and the skill's shape does not change — only the backend behind `reader.py`. |
| **An endpoint under a contract** | Vertex AI was built and then removed: a second path nobody exercises is a second path that rots, and the consumer chose the key. It comes back the day a deployment needs a data processing agreement, and it is the same shape — one module behind `reader.ask`. |
| **Read a whole series, not a sample** | A CT is twelve hundred images and a reader is given sixteen. Sampling is honest and stated in `limitations`, and it is not the same as reading the study. |

## Media, and the AI over it

**The floor is built; the capabilities are not.** [`media/`](media/) is a second
artefact, `aura-media`, with its own version line: an asset in, an address out.
It arrived from NAAT, which contracted a video CDN and a WebRTC server rather
than wait for it — the right call, and it does not remove the need. Four
capabilities that product needs are all AI over video, every one of them needs
frames, and **none of the four is built**:

| Capability | Why the kernel is the right place |
|---|---|
| **De-identify a clip before it is published** | Faces and on-screen text out of a video a clinician recorded in a consulting room. It has a human who approves and an artefact to seal, so it is a `logical.*` effect and not a filter. It is also the one that forces the hard half: it **produces a new video asset**, so this subsystem encodes, not just decodes. |
| **Transcribe and subtitle** | `sensorial.asr.transcribe` already exists for dictation. Pointing it at a published video needs the audio demuxed, and buys accessibility plus a searchable body of content. |
| **Draft a moderation judgement** | A health network without a moderation criterion is a misinformation platform with badges. A draft that cites what an assertion contradicts, with a person deciding behind it, is exactly the gate pattern. Needs sampled keyframes and the transcript. |
| **Poster and alt text** | Suggest, with a person confirming. Frames again. |

**Build it here, run it beside the kernel — not inside it.** Sharing the code is
free; sharing the process is what costs. Three reasons, and none is style:

- **Opposite load curves.** Approving an effect is milliseconds and must never
  queue; encoding is a core pinned for minutes. One process means a video upload
  can starve a clinician waiting to sign a note.
- **Zone crossing.** The consuming product keeps network media and patient data
  in separate processes on purpose, and the kernel is where PHI dictation is
  handled. Public video bytes do not belong in it — and when the kernel *does*
  cross the two, it should be at the de-identification gate, where the crossing
  is declared and approved.
- **The pin stops being governable.** Consumers pin one commit so they can say
  which kernel sealed a note. If libav ships in the same artefact, every codec
  CVE forces a kernel version bump — and a bump is meant to be a deliberate act
  with a changelog to read. **So this ships as a second artefact with its own
  version line**, and a consumer's pin file grows a second entry rather than one
  entry quietly meaning two things.

**What exists.** A job queue with `FOR UPDATE SKIP LOCKED` and N workers, where
a claim is a lease and a worker that dies loses its job rather than taking it
with it; ffmpeg for decode and encode; an HLS ladder that never upscales, with
keyframes aligned across rungs; still frames and a poster out of that same
decode, because every capability above needs frames and decoding twice costs
twice; an object store whose sources are served to nobody. It registers as
`logical.media.transcode` (C1 `format: projection`), so a graph can call it
without the encode entering the kernel.

**What is left in the floor.** An S3-compatible store beside the filesystem one
— the interface is there, the implementation is not. Shaka Packager where
ffmpeg's HLS muxer stops being enough. Per-job progress, rather than a state
that only moves when the job ends.

**LiveKit belongs in the same subsystem, and is not started.** It is what NAAT contracted for live
and for teleconsultation, and it already speaks the two directions this needs:
Ingress accepts RTMP and WHIP, Egress hands back composed frames and HLS. Real
time in, frames out, one integration — and it is what makes AI over a live
stream possible at all, which is the product line this whole section is for.

## Standards work

| | Status |
|---|---|
| **[Signed human approval](spec/proposals/draft-signed-human-approval.md)** | At `-01`, not submitted. The gap it fills is real: the existing IETF agent audit-trail draft records a pseudonymous operator id with no signature. `-01` adds the `context` member — digests of what the approver was shown — which is the half that turns "a named person clicked" into "a named person consented to this document", and which the kernel implements as C4 v1.7. Submitting is what turns a local design into a claim on the category. |
| **[MCP effect receipts](spec/proposals/mcp-effect-receipts.md)** | Written, not proposed upstream. |

## Test coverage, where it is thin and why it matters

Measured, not remembered — the numbers are in
[Milestone status](GUIDE.md#milestone-status).

- **`cmd/aura` at 8.8%.** Argument parsing and output formatting over logic
  tested where it lives. The honest outlier, and low risk.
- **`gateway` 54%, `store` 54%, `projection` 59%, `grammar` 52%.** The
  guarantees these implement are covered thoroughly; the accessor error paths
  around them are not.
- **The UI is tested at the model layer only.** That layer is now most of the
  logic — the graph model against the same C2 conformance vectors the kernel
  uses, the editing operations, the bypass, the conversation fold, the frames
  and every translation key the source asks for. What has nothing is the part
  that touches a browser: the components, the audio capture path and the
  session views.

  The split is deliberate rather than a plan half-executed. Anything that can
  produce IR the kernel would reject is pure and tested; pointer arithmetic and
  CSS are verified by driving a real node and looking. Both bugs the i18n suite
  was written for got past review and shipped, which is the argument for moving
  more of the browser half into something mechanical.
- **The seams between components are the real gap.** Every pre-existing bug found
  in the last round of work — WebTransport serving `/ws/skill` unauthenticated,
  an open path promoting a scoped credential, `/metrics` public because a rule
  matched a path shape, `store.Open` not creating its own directory — sat between
  two components that were each well tested. The conformance suite exercises
  contracts and the unit tests exercise packages; nothing exercised the joins.

## Deliberately not doing

- **A hosted control plane.** The neutrality argument only works if the registry
  and the witness are things you can host yourself.
- **A learned policy router.** Policy is meant to be readable at a glance by
  someone auditing it. A model that decides what gets gated is not.
- **Embedding vendor TEE roots in the binary.** A root that cannot be rotated
  fails closed at the worst moment or open at the wrong one.
