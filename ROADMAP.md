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
| **Node failover** | A node is one process. If it dies, live sessions die with it — the event log survives, the routing state does not. Answer honestly first: if AURA falls over, does your app degrade or does it stop? If it degrades, this is already deployable. | Large. Session state has to become recoverable by a second process, which touches the executor's in-memory indexes and the admission model. |
| **Backup and restore, documented** | The data directory holds the node identity, the effect ledger and the broker's encrypted secrets — and the broker's key is *derived* from the identity, so a restore without `identity/` yields ciphertext nobody can open. There is no written procedure for this, which means the first person to need one will be writing it during an incident. | Small. A documented procedure, a `aura backup`/`aura restore` pair, and a test that restores into a fresh node and verifies the ledger. |
| **Node key rotation** | The signing key is forever. An operator who suspects compromise has no move that does not invalidate every checkpoint. | Medium. Needs a key-succession record in C4 so old checkpoints still verify under the old key. |

## Blocking adoption

The engineering is ahead of the distribution by a wide margin, and that gap is
the actual problem.

| | Why it blocks | Shape of the work |
|---|---|---|
| **Publish the repository** | The README says `git clone` against a URL that returns 404. Nothing here exists for anyone yet. | Hours. |
| **Publish the SDKs** | `@deepaxiom/aura` and `aura-sdk` are packaged and unpublished. A full-stack team following [the integration guide](GUIDE.md#when-the-app-is-yours) cannot `npm install` it — they vendor it from the repo. | Hours: npm and PyPI, plus a CI job that publishes on tag. |
| **Publish binaries and a container image** | CI cross-compiles six targets and uploads them as artifacts with a seven-day retention, so a release is not downloadable. The Dockerfile is written and has never been built by CI. | Hours: a release job, and `docker build` in CI so the image is proven rather than described. |
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

## Standards work

| | Status |
|---|---|
| **[Signed human approval](spec/proposals/draft-signed-human-approval.md)** | Written as an Internet-Draft, not submitted. The gap it fills is real: the existing IETF agent audit-trail draft records a pseudonymous operator id with no signature. Submitting is what turns a local design into a claim on the category. |
| **[MCP effect receipts](spec/proposals/mcp-effect-receipts.md)** | Written, not proposed upstream. |

## Test coverage, where it is thin and why it matters

Measured, not remembered — the numbers are in
[Milestone status](GUIDE.md#milestone-status).

- **`cmd/aura` at 7.5%.** Argument parsing and output formatting over logic
  tested where it lives. The honest outlier, and low risk.
- **`gateway` 56%, `store` 56%, `projection` 59%, `grammar` 52%.** The
  guarantees these implement are covered thoroughly; the accessor error paths
  around them are not.
- **The UI has no tests at all.**
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
