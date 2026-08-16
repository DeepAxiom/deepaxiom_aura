# C5 — Inference Attestation (frozen contract)

**Protocol major: 1 · Status: v1.0 — FROZEN (2026-08-15, initial release, Phase 4).
Changes: additive only; breaking = new major via RFC.**

C4 answers *what acted on the world, and who authorized it*. It does not answer
the question that comes next in any real incident review: **what produced the
judgement that led to the act?**

A C4 entry saying `motor.erp.invoice.create` was delivered under policy `7c1e…`
is evidence about the effect and says nothing about the model whose output
argued for it — which model, which weights, which quantization, which sampling
parameters, which seed. Those are the variables that decide whether a bad
outcome was a policy failure, a prompt failure, or a silently swapped model.

C5 is one small record and one binding rule.

---

## What this proves, and what it does not

**Stated first, because everything below is worthless if this is misread.**

An attestation is **an assertion by a skill**, cryptographically bound to the
effects it led to and to the moment it was made. It is **not** proof that the
skill told the truth. A skill that lies about which model it ran produces a
sealed, chained, signed record of a lie.

What a conforming implementation guarantees is narrower, and still useful:

| Guaranteed | Not guaranteed |
|---|---|
| The claim cannot be altered after the fact | That the claim was true when made |
| The claim cannot be detached from the effects it caused | That the model named is the model that ran |
| The claim cannot be backdated or reordered | That no other model also contributed |
| A verifier needs neither the node nor its operator | Anything about the skill's internal honesty |

Closing the remaining gap requires hardware attestation — a TEE quote binding
a measurement of the process that actually executed the model. The `tee` field
is reserved for exactly that and is absent on every implementation today. Its
absence is what keeps this contract in the "assertion" column.

Implementations MUST NOT describe C5 output as proof of what executed. The
phrase this specification uses, and that tooling should echo, is: *who claimed
what, when, and what it caused — verifiable; whether the claim was true — only
as far as you trust the skill that made it.*

---

## The attestation record

A skill MAY attach one attestation to any envelope it emits (C3 `attest`,
additive). The record is JSON:

```json
{
  "engine": "llama.cpp",
  "engine_version": "b4321",
  "model": "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
  "model_revision": "f1d2d2f924e986ac86fdf7b36c94bcdf32beec15",
  "model_file": "qwen2.5-1.5b-instruct-q4_k_m.gguf",
  "model_sha256": "9f2a…",
  "quantization": "Q4_K_M",
  "params": { "temperature": 0.7, "seed": 42, "max_tokens": 512 },
  "prompt_sha256": "b221…",
  "output_sha256": "9853…",
  "energy": { "millijoules": 4120.5, "source": "nvml" },
  "tee": null
}
```

| Field | Norm |
|---|---|
| `engine` | **Required.** The runtime that executed the inference (`llama.cpp`, `faster-whisper`, `openai-compatible`). An attestation that does not say what ran it is not evidence of anything. |
| `engine_version` | Optional. For a hosted API with no version to name, implementations MAY carry the endpoint here instead — it is the closest available provenance and distinguishes two providers behind one model name. |
| `model` | **Required.** The model's identity in the engine's namespace. For anything from a model hub this SHOULD be the repo id (`org/name`), which is what makes `model_revision` meaningful. |
| `model_revision` | The immutable revision the weights came from — a commit sha. A repo id alone names a moving target; the pair names one set of bytes. Implementations that download weights SHOULD resolve and pin the revision **before** fetching, so the value describes what was actually retrieved rather than whatever `main` pointed at afterwards. |
| `model_file` / `model_sha256` | Pin the artifact for the local case, where weights are a file rather than a hub reference. |
| `quantization` | Separated from the file name because it changes the output distribution and is the most common undeclared difference between "the same model" in two places. |
| `params` | Free-form JSON: the sampling configuration actually used, including `seed`. Free-form because no contract should have to model every engine's knobs; hashed verbatim, so the record commits to exactly what was sent. |
| `prompt_sha256` / `output_sha256` | Bind the record to its input and output **without storing either**. Same rule as C4's `payload_sha256`: user data must not become permanently undeletable. |
| `energy` | Optional. See below. |
| `tee` | Reserved for a hardware attestation quote. Absent means the record is an assertion. |

### Size

