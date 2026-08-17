# Effect receipts for MCP tool results

**Status:** proposal · **Reference implementation:** shipping, see below ·
**Author:** Deep Axiom · **Depends on:** nothing new

## The gap

MCP answers two questions well: *which tools exist* and *what did this one
return*. It has nothing to say about the third, which is the one that matters
once an agent stops summarising documents and starts touching systems:

> **What did that call do, on whose authority, and can either be shown
> afterwards?**

Today a tool call that issues a refund and a tool call that reads a customer
record are the same shape on the wire: arguments in, content out. Whether the
first was authorized, by which policy, approved by which human, and whether the
record of it has been edited since — none of that is expressible. It is not that
MCP answers it badly; there is nowhere to put the answer.

The workarounds all sit outside the protocol and all have the same defect. A
gateway logs the call, an observability vendor records the span, a wrapper
writes to an audit table. Each produces a record *the caller has to be trusted
to have written honestly*, held somewhere the agent's output does not point at,
and correlated back after the fact — which in practice means never, because the
correlation is only ever attempted during an incident, when the transcript and
the log have already drifted apart.

## The proposal

**A tool result MAY carry receipts for the effects the call produced, in
`_meta`.**

```jsonc
{
  "content": [{ "type": "text", "text": "Invoice INV-4417 created." }],
  "_meta": {
    "org.deepaxiom/effects": [
      {
        "receipt":    "sha256:9f2a1c…",
        "capability": "motor.api.erp.create_invoice",
        "decision":   "gate",
        "outcome":    "delivered",
        "approver":   "grace"
      }
    ]
  }
}
```

| Field | Meaning |
|---|---|
| `receipt` | Content address of a sealed, tamper-evident record of this effect. Opaque to MCP. |
| `capability` | What was invoked, in the server's own vocabulary. |
| `decision` | What authorized it: allowed by policy, or gated for a human. |
| `outcome` | What became of it: delivered, or refused. |
| `approver` | Who approved, when a human did and signed for it. Absent otherwise, and the absence is meaningful. |

Five short strings. Nothing else changes: not the transport, not `tools/list`,
not `tools/call`'s request shape, not any existing field.

## Why this shape

**`_meta`, not a new top-level field.** `_meta` is the extension point the
protocol already defines, with reverse-DNS namespacing so several vendors can
attach several things without colliding. A client that ignores it sees exactly
what it sees today.

**A receipt, not the record.** What travels is a hash. That keeps the result
small, keeps it safe to log and to paste into a ticket, and — the reason that
actually matters — keeps payloads out of it. A field that carried the effect's
contents would put personal data into agent transcripts, which is the opposite
of what an audit mechanism should do. The receipt is a pointer to evidence, not
a copy of it.

**Optional, and silent when there is nothing to say.** A read-only tool
produces no effects and no `_meta`. Absence means "this server does not seal
effects" or "this call had none" — never "something was hidden".

**Verifiable without the server.** The point of a content address is that a
third party can check the record it names without asking the party that
produced it. A server implementing this should be able to hand someone a
receipt and a verifier and have them reach a conclusion with the server
switched off. Anything weaker is a log with extra steps.

## What this is deliberately not

- **Not an audit-log format.** How a server seals effects, what its records
  contain, and what it signs them with are entirely its business. This
  proposal standardises one thing: that a result can *point at* such a record.
  A server backed by a hash chain, a WORM bucket or a notary service all fit.
- **Not authentication or authorization.** It says nothing about who may call a
  tool. It describes what happened after somebody did.
- **Not mandatory.** Servers that seal nothing are unaffected; clients that
  ignore `_meta` are unaffected.
- **Not a trust claim by itself.** A receipt from a server you have no reason to
  trust proves that server committed to a record. Whether that is worth
  anything depends on what else the record is anchored to — which is outside
  this proposal, and correctly so.

## What a client can do with it

The minimum is to keep it: store the receipt beside the transcript so an
incident review six months later has a thread to pull. That alone is more than
exists today.

Beyond that: surface the approver in a UI so a reviewer sees *who* allowed an
action rather than that one was allowed; refuse to continue a plan whose last
effect came back `denied`; carry receipts into a support ticket so the customer
and the vendor are looking at the same evidence.

## Reference implementation

Implemented and shipping in the Deep Axiom kernel, both halves:

- **As a server** — every skill is an MCP tool, and a `motor.*` call returns its
  receipts in `_meta`. `internal/mcpsrv`.
- **As a client** — `aura guard` fronts the MCP servers an agent already calls,
  so tool calls pass a policy, a human-approval gate and a ledger without the
  agent being rewritten. `internal/guard`.

The records behind the receipts are hash-chained, committed to an
[RFC 6962](https://datatracker.ietf.org/doc/html/rfc6962) Merkle head the
server signs, and independently counter-signed by third-party witnesses whose
own logs are public and auditable. `aura receipt --verify <file>` checks one
with no database, no server and no network. The full contract is
[`spec/c4-ledger.md`](../c4-ledger.md); none of it is required to adopt this
proposal, and it is offered only as evidence that the shape works end to end
rather than as the shape everyone must implement.

## Open questions

1. **Namespace.** `org.deepaxiom/effects` while this is one vendor's extension.
   If the idea is adopted, an unprefixed `effects` under an MCP-owned namespace
   would be better, and this reference implementation would follow it.
2. **Batching.** One call can produce several effects. Modelled here as an
   array, in the order they were sealed. An alternative is one receipt covering
   the whole call, which is simpler and loses the ability to say which of three
   writes was the one a human refused.
3. **Progress notifications.** Long-running tools stream progress; whether an
   effect sealed mid-call should be reported before the result, rather than
   only with it, is worth deciding rather than leaving to implementations.
4. **Elicitation.** When a server elicits approval from the user through the
   client rather than out of band, the client is the one holding the human. A
   future extension could let the client return a *signed* approval, making the
   client's user the named approver. That is a larger change and is mentioned
   only so the design does not preclude it — the shape above already carries an
   `approver`, and nothing here assumes the server found them.

## Why it is worth doing now

Agents are moving from reading to acting faster than the mechanisms for holding
them accountable are arriving, and the regimes now being written — the EU AI
Act's record-keeping obligations among them — will ask for evidence about
individual actions, not aggregate logs. The protocol that carries the actions is
the natural place to carry a pointer to that evidence, and it costs one optional
field.

The alternative is not that nothing gets built. It is that every vendor builds
it in a private namespace, and an auditor ends up correlating five formats by
timestamp.
