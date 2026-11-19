# C2 — Graph IR (frozen contract)

**Protocol major: 1 · Status: v1.3 — FROZEN (2026-11-19: `edges[].respond_ms` and rule 9, a responsiveness target distinct from a deadline — additive; rule 7 now states how a deadline is inherited and that a late effect is sealed as `expired`, which is what the rule already required and no implementation did; v1.2 2026-08-15: `edges[].speculative`, `edges[].deadline_ms`, `edges[].priority` and `context_budget` — all additive and all defaulting to prior behaviour, Phase 5; v1.1 2026-08-01 added the motor-gate invariant and `gate: "none"` as its explicit opt-out; base v1.0 frozen 2026-07-14). Changes: additive only; breaking = new major via RFC.**

The IR (intermediate representation) is the ONLY graph format the executor knows.
Human-declared graphs (YAML) and planner-generated plans **compile to it**.
Conforms to `schemas/graph-ir.schema.json`.

```json
{
  "ir": "1",
  "graph_id": "basic-chat",
  "origin": { "kind": "declared" },
  "nodes": [
    { "ref": "eco", "resolve": "logical.echo" }
  ],
  "edges": [
    { "from": "client.text_out", "to": "eco.text_in" },
    { "from": "eco.text_out",    "to": "client.text_in" }
  ]
}
```

## Fields

- **`ir`** — format major. Executors reject majors they do not understand.
- **`graph_id`** — graph identifier (unique per registration).
- **`origin`** — provenance: `{ "kind": "declared" }` or `{ "kind": "planner", "skill": "<id>", "cause": "<msg-id>" }`. The plan's causality stays chained to the message that originated it.
- **`nodes[]`** — each node declares:
  - `ref` — local name within this graph.
  - `resolve` — a **capability demand** (`logical.echo`, `cognitive.llm.chat`) OR an exact package via `use: "org/cat/name"`. Resolution belongs to the kernel, not the author.
  - `constraints` — optional: `{ "place": "...", "privacy": "no-egress", ... }` for the scheduler.
- **`edges[]`** — port-to-port connections: `"<ref>.<port>"`. The pseudo-node **`client`** represents the external user/application connection: its ports are `client.text_out` (what it sends) and `client.text_in` (what it receives), extensible to other schemas.
- **`edges[].gate`** — optional: `"human-approval"` holds the message until the user confirms; `"none"` declares that this edge needs no approval even though its destination acts on the world (see rule 5).
- **`waves[]`** — optional: wave-based execution with barriers (`[["a","b"],["c"]]` = a and b in parallel, c waits).
- **`edges[].speculative`** — v1.2, optional. Deliver this edge on *partial*
  upstream output rather than waiting, and discard the work if the producer's
  final output differs. See rule 6.
- **`edges[].deadline_ms`** — v1.2, optional. How long a delivery here is worth
  waiting for. See rule 7.
- **`edges[].respond_ms`** — v1.3, optional. How soon the *first output* caused
  by a delivery here is due. A target, not a validity bound. See rule 9.
- **`edges[].priority`** — v1.2, optional integer, higher wins. Orders
  preemption when two chains contend for one Expertise.
- **`context_budget`** — v1.2, optional. A cap, in estimated tokens, on the
  context a session of this graph may accumulate. See rule 8.

## Normative rules

1. The executor validates the IR against the schema and against the registry (every `resolve` must be satisfiable or degradable) BEFORE instantiating a session.
2. Schema compatibility of ports connected by an edge is validated at instantiation: `from.schema` must be compatible with `to.schema` (same ref and major).
3. Once compiled, a planner-generated graph is indistinguishable from a declared one: same permissions, same replay, same debugger.
4. `gate`s are a property of the graph, not Expertise code: the kernel holds the message, emits the question through `client.text_in` (schema `std/confirmation@1`), and resumes with the answer.
5. **Motor-gate invariant.** An edge whose destination resolves to an Expertise of type `motor` (one that acts on the world) always carries a `human-approval` gate, whatever the IR says. The kernel applies this at session instantiation, so it holds identically for declared graphs, planner output, a visual editor, or any future producer. In `local` and `site` modes an omitted gate is added; in `published` mode the session is refused instead, because on a public network silently repairing the graph would hide an authoring mistake. An explicitly declared gate is preserved, never doubled.

   The rule is aimed at **omission** — a graph reaching an Expertise that acts on the world without its author ever considering the question. An author who has considered it may say so with `gate: "none"`, and the kernel adds nothing. That is not a loophole but the point: it turns an invisible default into a deliberate, greppable line in the graph that a reviewer can find. Some motor Expertises are unusable otherwise — speech synthesis is `motor`, and an assistant that asked permission before every spoken clause would not be one.

