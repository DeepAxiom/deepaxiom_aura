---
title: "Signed Human Approval Records for AI Agent Audit Trails"
abbrev: "Signed Human Approval"
docname: draft-belen-signed-human-approval-01
category: info
submissiontype: independent
ipr: trust200902
area: Security
workgroup: Independent Submission
keyword: [audit, agents, non-repudiation, human-in-the-loop]
stand_alone: yes
pi: [toc, sortrefs, symrefs]
author:
  -
    ins: D. Belen
    name: Diego Belen
    organization: Deep Axiom
    email: diego.belen.js@gmail.com
normative:
  RFC2119:
  RFC8174:
  RFC8032:
  RFC4648:
informative:
  RFC6962:
  I-D.sharif-agent-audit-trail:
  EU-AI-ACT:
    title: "Regulation (EU) 2024/1689 laying down harmonised rules on artificial intelligence"
    target: https://eur-lex.europa.eu/eli/reg/2024/1689/oj
    date: 2024
---

# Abstract

Audit trails for autonomous AI agents record that a human intervened in an
agent's decision, but not, in any way a third party can check, *which* human
did so. The record is written by the same system whose behaviour is under
review, so it establishes only that the system asserts an approval occurred.

This document defines a **signed human approval record**: a small structure,
produced with a key the recording system never holds, that binds one named
approver to one specific agent action, and optionally to digests of the material
that approver was shown. It is transport-independent and format-independent, and
is intended to occupy the place existing agent audit formats reserve for human
intervention.

--- middle

# Introduction

An AI agent that acts on the world — issuing a payment, modifying a record,
sending a message — is increasingly required to pause and obtain human approval
before doing so. Regulatory frameworks assume this control exists and assume it
is evidenced; {{EU-AI-ACT}} Article 14 requires human oversight for high-risk
systems and Article 12 requires the automatic recording of events over their
lifetime.

Existing agent audit formats record the intervention. {{I-D.sharif-agent-audit-trail}},
for example, defines an optional `human_override` object carrying an operator
identifier, a reason, and the original action. Records may carry a signature,
but that signature is made by the *agent*.

This produces an audit trail with a specific and load-bearing gap:

> The only evidence that a human approved an action is the word of the system
> being audited.

That is precisely the claim an audit cannot rest on. Every other property such
a trail provides — tamper-evidence, ordering, hash chaining — protects the
record against modification *after* it was written. None of them constrains
what the system wrote in the first place. A system able to fabricate an
approval produces a perfectly chained, perfectly signed, tamper-evident record
of an approval that never happened, and no amount of downstream verification
distinguishes it from a real one.

## Scope

This document specifies:

- a record binding one human approver to one agent action ({{the-record}});
- the exact bytes that are signed, and why they are those bytes
  ({{signed-payload}});
- the separation between verifying a signature and authorizing an approver
  ({{roster}});
- verification requirements ({{verification}}).

This document does **not** specify: how an approval is requested, how an
approver is enrolled, key distribution, or the surrounding audit log format. It
is designed to be carried inside an existing one.

## Requirements Language

{::boilerplate bcp14-tagged}

# Design Rationale

## The key must not be held by the system under review

The central requirement is negative: an approval record is worth something only
if the party whose behaviour it excuses could not have produced it.

This rules out the natural implementation. A system that signs approvals with
its own key — even a separate, dedicated, well-protected key — has produced a
record it could have produced without any human present. The signature proves
the record was made by that system, which was never in doubt.

It therefore follows that the approver holds a private key the recording system
never possesses. This is the entire mechanism; everything else is detail. It
also implies the corollary that makes the mechanism deployable: the recording
system needs only the public key, so enrolling an approver requires no secret
to be transported to the system at all.

## Identity must be specific, and that has a privacy cost

An approval that names a role ("an operator") rather than a person restores the
original problem in a smaller form: it establishes that *somebody holding the
role key* approved, which is a weaker claim than it appears when the key is
shared.

{{I-D.sharif-agent-audit-trail}} takes the opposite position, requiring that
operators be identified by "pseudonymous identifier or role, not a natural
person's name", on data-minimisation grounds. That tension is real and this
document does not dismiss it. The resolution proposed here is that the
*identifier* may be pseudonymous while the *key* is not shared: a per-person key
under a pseudonymous label gives non-repudiation without publishing a name, and
the mapping from label to person is held wherever the deployment's privacy
regime says it should be. What a conforming implementation MUST NOT do is issue
one key to several people, because that converts a signature into a shared
secret and silently voids the property the record exists to provide.

## Binding must be to one action, not to a session

