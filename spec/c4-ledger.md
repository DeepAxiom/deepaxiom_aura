# C4 — Effect Ledger & Policy (frozen contract)

**Protocol major: 1 · Status: v1.1 — FROZEN (2026-08-02: `compensates` added to the ledger entry — additive, Phase 2, `aura undo`; base v1.0 frozen 2026-08-01, initial release, Phase 1). Changes: additive only; breaking = new major via RFC.**

C1, C2 and C3 answer *what a skill is*, *how a graph is wired*, and *how
envelopes flow*. None of them answer the question a node actually has to
answer before it lets an effect happen: **who authorized this, and can it be
proven afterward?** That is C4.

Two documents make up this contract: the **policy** a node loads at startup
(`aura.policy.yaml` — see [`c1-manifest.md`](c1-manifest.md) for `compensates`,
the C1 field this contract reads), and the **ledger** the node writes to as it
runs. Read [security-model in the README](../README.md#security-model) for
the policy's operator-facing shape; this document specifies the ledger entry
and the guarantees a conforming implementation MUST provide.

## The Effect Checkpoint

Every edge that delivers into a skill of type `motor` (C1 — the type whose
edges act on the world) passes through one point in the executor, once, on
every delivery:

```
envelope → [ Effect Checkpoint ] → skill
             1. classify   is the destination motor.*?
             2. authorize  policy → allow | gate | deny   (C4's policy half)
             3. attest     append to the ledger → receipt (C4's ledger half)
             4. deliver    with the receipt attached (C3, additive)
```

A non-effect delivery (`sensorial`, `cognitive`, `memory`, `logical`) is not
sealed. The ledger is evidence of effects, not a second copy of the causal
event log C3 rule 7 already requires — sealing every token of an LLM reply
would make the ledger a data lake, which is exactly what it is designed not to
be.

## The ledger entry

```json
{
  "seq": 42,
  "prev": "sha256:9f2a…",
  "ts": 1754083200000,
  "node": "node-a1b2c3d4",
  "session": "sess-8f3a",
  "envelope": "01J9ZK3V7Q…",
  "cause": "01J9ZK2M1P…",
  "actor": "acme/motor/erp-writer@1.2.0",
  "capability": "motor.erp.invoice.create",
  "decision": "gate",
  "outcome": "delivered",
  "policy": "sha256:7c1e…",
  "payload_sha256": "b5b3…",
  "compensation": { "capability": "motor.erp.invoice.create", "port": "undo_in", "schema": "acme/erp-undo@1" },
  "compensates": "sha256:1a2b…"
}
```

| Field | Norm |
|---|---|
| `seq` | Monotonic, gap-free, starting at 1. The node's own counter — not derived from any session's `seq` (C3), which restarts per edge. |
| `prev` | The previous entry's `hash` (see below), or `""` for the entry at `seq: 1`. This is what makes the entries a chain rather than a list. |
| `ts` | Unix milliseconds, when the entry was sealed. |
| `node` | The sealing node's id (identity.Node.ID). |
| `session` | The C3 session the effect belongs to. |
| `envelope` / `cause` | The id of the envelope that carried the effect, and its `cause_id` — the join back into the causal event log (C3 rule 7), so `aura why` and the ledger describe one history from two angles. |
| `actor` | `org/category/name@version` of the skill package that produced the effect. The version is mandatory: "which build did this" is the first question an incident review asks. |
| `capability` | The C1 capability delivered into. |
| `decision` | `allow` \| `gate` \| `deny` — what the policy ruled. `deny` never reaches the ledger: the session is refused before it starts (see below), so there is nothing to seal. |
| `outcome` | `delivered` \| `denied` — what happened after. A `gate` decision that a human later refuses is **one** entry, sealed once resolution is known, not two. |
| `policy` | The hash of the policy document in force (the same value a node prints at startup) — an auditor can find the exact document that authorized this. |
| `payload_sha256` | sha256 of the delivered payload. **Never the payload itself** — the ledger is evidence, not a data lake, and a payload carrying personal data must not become permanently undeletable. |
| `compensation` | Present only when the skill declared C1 `compensates`; carries its capability, port and schema. Absent (not null-valued — omitted) means the effect was recorded as irreversible. |
| `compensates` | Present only on an entry that IS an undo: the `hash` (see below) of the entry it reverses. Absent on every ordinary effect. A conforming implementation MUST refuse to seal a second entry with the same `compensates` value — an undo is a one-time action, not a repeatable one (see [`ROADMAP.md`](../ROADMAP.md), Phase 2, `aura undo`). |

### Hash

An entry's identity in the chain is:

```
hash(entry) = sha256(canonical_json(entry))
```

over every field above (`prev` included — that is what makes each hash depend
on everything before it). A conforming implementation's JSON encoding MUST be
deterministic for a fixed entry — field order fixed by the language's
struct/record representation is sufficient; entries have no map-typed field,
so no additional key-sorting canonicalization is required the way C1 manifest
hashing needs one for arbitrary YAML.

## What `deny` does

A `deny` decision is resolved **before** a session exists: `NewSession` (the
executor's session-construction step) refuses to build a session that wires an
edge into a capability the policy denies. There is no envelope, no delivery,
and therefore nothing to seal — the refusal itself is visible in the ordinary
event log and in the error the caller receives, not in the ledger. The ledger
records effects that were *authorized*, gated-and-approved, or gated-and-later-
denied; it does not record capabilities a node's policy ruled out categorically.

## Checkpoints

Chaining alone proves an entry is self-consistent with its neighbors; it does
not prove nobody with write access to the underlying storage rewrote a whole
suffix of the chain to stay self-consistent. That is what a checkpoint is for.

Every **N entries or T seconds, whichever comes first**, a node signs its
current head:

```json
{ "seq": 100, "head_hash": "sha256:9f2a…",
  "pubkey": "base64…", "signature": "base64…", "ts": 1754083260000 }
```

The signed payload is domain-separated and MUST be exactly:

```
"aura-ledger-checkpoint-v1:" + seq + ":" + head_hash
```

so a checkpoint signature can never be replayed as, or confused with, a
package-publish signature (C1's `aura publish`, which uses a different domain
prefix) even though both use the same node's — or a different identity's —
Ed25519 key mechanism.

N and T are implementation choices, not part of the wire contract: a verifier
checks that every checkpoint present *does* verify, not that checkpoints arrive
at any particular cadence. A reference value (100 entries or 60 seconds) is
documented in [`ROADMAP.md`](../ROADMAP.md) for context, not normatively here.

## Verification

A conforming verifier, given the ledger's storage and the node's public key,
MUST:

1. Recompute `hash(entry)` for every entry in `seq` order and confirm
   `entry[n].prev == hash(entry[n-1])`, with `entry[1].prev == ""`.
2. For every checkpoint, confirm the entry at its `seq` recomputes to its
   `head_hash`, and confirm the signature over the checkpoint payload verifies
   against `pubkey`.
3. Report the chain unsound if either check fails anywhere — a chain that
   recomputes cleanly but disagrees with a checkpoint's signed head is
   evidence of exactly the "rewrite a suffix" attack checkpoints exist to
   catch, and MUST NOT be reported as intact.

Verification MUST be possible from storage alone, with no running node and no
access to any private key — only the node's public key, which is not a
secret. This is the property that makes the ledger evidence rather than logs:
an operator, an auditor, or a court does not have to trust the process that
wrote the log, only the math.

## What this contract deliberately does not specify

- **The policy's rule language.** `aura.policy.yaml`'s shape (`default_effect`,
  ordered `rules`, `match`, `decision`, `limit`) is documented in the README
  and enforced by the kernel; C4 only requires that a node cite, in `policy`,
  a hash identifying whichever document authorized a sealed entry. A future
  node could use a different policy language entirely and still speak C4.
- **Compensation execution.** `aura undo` (Phase 2) walks the ledger and
  re-delivers an effect's original payload to the skill's declared
  compensation port — that behavior is scoped to the runtime, not to this
  wire contract. C4 only requires that the resulting entry, if one is sealed,
  carry `compensates` pointing at what it reversed; how a caller decides
  *which* entries to undo, in what order, or how it recovers the original
  payload (from the causal event log, C3 rule 7) is runtime behavior, not
  part of this contract.
- **Storage.** Nothing here requires SQLite, or any particular database. The
  guarantees are about the entries and their hashes, not about how they are
  kept durable.