6. **The speculation invariant.** An edge may declare `speculative: true`,
   asking the executor to deliver it on partial upstream output and to discard
   the result if the producer's final output differs. **A conforming
   implementation MUST refuse a graph that declares `speculative` on an edge
   whose destination resolves to an Expertise of type `motor`.**

   This is the motor-gate invariant (rule 5) seen from another angle: the type
   of Expertise that must wait for a human is the type that must never run ahead of
   certainty, because a discarded effect is not discarded. C1's five types are
   an *effect type system*, so "is this safe to run early?" is answered by the
   manifest — statically, for every Expertise in the graph — rather than by the
   author's judgement or by a heuristic. Refused rather than silently ignored:
   an author who wrote `speculative` on an edge that sends an email has
   misunderstood something, and dropping the flag would leave them believing
   it worked.

   `speculative` combined with `gate: "human-approval"` on the same edge MUST
   also be refused: asking a human to decide and asking to run before the
   decision are incompatible requests.

   Reconciliation is normative. The executor folds partials into a running
   value using the schema's own semantics — appending for a delta schema
   (`std/text@1`), replacing for a replacement schema (`std/transcript@1`) —
   and delivers that value marked complete. When the producer's final output
   arrives, the implementation MUST compare it against what was speculated on,
   **disregarding any completion flag**, and then either suppress the redundant
   delivery (the values agree) or abandon the speculative chain through the
   ordinary C3 cancel path and deliver the real value (they do not). Comparing
   the completion flag would make every speculation a miss.

   A node MAY refuse speculation wholesale by policy. Doing so downgrades the
   edge rather than refusing the graph, because a node-wide preference is not
   an authoring mistake.

7. **Deadlines are absolute and inherited.** `deadline_ms` on an edge is
   converted to an absolute instant at the hop that declares it and travels on
   the envelope (C3 `deadline`). A downstream hop MAY tighten it and MUST NOT
   extend it: a chain given 200ms cannot grant itself more by adding hops. A
   receiving Expertise is expected to **degrade** — fewer beams, a smaller model, a
   coarser quantization — rather than abort, because a worse answer in time
   beats a better one after nobody is listening. An implementation MAY drop a
   `data` delivery whose deadline has already passed, and MUST NOT drop a
   terminal one (`done`, `error`, an approval request), since a consumer
   waiting forever is worse than a late answer.

   A deadline is a **validity bound**: it says when what a delivery causes
   stops being worth having. That makes it the right tool for an act on the
   world, and the wrong one for a reply that is merely wanted soon — see rule 9.

   **Inherited by the kernel, never carried by the Expertise** (v1.3). What an
   Expertise emits inherits the deadline of the delivery it names as its cause
   (C3 `cause_id`), looked up in the executor's own record of that delivery. An
   Expertise does not echo `deadline` and an executor MUST ignore one it sent:
   a budget carried by the party it constrains is a budget that party can
   extend. The same record is what lets the executor refuse to believe a cause
   that names a delivery made to someone else.

   Per delivery, not per chain. Every token of a reply shares one root, so a
   deadline remembered per root would condemn everything after the first
   instant recorded. An edge that declares `deadline_ms` directly after a
   streaming producer gives *each item* its own budget, counted from when that
   item was forwarded.

   The budget of an edge is spent from the moment the hop is attempted. For a
   gated edge that is when the delivery is held, not when a human answers: an
   approval that arrives after the deadline approves an act that is no longer
   valid, and the effect MUST NOT be carried out.

   **A late effect is recorded, not just skipped.** Whether a delivery is late
   MUST be decided before the Effect Checkpoint (C4): an effect dropped for its
   deadline is sealed with outcome `expired` and is never sealed as
   `delivered`. An executor SHOULD tell the client once per chain and
   destination that late output is being dropped, so a reply cut short is
   explicable.

8. **The context budget is the kernel's to enforce.** When a graph declares
   `context_budget`, the executor accounts every `data` delivery into an Expertise
   against it and refuses the delivery once it is exhausted. It lives here
   rather than in an Expertise's config for the same reason gates live in the
   executor: a budget enforced by the Expertise holds only for Expertises that
   remembered to implement it, and the failure mode differs everywhere — a
   truncation in one, a 413 in another, silent data loss in a third.

   Token counting is explicitly an **estimate**. A kernel has no tokenizer and
   should not grow one, since that would couple it to a model family. An
   implementation MUST document its estimator rather than present the figure as
   exact.

9. **A response target is a different promise from a deadline** (v1.3).
   `respond_ms` on an edge is converted to an absolute instant at the hop that
   declares it and travels on the envelope (C3 `respond_by`). It says when the
   **first output** caused by that delivery is due.

   Two fields rather than one with a mode, because a delivery may be under
   both and they fail differently:

   | | `deadline_ms` → `deadline` | `respond_ms` → `respond_by` |
   |---|---|---|
   | Meaning | After this, do not bother. | Start answering by then. |
   | Bounds | Everything the delivery causes | The front of what it causes |
   | On a miss | `data` is dropped; an effect is sealed `expired` | Recorded; nothing is dropped |
   | Right for | An act on the world | A reply someone is waiting to hear begin |

   A graph that puts `deadline_ms: 300` ahead of a streaming model to make it
   answer promptly gets a reply truncated at 300ms, which is what a deadline
   means. `respond_ms: 300` is what was meant.

   **Carried by the front.** The first `data` output an Expertise emits for a
   delivery inherits that delivery's `respond_by`, and carries it on down the
   chain; every later output for the same delivery inherits none. A reply that
   began in time is not late for being long. Tightening works as in rule 7: a
   downstream edge MAY bring the instant forward and MUST NOT push it back.

   **A miss is a measurement.** When the front is delivered after its
   `respond_by`, the executor records it — in the causal log, beside the
   delivery that was late — and delivers. It MUST NOT drop anything on the
   strength of a response target, and it reports a given lateness once rather
   than at every hop further down.