A signature over "I approve" is replayable against any action. A signature over
a session is replayable against every action in it. The payload defined in
{{signed-payload}} therefore includes an action identifier, and a verifier
checks it against the action the record is attached to.

## Binding to an action does not say what was displayed

An identifier binds a record to one action. It does not describe how that action
was put to the person, and the two can differ without any component
malfunctioning: the system renders a summary, the person reads the summary, and
the identifier in the record is the same either way.

This is not a hypothetical failure of an honest deployment so much as the
cheapest available attack on an audited one. A record that binds only an
identifier is satisfied by a rendering that says anything at all, which makes
the display the unaudited part of an otherwise audited path.

{{shown}} therefore defines an optional member carrying digests of the material
the approver was shown. It is optional because a deployment whose actions are
self-describing does not need it, and because a record made before this member
existed remains valid and unchanged.

# The Approval Record {#the-record}

An approval record is a JSON object with the following members.

~~~ json
{
  "system":   "node-a1b2c3d4",
  "session":  "sess-8f3a",
  "action":   "01J9ZK3V7Q8M2N",
  "operator": "grace",
  "pubkey":   "MCowBQYDK2VwAyEA…",
  "decision": "approve",
  "ts":       1754083200000,
  "context":  [{ "label": "screen", "digest": "sha256:4b8c…" }],
  "alg":      "Ed25519",
  "sig":      "3n1Wq…"
}
~~~

| Member | Type | Requirement |
|---|---|---|
| `system` | string | REQUIRED. Identifier of the system that requested the approval. |
| `session` | string | REQUIRED. Identifier of the conversation, run or workflow. MAY be the empty string where the deployment has no such concept. |
| `action` | string | REQUIRED. Identifier of the one action being approved. MUST be unique within `system`. |
| `operator` | string | REQUIRED. The approver's identifier within the deployment. MAY be pseudonymous; MUST NOT be shared between people. |
| `pubkey` | string | REQUIRED. The approver's public key, base64 {{RFC4648}} Section 4. |
| `decision` | string | REQUIRED. `"approve"` or `"deny"`. |
| `ts` | number | REQUIRED. Time of signing, milliseconds since the UNIX epoch. |
| `context` | array | OPTIONAL. Digests of the material the approver was shown. See {{shown}}. |
| `alg` | string | OPTIONAL. Signature algorithm; absent means `"Ed25519"`. |
| `sig` | string | REQUIRED. Signature over {{signed-payload}}, base64. |

A `"deny"` decision is as much a record as an approval and MUST be retained.
"Which model kept proposing the payment a human kept refusing" is a question an
incident review asks, and a trail that retains only approvals cannot answer it.

## Algorithms

Implementations MUST support Ed25519 {{RFC8032}}. Implementations MAY support
other signature algorithms, indicated by `alg`. A verifier that does not
recognise `alg` MUST treat the record as unverified rather than as valid.

# What the Approver Was Shown {#shown}

The `context` member is an array of at most 8 objects, each with two string
members:

| Member | Requirement |
|---|---|
| `label` | REQUIRED. What kind of material this is. MUST match `^[a-z][a-z0-9_]{0,31}$`. |
| `digest` | REQUIRED. `<algorithm>:<lowercase hex>`, of a digest of at least 256 bits. MUST match `^[a-z0-9][a-z0-9-]{0,15}:[0-9a-f]{64,128}$`. |

Labels MUST be unique within one record. A verifier encountering a duplicated
label MUST treat the record as unverified rather than resolving it: a record
asserting that one thing was two things has no meaning, and choosing between
them would be the verifier deciding what a person approved.

The label vocabulary is deliberately not enumerated. What a person must be shown
before authorising an act is specific to the deployment — a rendered document, a
diff, a beneficiary, a consent form — and an enumeration here would be this
document guessing at domains it does not know. What is fixed is the shape, so
that one tool can read records from unrelated deployments.

## Collision resistance is the property

The member asserts that material the signer held hashes to the given value, so
its worth is bounded by the difficulty of producing a second artifact with the
same digest. An attacker who can do that can display the acceptable one and act
on the other, and every signature in the record still verifies.

Implementations MUST NOT accept a digest shorter than 256 bits, and SHOULD
prefer an algorithm with no published collision. The length bound above enforces
the first; the second cannot be enforced by a pattern, since an algorithm's
standing changes after its identifier is chosen.

## Digests, never content

`digest` MUST be a cryptographic digest of the material, and the material itself
MUST NOT appear in the record. {{signed-payload}} already forbids the action's
payload in the signed string; the same reasoning applies here and one more does
besides. An approval record is durable by design and frequently unerasable
({{privacy}}), and the material most worth binding — a clinical document, a
statement, a photograph — is the material most likely to carry personal data. A
digest binds it without republishing it.

