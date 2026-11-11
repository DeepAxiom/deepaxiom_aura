# C3 — Channel Protocol (frozen contract)

**Protocol major: 1 · Status: v1.7 — FROZEN (2026-11-11: optional `trace` field added, carrying a W3C Trace Context `traceparent` along a causal chain, see [Trace context](#trace-context-joining-a-callers-trace) — additive, and absent from every envelope of a chain whose client supplied none, so a v1.6 peer sees an unchanged wire; 2026-08-15: optional `deadline`, `priority` and `speculative` fields added, carrying [C2](c2-graph-ir.md) v1.2 scheduling intent on the wire — additive; 2026-08-15: optional `attest` field added, carrying a [C5](c5-attestation.md) inference attestation — what the emitting Expertise asserts about how it produced the payload — additive, wire format otherwise unchanged; 2026-08-01: optional `receipt` field added, carrying a [C4](c4-ledger.md) ledger entry's hash on an envelope that sealed an effect — additive, wire format otherwise unchanged; 2026-08-01: QoS rule 4 now specifies what each class does under pressure and is enforced, and rule 7 permits payload elision on `realtime` edges — wire format unchanged; 2026-08-01: `cancel` widened from single-hop best-effort to whole-chain with kernel-side suppression, see [Cancel](#cancel-abandoning-a-chain); 2026-07-30: `config_update` kind added; base v1.0 frozen 2026-07-14). Changes: additive only; breaking = new major via RFC.**

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
| `kind` | `data` · `done` (logical stream close) · `error` · `status` · `register` · `confirm_request` · `confirm_response` · `cancel` (best-effort abort request, see below) · `config_update` (kernel→Expertise: runtime config changed live, see below) |
| `payload` | Content conforming to the schema |
| `receipt` | Optional ([C4](c4-ledger.md)). Present only on an envelope the kernel delivered into a `motor.*` Expertise: the hash of the ledger entry that sealed that effect. Absent on every envelope that carries no effect — most of them. A receiving Expertise or client MAY ignore it; a client that wants proof an effect was attested, not merely delivered, looks it up via `GET /v1/ledger` or `aura verify`. |

| `trace` | Optional (v1.7). A W3C Trace Context `traceparent`: `00-<32 hex>-<16 hex>-<2 hex>`, lowercase. See [Trace context](#trace-context-joining-a-callers-trace). |

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
whether any Expertise notices the cancel: an Expertise that ignores `cancel` entirely
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
why` can show work an Expertise did after being asked to stop.

**2. Propagation — best-effort.** The kernel SHOULD also ask every Expertise
known to be working on the chain to stop, at any depth, not just the first
hop. Each such `cancel` MUST carry the `cause_id` that *that* Expertise will
recognise — the id of whatever the kernel delivered to it — not the id the
client named. An Expertise three hops down never saw the client's envelope id;
a cancel carrying it would be silently ignored, which is the failure mode
this rule exists to prevent.

An Expertise that does not recognise `cancel` ignores it, per rule 2 applied to
unknown kinds — no Expertise update is required. An Expertise that does recognise it
MAY stop at its own next convenient checkpoint; it is never required to stop
mid-instruction. Honouring it is an optimisation: it stops the Expertise burning
time on output that would be discarded anyway, which is what frees capacity
for whatever the client asked for instead.

*Changed in v1.2. The wire format is byte-identical; v1.1 specified
single-hop propagation and no suppression. This is a widening — every client
that conformed to v1.1 still conforms, it simply receives less stale output
than the old text allowed.*

## Trace context (joining a caller's trace)

```json
{ "v": "1", "id": "...", "kind": "data", "trace": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "...": "..." }
```

The causal log is already a trace — every envelope names its cause. What it
could not do was join the trace a caller already has: a request that starts in
someone's backend and ends as an effect in a node's ledger was two trees that
had to be lined up by timestamp. `trace` closes that.

1. **A client MAY set `trace`** on an envelope that starts a chain. The value
   is a W3C `traceparent`, version `00`, lowercase hex, with a non-zero trace
   id and parent id.
2. **A kernel MUST carry it** on every envelope it forwards in that chain, at
   any depth, including the envelopes it delivers back to the client. It MUST
   do so from its own record of the chain and not rely on Expertises echoing the
   field: every Expertise built before v1.7 does not, and a chain that lost its
   context at the first old Expertise would be worse than no feature.
3. **An Expertise SHOULD echo** the `trace` of the delivery that caused an emission.
   This is what keeps the context where a kernel's bounded per-session index no
   longer holds the chain's root.
4. **A malformed value MUST be dropped, never refused.** Tracing is never the
   reason a delivery fails. A kernel MUST NOT propagate a value that does not
   match the grammar: the field is copied onto every hop and handed to an
   exporter, and accepting anything string-shaped would make it a side channel
   into both.
5. **Absent means absent.** A chain whose client supplied no context carries no
   `trace` key on any envelope. Nothing is minted on the wire; an exporter that
   needs an id for such a chain derives one (the reference kernel hashes the
   session id, as it did before this field existed).

Over an HTTP ingress route the standard `traceparent` request header is the
same thing as rule 1.

`trace` is opaque to routing. It takes no part in ordering, deduplication,
authorization or sealing, and it is not evidence of anything: it is a caller's
label, carried faithfully.

## Expertise registration (first message on connect)

```json
{ "v": "1", "id": "...", "kind": "register", "payload": { /* full C1 manifest as JSON */ } }
```

The kernel replies `{ "kind": "status", "payload": { "state": "registered", "skill": "<id>", "config": { /* effective runtime config, see C1 */ } } }`
or `{ "kind": "error" }` with the cause (invalid manifest, unknown schema, permission).

## Config update (kernel → Expertise, additive)

```json
{ "v": "1", "id": "...", "kind": "config_update", "payload": { /* full new effective config, same shape as the register ack's config */ } }
```

Sent on an Expertise's own connection when a runtime config value it declared
(C1 `config`) changes — via `PUT /v1/skills/config` (UI or any HTTP client)
— while that Expertise is connected. Payload is always the *complete* effective
config, not a diff, so an Expertise can just replace what it's holding. An Expertise
that doesn't recognize `config_update` (every Expertise built before this
addition) ignores it, same additive rule as `cancel`: no Expertise update is
required for this to be safe to receive.
