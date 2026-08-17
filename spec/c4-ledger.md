# C4 — Effect Ledger & Policy (frozen contract)

**Protocol major: 1 · Status: v1.4 — FROZEN (2026-08-16: the witness's own published log, signed `last-seen`, and `log_seq` on a countersignature — all additive; v1.3 2026-08-16 added the `approver` signature — additive; v1.2 2026-08-15 added the Merkle tree head, portable receipts, external witnessing and the `inference` citation — all additive, Phase 4; v1.1 2026-08-02 added `compensates` — additive, Phase 2, `aura undo`; base v1.0 frozen 2026-08-01, initial release, Phase 1). Changes: additive only; breaking = new major via RFC.**

C1, C2 and C3 answer *what a skill is*, *how a graph is wired*, and *how
envelopes flow*. None of them answer the question a node actually has to
answer before it lets an effect happen: **who authorized this, and can it be
proven afterward?** That is C4.

Two documents make up this contract: the **policy** a node loads at startup
(`aura.policy.yaml` — see [`c1-manifest.md`](c1-manifest.md) for `compensates`,
the C1 field this contract reads), and the **ledger** the node writes to as it
runs. Read [security-model in the guide](../GUIDE.md#security-model) for
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
| `inference` | v1.2, additive. The sorted, de-duplicated content addresses of every C5 attestation produced in this effect's causal chain — what the models that argued for this act claimed about themselves. Omitted when none contributed. See [`c5-attestation.md`](c5-attestation.md) for the record, the citation rule, and — importantly — the limits of what it proves. |
| `approver` | v1.3, additive. The signed identity of the human who resolved this effect's gate. Present only on an entry whose gate was answered with a verified signature; omitted otherwise. See "The approver" below. |

## The approver (v1.3)

This contract opens by asking *who authorized this, and can it be proven
afterward?* Through v1.2 it answered only the first half. An entry cited the
**policy** that authorized the class of effect and recorded that a gate was
resolved — but *which human* resolved it existed nowhere, and "a human
approved" without "which human" is a log, not an audit trail.

It was also unprovable in principle, which is the more serious half. The node
writes its own entries, so a node that wished to claim an approval had happened
could simply write one. Every other guarantee in C4 rests on a signature the
node cannot forge *on someone else's behalf*; approval had none.

```json
{
  "operator": "grace",
  "pubkey":   "base64…",
  "envelope": "01J9ZK2M1P…",
  "decision": "approve",
  "ts":       1754083200000,
  "sig":      "base64…"
}
```

The signed payload is domain-separated, like every other signature in this
contract:

```
"aura-approval-v1:" + node + ":" + session + ":" + envelope + ":" + decision + ":" + ts
```

| Field | Norm |
|---|---|
| `operator` | The enrolled identity that answered. A **claim**, not what verification trusts — see the roster rule below. |
| `pubkey` | Base64 Ed25519 public key whose private half produced `sig`. Carried inline, not looked up: an entry MUST remain verifiable from stored data alone, and an id resolved against a mutable roster would make history depend on the roster's present state. |
| `envelope` | The delivery the operator was shown — the envelope the executor held at the gate. Recorded rather than inferred because the entry's own `envelope` field is not the same id on both paths: a delivered effect seals the outbound envelope the gate released (whose `cause` is the held one), a denied effect seals the held envelope itself. |
| `decision` | `approve` \| `deny`. Deliberately not the `policy_decisions` vocabulary: that is what a *policy* ruled about a class of effect, this is what a *person* answered about one delivery. |
| `ts` | Unix millis at signing, inside the signed bytes so an approval cannot be backdated without invalidating itself. |
| `sig` | Base64 Ed25519 over the payload above. |

Every component is inside the signature, so a valid approval cannot be
transplanted: not to another node, another session, another delivery, another
decision, or another time.

**Two checks, deliberately separated.** A conforming implementation MUST make
both, and MUST NOT merge them:

1. **Authorship** — does `sig` verify against `pubkey` over the payload? Pure
   cryptography, answerable forever from stored data, with no roster.
2. **Enrollment** — at the moment the gate is answered, is `operator` enrolled
   on this node, not revoked, and enrolled *under this exact `pubkey`*? Anyone
   may mint a keypair and sign as "grace"; only this check refuses it.

Enrollment is evaluated **once, when the answer arrives, and never again.**
Revoking an operator MUST NOT invalidate approvals they already gave. An audit
trail that changes when the org chart changes is not an audit trail.

**Sealing rules.** A conforming implementation:

- MUST verify an approval before sealing it, and MUST refuse to seal one that
  does not verify — an entry cannot be edited or withdrawn afterward, so this
  is the last moment a forgery can be stopped.
- MUST refuse a resolution whose signed `decision` disagrees with the transport
  that carried it, rather than preferring either. The signature covers the
  decision precisely so an intercepted `deny` cannot be forwarded as an
  `approve` by editing an unauthenticated field beside it.
- MUST seal the approver on a **denial** as well as on a delivery. "Who refused
  this" is as much a fact an incident review needs as who allowed it.
- MUST NOT hold any operator private key. A node that could sign on an
  operator's behalf could manufacture the evidence it is audited by, which
  reduces the field to decoration.

**Absence is ambiguous, and the entry does not disambiguate it.** A missing
`approver` means either the effect was never gated (`decision: allow` — nobody
was asked) or it was gated and answered by a client that does not sign. A node
that needs the second case to be impossible sets `require_signed_approval` in
its policy, after which an unsigned answer to a gate is a **denial**, not a
downgrade — an enforcement that can be skipped by omitting a field enforces
nothing. Such a node MUST refuse to start with an empty roster, since every
gated effect would otherwise be unanswerable.

`aura verify` checks approver signatures during the same offline walk it uses
for the chain, and an approval that no longer verifies makes the ledger
**unsound** — it is direct evidence that an entry was edited after sealing, or
that one was written claiming a human said yes when none did. A ledger with no
approvals at all is not unsound; it is unsigned, which is a weaker claim and is
reported as such.

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
{ "seq": 100, "head_hash": "sha256:9f2a…", "merkle_root": "sha256:4b8c…",
  "pubkey": "base64…", "signature": "base64…", "ts": 1754083260000 }
```

The signed payload is domain-separated. Two versions exist, and **which one
applies is decided by the data, not by a flag**:

```
v1 (no merkle_root)  "aura-ledger-checkpoint-v1:" + seq + ":" + head_hash
v2 (with merkle_root) "aura-ledger-checkpoint-v2:" + seq + ":" + head_hash + ":" + merkle_root
```

A conforming implementation MUST emit v2. It MUST also still verify v1
checkpoints, which is what lets a node upgraded in place keep every signature
it ever made instead of orphaning its own history.

Because the version lives inside the signed bytes, a v2 signature can never be
re-presented as a v1 signature over the same seq and head: stripping the
Merkle root does not yield a valid v1 checkpoint, it yields one that fails.

The domain separation also means a checkpoint signature can never be replayed
as, or confused with, a package-publish signature (C1's `aura publish`, which
uses a different prefix) or a witness countersignature (below), even though
all three may use the same Ed25519 key.

## The Merkle tree (v1.2)

The chain proves the whole ledger to someone holding the whole ledger. That is
the wrong shape for the common case: an auditor handed **one** receipt and
asked "was this effect really sealed by that node, in that history?" should
not need the entire database — which is both a privacy problem (every other
effect comes along) and an availability one.

A conforming implementation MUST therefore also maintain an **RFC 6962**
Merkle tree over its entries, and MUST commit to its head in every checkpoint.

```
leaf(n)      = SHA256(0x00 || entry_json(n))
interior(l,r)= SHA256(0x01 || l || r)
MTH({})      = SHA256()
MTH(D[n])    = interior(MTH(D[0:k]), MTH(D[k:n])),  k = largest power of 2 < n
```

RFC 6962 rather than a hand-rolled tree, for three reasons: it is the most
analysed append-only log construction available; it defines both proof types
below against one tree shape; and its `0x00`/`0x01` domain separation closes
the second-preimage attack a naive tree has, where an interior node can be
presented as a leaf.

`leaf(n)` MUST be computed over the **stored entry bytes**, byte for byte, so
a verifier rebuilding the tree from storage cannot diverge from the sealer on
a JSON encoding detail.

The linear `prev` chain is unchanged. This is strictly additive: pre-v1.2
entries rehash identically, and a v1 checkpoint carrying no root stays valid.

### Inclusion proofs

An **inclusion proof** (RFC 6962 audit path) establishes that entry *i* sits at
that position in a tree of size *n* whose head has been signed. It is
`⌈log₂ n⌉` sibling hashes and discloses nothing about any other entry.

### Consistency proofs

A **consistency proof** establishes that the tree of size *m* is a strict
prefix of the tree of size *n* — nothing inserted, reordered or dropped
between them. This is what witnessing (below) rests on.

## External witnessing (v1.2)

Self-signing catches an attacker who edits storage without the node's key. It
does not catch the key's holder, who can rewrite history and re-sign the
result: the chain recomputes, every signature verifies, and nothing on disk
records that it said something else an hour ago.

A **witness** is a second party that remembers what it was already shown.

```
1. node → witness   signed head (seq n, root R_n) + consistency proof from
                    the last size m that witness vouched for
2. witness          verifies the node's signature, then the proof:
                    "the m entries I already vouched for are still, unchanged
                    and in order, a prefix of these n"
3. witness → node   countersignature, or a refusal
```

The witness's countersignature payload MUST be exactly:

```
"aura-ledger-witness-v1:" + witnessed_node_id + ":" + seq + ":" + merkle_root
```

The node id is included so a countersignature obtained for node A can never be
replayed as vouching for node B.

A conforming witness:

- MUST verify the presented head's own signature before anything else.
- MUST refuse when a consistency proof from its last recorded size does not
  verify. There is no proof to forge here — it either exists, because history
  really is an extension, or it does not.
- MUST refuse a presented size smaller than one it already vouched for.
- MUST record what it vouched for **before** returning the countersignature.

The resulting property: a node can still lie, but it cannot lie *consistently
to two parties over time*. Passing off a rewritten history would require every
witness to forget, simultaneously, what it had already signed.

**What this does not claim.** Countersignatures stored by a node are stored by
that node, which can drop the inconvenient ones. A report of "0 witnesses" is
therefore not proof that none were issued; the authoritative copy is the
witness's own record. Implementations MUST NOT present their own witness count
as complete.

## The witness's own log (v1.4)

v1.2 answered "can a node rewrite its own history?" — no, because a witness
remembers. It left the next question open, and it is the one anyone evaluating
a shared witness asks immediately: **who watches the witness?**

Through v1.3 the answer was nobody. A witness kept one record per node,
replaced as that node advanced. A replaced record says nothing about what it
used to say, so a witness could quietly revise what it had vouched for and no
one could demonstrate it — which makes a witness a party you have to *trust*,
the exact thing this contract exists to stop needing.

A conforming witness therefore **MUST** maintain its own append-only log:

- **Every countersignature it issues is appended**, before the signature is
  returned to the caller, and never updated or deleted. A signature issued but
  not published is one the witness could later deny having made, which is the
  single move that would let a split view go unpunished. A witness that cannot
  append MUST refuse to counter-sign rather than return an unlogged signature.
- **The log has an RFC 6962 tree**, and the witness MUST serve its **signed
  head** — size, root, its own key, a timestamp, and a signature over them.
- **It MUST serve consistency proofs** over that log, so a follower can check
  the history it verified last week is still a prefix of this week's.
- **It SHOULD serve inclusion proofs**, so a node holding a countersignature
  can show a third party the anchor was *published*, not handed over privately.
  `log_seq` on a countersignature (additive) names the position to ask about.

These routes carry heads, roots, keys and signatures — **never a payload,
capability, session or operator**. A witness must have nothing to leak, or the
organisations that most need one cannot use it.

### Signed `last-seen`

A node asks a witness how far it has already vouched for it, before building a
consistency proof. A conforming witness **MUST** sign that answer, and the
signature **MUST** cover the witness's own log size and root at the moment of
answering.

```
"aura-witness-lastseen-v1:" + witness_key + ":" + node + ":" + seq + ":"
    + merkle_root + ":" + log_size + ":" + log_root + ":" + ts
```

This is the load-bearing requirement of v1.4. **Unsigned, two different answers
to two parties are two rumours; signed, they are two statements over one key
that cannot both be true.** A split view stops being undetectable and becomes
self-incriminating. "Never seen this node" is also a signed answer — a witness
must be holdable to a denial, or denial is the one free lie.

An implementation SHOULD provide a way to compare two such statements and
report an irreconcilable pair. Contradiction means: the same witness, about the
same node, giving different answers at the same log size; or reporting a
smaller position later than it reported earlier.

### The follower's baseline

The value of all of this is in what a *follower* keeps. A witness that publishes
a head and later publishes a smaller or incompatible one is caught by whoever
retained the earlier head, and by nobody else. An implementation that follows a
witness therefore SHOULD persist each verified head, MUST verify a head's
signature before recording it, and MUST NOT lower a recorded baseline — a
follower that can be talked into forgetting is one that can be talked into
forgetting the evidence.

## The portable receipt (v1.2)

A conforming implementation SHOULD be able to emit, for any sealed effect, a
self-contained document carrying:

| Part | Why |
|---|---|
| the sealed entry, verbatim | what happened, under what authority |
| an inclusion proof | that it is at that position in a tree of that size |
| the signed checkpoint | the node's commitment to that tree head |
| any witness countersignatures | third parties that saw the same head |
| the cited C5 attestation records | what the models upstream claimed |

A verifier given only this document MUST be able to check all of it — no
database, no network, no running node, no key material beyond what the
document carries. That property is the difference between evidence that can be
shared and evidence that requires granting someone server access.

A receipt anchors to the **earliest** checkpoint covering the entry, because
that is the oldest signed commitment naming it and therefore the strongest
available statement about when the node committed.

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
   against `pubkey` — selecting the v1 or v2 payload by whether the checkpoint
   carries a `merkle_root`.
3. For every checkpoint carrying a `merkle_root`, recompute the tree head over
   the first `seq` entries and confirm it matches. A validly-signed root the
   entries do not produce means the node signed a history different from the
   one stored, which the linear chain check alone can miss: `prev` relates each
   entry only to its neighbour, while the tree commits to all of them at once.
4. Report the chain unsound if any of the above fails anywhere — a chain that
   recomputes cleanly but disagrees with a checkpoint's signed head is
   evidence of exactly the "rewrite a suffix" attack checkpoints exist to
   catch, and MUST NOT be reported as intact.

A verifier SHOULD also check any stored witness countersignatures, and MUST
treat a countersignature vouching for a head the entries no longer produce as
invalid. It MUST NOT fold witness counts into the soundness verdict: zero
witnesses means unwitnessed, which is a weaker claim, not a corrupt ledger —
conflating them would make every fresh node report itself unsound.

Verification MUST be possible from storage alone, with no running node and no
access to any private key — only the node's public key, which is not a
secret. This is the property that makes the ledger evidence rather than logs:
an operator, an auditor, or a court does not have to trust the process that
wrote the log, only the math.

**The scope of a passing verification, stated precisely.** Everything above is
checked against the sealing node's own key. It establishes that nobody altered
the ledger *without* that key. It does not establish that the key's holder did
not. Only witness countersignatures narrow that, and an implementation
reporting a clean verification SHOULD say which of the two it has
established rather than leaving a bare "sound" to be over-read.

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