The bound of 8 entries is not a size limit. It is a limit on how much one person
can be said to have examined in a single decision; a record binding forty
artifacts to one act describes a review that did not take place.

## The recording system MUST NOT supply it

A digest produced by the system under review is a digest of whatever that system
wished it had displayed, and adds nothing an action identifier did not already
establish. Implementations MUST compute `context` on the approver's side, from
the material actually rendered to them, and MUST NOT accept it from, or generate
it within, the recording system.

This follows the same logic as key custody ({{security-considerations}}): the
value of the record comes entirely from what the audited party could not have
produced alone.

# The Signed Payload {#signed-payload}

The signature is computed over the UTF-8 encoding of the following string,
with no trailing newline:

~~~
"aura-approval-v1:" system ":" session ":" action ":" decision ":" ts
~~~

where `ts` is the decimal representation of the `ts` member with no leading
zeros or sign.

When `context` is present and non-empty, the string continues:

~~~
... ":" ts ":" context-digest
~~~

where

~~~
context-digest = "sha256:" lowercase-hex(SHA-256(canonical))
canonical      = entries sorted by label, each rendered as label "=" digest,
                 joined by LF (U+000A), with no trailing newline
~~~

An absent or empty `context` produces the shorter string exactly, so a record
made before this member existed verifies unchanged, and an implementation that
never emits it is unaffected.

Four properties of this construction are deliberate.

**It is domain-separated.** The `aura-approval-v1:` prefix ensures a signature
made for this purpose cannot be presented as one made for another. An approver
key used elsewhere in a deployment cannot have one of its signatures replayed
here, and vice versa.

**It is a string, not a canonicalised object.** Signing a JSON object requires
every implementation to agree on canonical encoding, which is a well-known
source of interoperability failure and of signature-bypass vulnerabilities. A
concatenation of five values whose formats are fixed above has one encoding.

**The context enters it as a single value, reduced injectively.** `context` is a
structure, and admitting it directly would reintroduce exactly the canonical
encoding problem the previous paragraph avoids. Reducing it to one digest keeps
the signed input a flat string, and the reduction has one encoding of its own:
the charsets in {{shown}} exclude `=` from labels and LF from both members, so
`canonical` splits back into exactly one list of pairs. Those charsets are
normative for that reason and not as input hygiene. Sorting is part of the
reduction, so the array order a record happens to carry is not part of what was
signed; an implementation SHOULD store the entries in that order once verified.

The result is not strippable in either direction. Removing `context` from a
record that carried one leaves a signature made over the longer string, which
then fails to verify; attaching one to a record that carried none fails the same
way. Both directions fail closed.

**It excludes `pubkey` and `sig`.** Including the key would be circular.
Excluding it means the key travels beside the signature and is checked against
the roster ({{roster}}) rather than trusted from within the record.

Implementations MUST NOT include the approved action's payload in the signed
string. An approver signs the identifier of an action, not its contents; a
payload of unbounded size in the signed input makes the signature expensive to
verify and creates an incentive to truncate it.

# Verification and Authorization Are Separate {#roster}

A verifier answers two questions that MUST NOT be collapsed:

1. **Did this operator sign this record?** Pure cryptography over the record.
   Answerable forever, by anyone holding the record, with no access to the
   system that produced it.
2. **Was that operator permitted to approve on this system?** Mutable state,
   answerable only against the system's roster of enrolled approvers, and
   meaningful only at the moment the approval was given.

Collapsing them makes history depend on the present. If question 2 is
re-evaluated at audit time, removing an employee retroactively invalidates
every action they legitimately approved, and an audit trail whose contents
change when the organisation chart changes is not one.

Implementations MUST therefore:

- check enrollment **when the approval is received**, and record the outcome;
- record the **key** in the approval record, not a reference to a roster entry;
- treat a later revocation as ending the operator's ability to approve
  *thereafter*, with no effect on records already made.

# Verification Requirements {#verification}

A verifier presented with an approval record and the action it is attached to
MUST perform all of the following, and MUST treat the record as unverified if
any fails:

1. `action` equals the identifier of the action the record is attached to.
2. `decision` is `"approve"` or `"deny"`.
3. `alg` is recognised, or absent.
4. `context`, if present, is well-formed under {{shown}}: every `label` and
   `digest` matches its pattern, no label repeats, and there are at most 8
   entries.
5. `sig` verifies over {{signed-payload}} using `pubkey`.
6. `system` and `session` match the circumstances the record is presented in.