An attestation MUST be rejected if it exceeds **8192 bytes**. It carries model
identity and sampling parameters — never prompts, never outputs, never
retrieved context. The cap is what stops the field becoming an unbounded,
permanently-retained side channel into a node's storage.

### Energy

```json
{ "millijoules": 4120.5, "source": "nvml", "basis": "" }
```

`source` is **required** whenever `energy` is present, and MUST be one of
`nvml`, `rapl`, `powermetrics`, `estimated`. `basis` is required for
`estimated` and MUST state the inputs in one line (`"14.2s wall x 45W assumed
package draw"`).

This is not decoration. A measured joule and a modelled joule differ by an
order of magnitude in trustworthiness, and a field carrying only the number
would launder an estimate into a measurement the first time it reached a
sustainability report. A consumer that accepts only hardware counters can
filter on `source`; one that accepts estimates knows what it is accepting.

---

## Content addressing

```
attestation_hash = "sha256:" + hex(sha256(raw_attestation_bytes))
```

Over the **raw bytes as received**, not over a re-serialization of a parsed
record. Any canonicalization step is a place where the producer's encoder and
a verifier's encoder can diverge, and `params` and `tee` are free-form JSON
where that risk is concrete rather than theoretical.

Two records that parse identically but differ byte-for-byte therefore have
different hashes. This is intended: the hash commits to what was sent.

A conforming implementation MUST store each distinct record once, keyed by its
hash, and MUST re-verify the content address on read. The attestation store
sits outside C4's hash chain, so its integrity rests entirely on the hashes
sealed entries cite — checking on read is what makes that binding
load-bearing rather than decorative.

---

## The binding rule

> **When an effect is sealed, its C4 entry MUST cite every attestation
> produced anywhere in that effect's causal chain.**

C4's entry gains one additive field:

```json
"inference": ["sha256:3a7f…", "sha256:9c21…"]
```

| Rule | Norm |
|---|---|
| Scope | Every attestation in the effect's causal chain (C3 `cause_id` lineage), not only the immediately preceding hop. An ASR transcription and an LLM reply are siblings, and a mis-transcribed amount is as consequential as a mis-reasoned one. |
| Ordering | The array MUST be sorted and de-duplicated. The entry hash is over the marshalled entry, so an unstable order would produce a hash no verifier could reproduce; sorting makes the field a function of the *set*, which is what it means semantically. |
| Absence | Omitted entirely when no attested inference contributed. Absence is meaningful — a webhook that fires a write directly genuinely had no model behind it — and is **not** the same as "unknown". |
| Denied effects | An effect a human refused MUST cite its inferences too. "Which model kept proposing the payment a human kept refusing" is a question the ledger must be able to answer. |
| Truncation | An implementation MAY bound how many distinct attestations one chain accumulates. If it truncates, it MUST record that it did. An entry citing a partial set while appearing complete is worse than one citing none. |
| Malformed input | A malformed attestation MUST be dropped, not fatal: routing continues and the resulting effect cites one fewer inference. A metadata defect must not be able to take down a working graph, and an unfixable dangling hash in an immutable chain is worse than an explicable gap. |

---

## Verification

Given a C4 entry and the attestation records it cites, a conforming verifier
MUST:

1. Recompute each record's content address and confirm it equals the cited
   hash.
2. Report a cited hash with no corresponding record as **unresolved**, not as
   a failure of the effect's own proof. The effect remains proven; what is
   missing is the basis.
3. Never report an attestation as establishing what executed. See the first
   section.

Verification MUST be possible from a portable receipt alone — no running node,
no database, no network. See [`c4-ledger.md`](c4-ledger.md) for the receipt
document that carries an entry, its inclusion proof, the signed head and the
attestations it cites.

---

## What this contract deliberately does not specify

- **Which skills should attest.** Any skill MAY. Restricting it to `cognitive`
  would be wrong: a `sensorial` ASR skill's model matters to a downstream
  effect for exactly the same reason.
- **Attestation cadence.** A streaming skill SHOULD attest once per logical
  reply rather than once per token — the record describes the inference, and
  implementations de-duplicate identical records anyway — but nothing here
  makes that normative.
- **The TEE quote format.** Reserved, not defined. Binding it will be
  additive: a quote is evidence *about* an attestation, and the record already
  has a field for one.
- **Storage.** Nothing here requires SQLite or any particular database. The
  guarantees are about records, hashes and the citation rule.
