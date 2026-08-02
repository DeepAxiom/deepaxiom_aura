# C3 — Channel Protocol (frozen contract)

**Protocol major: 1 · Status: v1.6 — FROZEN (2026-08-15: optional `deadline`, `priority` and `speculative` fields added, carrying [C2](c2-graph-ir.md) v1.2 scheduling intent on the wire — additive; 2026-08-15: optional `attest` field added, carrying a [C5](c5-attestation.md) inference attestation — what the emitting skill asserts about how it produced the payload — additive, wire format otherwise unchanged; 2026-08-01: optional `receipt` field added, carrying a [C4](c4-ledger.md) ledger entry's hash on an envelope that sealed an effect — additive, wire format otherwise unchanged; 2026-08-01: QoS rule 4 now specifies what each class does under pressure and is enforced, and rule 7 permits payload elision on `realtime` edges — wire format unchanged; 2026-08-01: `cancel` widened from single-hop best-effort to whole-chain with kernel-side suppression, see [Cancel](#cancel-abandoning-a-chain); 2026-07-30: `config_update` kind added; base v1.0 frozen 2026-07-14). Changes: additive only; breaking = new major via RFC.**

Everything that flows through the system travels as an **envelope** over a typed channel.
Conforms to `schemas/envelope.schema.json`.

## Envelope

```json
{
  "v": "1",
  "id": "01J9ZK3V7Q...",
  "cause_id": "01J9ZK2M1P...",
  "session": "sess-8f3a",
  "node": "eco",
  "port": "text_out",
  "seq": 7,
  "idem": "sess-8f3a:eco:text_out:7",
  "schema": "std/text@1",
  "kind": "data",
  "payload": { "text": "hello" }
}
```

| Field | Norm |
|---|---|
| `v` | Protocol major. Handshake negotiates; a peer at N speaks to a kernel at N-2 |
| `id` | Unique ULID of the message |
| `cause_id` | `id` of the message that caused this one. **Mandatory** except for root messages (client input, registration). The event log is a causal graph |
| `session` | Session this message belongs to |
| `node` / `port` | Logical origin within the graph (`client` for the client pseudo-node) |
| `seq` | Monotonic counter per (session, node, port). Guarantees gap detection |
| `idem` | Idempotency key. **At-least-once** delivery: receivers MUST deduplicate by `idem` |
| `schema` | Payload schema (`ns/name@major`) |
| `kind` | `data` · `done` (logical stream close) · `error` · `status` · `register` · `confirm_request` · `confirm_response` · `cancel` (best-effort abort request, see below) · `config_update` (kernel→skill: runtime config changed live, see below) |
| `payload` | Content conforming to the schema |
| `receipt` | Optional ([C4](c4-ledger.md)). Present only on an envelope the kernel delivered into a `motor.*` skill: the hash of the ledger entry that sealed that effect. Absent on every envelope that carries no effect — most of them. A receiving skill or client MAY ignore it; a client that wants proof an effect was attested, not merely delivered, looks it up via `GET /v1/ledger` or `aura verify`. |

## Normative semantics

1. **Ordering:** FIFO per channel (origin→destination pair). No guarantee across different channels.
2. **Delivery:** at-least-once. Every handler MUST be idempotent (dedup by `idem`).
3. **Backpressure:** mandatory. The sender respects the transport's credit window; over WS, the kernel applies a bounded buffer and blocks (QoS `reliable`) or drops the oldest (QoS `realtime`).
4. **Declarable QoS per channel:** `reliable` (default) · `realtime` · `bulk`.
   - `reliable` — the sender is blocked by a bounded buffer until the receiver
     catches up. Nothing is lost; everything waits.
   - `realtime` — the sender is never blocked. When the buffer is full the
     **oldest** frame is discarded, because in a live stream a stale frame is
     the worthless one. A voice channel that buffers is a voice channel that
     cannot be interrupted.
   - `bulk` — named but not yet specified. Implementations MUST treat it as
     `reliable` until it is, so nothing is silently lost against an
     unspecified rule.

   Envelopes that are terminal or explanatory — `done`, `error`, `status`,
   `confirm_request` — MUST travel reliably whatever the edge declares.
   Dropping one leaves a consumer waiting forever for something that already
   happened.
5. **Negotiated transport:** the control plane picks the best route (same process → LAN → QUIC/WebRTC → kernel relay). Data does NOT transit the kernel when a direct route exists. (H1 implements relay only; negotiation arrives with federation, same envelope.)
6. **Metering:** the channel edge counts messages and bytes ALWAYS. Billing is userland; the counting is not.
7. **Causality:** kernels MUST persist every envelope in the session's event log. `aura why` and replay derive from this.

   Every *envelope*, not every byte. On a `realtime` edge a kernel MAY replace
   the payload with a description of it — `{ "elided": true, "bytes": n,
   "sha256": "…" }` — keeping the header and the causal links intact. A minute
   of speech is megabytes of base64 in the log and a permanent recording of
   someone talking sitting on disk; the causal chain is what explains a session,
   the samples are not. Keyed on the edge's declared class rather than the
   payload's schema, so a graph that declares an audio edge `reliable` is
   asking for a recording and gets one.

## Cancel (abandoning a chain)

```json
{ "v": "1", "id": "...", "cause_id": "<id of the message whose chain to abandon>", "kind": "cancel" }
```

A client sends `cancel` naming, as `cause_id`, an envelope in the chain it
wants abandoned — normally a `data` envelope it originated, but naming any
envelope of that chain abandons the whole chain.

Two things follow, and they are deliberately different in strength.

**1. Suppression — a guarantee.** The kernel marks the chain cancelled and
MUST NOT route anything further belonging to it. This holds regardless of
whether any skill notices the cancel: a skill that ignores `cancel` entirely
keeps working, and everything it emits is dropped at the kernel. A client
therefore does **not** need its own discard-by-provenance logic to be safe
from stale output. (Envelopes already handed to the transport before the
cancel may still arrive — suppression acts at routing, not on bytes in
flight — so a latency-sensitive client may still want to discard by
provenance for the last few frames.)

Cancelling MUST be scoped to the named chain. A session carrying several
concurrent chains — a second utterance, a vision stream, a background
memory write — MUST be unaffected.

Suppressed envelopes are still recorded in the causal event log (rule 7).
The log is a record of what happened, not of what was delivered, so `aura
why` can show work a skill did after being asked to stop.

**2. Propagation — best-effort.** The kernel SHOULD also ask every skill
known to be working on the chain to stop, at any depth, not just the first
hop. Each such `cancel` MUST carry the `cause_id` that *that* skill will
recognise — the id of whatever the kernel delivered to it — not the id the
client named. A skill three hops down never saw the client's envelope id;
a cancel carrying it would be silently ignored, which is the failure mode
this rule exists to prevent.

A skill that does not recognise `cancel` ignores it, per rule 2 applied to
unknown kinds — no skill update is required. A skill that does recognise it
MAY stop at its own next convenient checkpoint; it is never required to stop
mid-instruction. Honouring it is an optimisation: it stops the skill burning
time on output that would be discarded anyway, which is what frees capacity
for whatever the client asked for instead.

*Changed in v1.2. The wire format is byte-identical; v1.1 specified
single-hop propagation and no suppression. This is a widening — every client
that conformed to v1.1 still conforms, it simply receives less stale output
than the old text allowed.*

## Skill registration (first message on connect)

```json
{ "v": "1", "id": "...", "kind": "register", "payload": { /* full C1 manifest as JSON */ } }
```

The kernel replies `{ "kind": "status", "payload": { "state": "registered", "skill": "<id>", "config": { /* effective runtime config, see C1 */ } } }`
or `{ "kind": "error" }` with the cause (invalid manifest, unknown schema, permission).

## Config update (kernel → skill, additive)

```json
{ "v": "1", "id": "...", "kind": "config_update", "payload": { /* full new effective config, same shape as the register ack's config */ } }
```

Sent on a skill's own connection when a runtime config value it declared
(C1 `config`) changes — via `PUT /v1/skills/config` (UI or any HTTP client)
— while that skill is connected. Payload is always the *complete* effective
config, not a diff, so a skill can just replace what it's holding. A skill
that doesn't recognize `config_update` (every skill built before this
addition) ignores it, same additive rule as `cancel`: no skill update is
required for this to be safe to receive.