A verifier with access to the system's roster SHOULD additionally check that
`pubkey` is the key that system enrolled for `operator`. Without this check, an
attacker who can write to the log can record an approval attributed to `grace`
signed by a key they generated themselves, and every cryptographic check above
passes. This check is what makes the `operator` member meaningful.

A verifier that holds the material the record claims was shown SHOULD recompute
its digest and compare. A verifier that does not hold it MUST NOT treat the
record as unverified on that account: `context` establishes what the signer
committed to, and whether the deployment can still produce that material is a
property of the deployment's storage, not of the record.

Verifiers MUST NOT infer anything from the absence of an approval record. A
record's absence is consistent with an action that required no approval, an
approval given through an unsigned channel, and an approval that was removed.
Distinguishing these is a property of the enclosing log, not of this record.

# Relationship to Existing Work

This record is designed to be carried, not to stand alone.

In {{I-D.sharif-agent-audit-trail}}, it fits inside `human_override` as an
additional member; the existing `operator_id` and `reason` members are
unaffected, and a consumer that does not understand the signature is unaffected
by its presence.

In a log whose entries are hash-chained or committed to a Merkle tree
{{RFC6962}}, the record SHOULD be included in the hashed content of the entry
it belongs to. The two mechanisms are complementary and neither substitutes for
the other: the chain establishes that the record has not been altered since it
was written, and the signature establishes that the system did not write it
alone.

# Security Considerations {#security-considerations}

**Key custody is the whole security boundary.** An approver whose private key is
held by, accessible to, or recoverable by the recording system provides no
guarantee whatsoever. Implementations SHOULD hold approver keys in hardware
tokens or platform keystores, and MUST NOT provide any interface by which the
recording system can obtain one.

**A shared key voids the mechanism.** One key issued to several people converts
non-repudiation into a shared secret. Implementations MUST refuse to enroll a
key already enrolled for a different operator.

**Coercion and delegation are out of scope.** This record establishes that the
holder of a key signed a statement. It does not establish that they understood
it, were free to refuse, or were the person the key was issued to.

**`context` binds the material, not its fidelity.** A digest establishes that the
signer held material hashing to that value and committed to it. It does not
establish that the material was a faithful rendering of the action: no verifier
can check that, because the rendering is produced outside the record. What the
member changes is that a discrepancy becomes demonstrable — a verifier holding
both the action and the material can show they disagree — where previously
there was nothing in the record to disagree with.

**A required context must be required, not requested.** A deployment that needs
this evidence MUST reject an approval that omits the labels it requires, rather
than accepting it and noting the omission. An implementation that can be
satisfied by leaving a member out is satisfied by leaving it out.

**Timestamps are asserted by the signer.** `ts` is part of the signed payload
and so cannot be altered afterwards, but a signer may state any value. Where
ordering matters, it MUST be established by the enclosing log rather than by
`ts`.

**Replay is bounded by `action`, not prevented.** A record remains valid for its
action forever, by design — that is what makes it evidence. An implementation
that reuses action identifiers makes a past approval reusable; identifiers MUST
be unique within `system`.

**Denial of an approval that occurred.** A system that discards approval records
it dislikes produces a log with a gap. This document does not address that; it
is the property an append-only, externally witnessed log provides, and is the
reason {{RFC6962}}-style publication is recommended above.

# Privacy Considerations {#privacy}

An approval record links a natural person to a specific act at a specific time,
and is durable by design. Two consequences follow.

`operator` MAY be a pseudonymous identifier, and the mapping to a natural person
held separately under the deployment's own controls. This preserves
non-repudiation — the key is still per-person — while keeping the name out of a
record that may be shared with third parties.

A record is not erasable without breaking whatever integrity mechanism encloses
it. Deployments subject to erasure obligations SHOULD ensure that the record
contains no personal data beyond the identifier and key, which is why this
document places no free-text member in the signed structure — including in
`context`, whose values are constrained to digests for exactly this reason
({{shown}}).

# IANA Considerations

This document has no IANA actions.

--- back

# Implementation Status

An implementation of this record exists in the Deep Axiom kernel, where it is
sealed into a hash-chained effect ledger and verified both at the moment of
approval and by an offline verifier reading the stored log alone. `context`
({{shown}}) is implemented there as of contract version C4 v1.7, including the
deployment-side requirement described in {{security-considerations}}: a node may
name the labels an approval must bind, after which an answer that omits one is a
refusal rather than a downgrade. The `aura-approval-v1` domain prefix in
{{signed-payload}} is that implementation's; a version of this document adopted
by a working group would be expected to change it.

# Acknowledgments
{:numbered="false"}

The separation in {{roster}} between verifying a signature and authorizing an
approver was arrived at after observing that the natural implementation — check
the roster at audit time — makes an audit trail's contents depend on current
staffing.
