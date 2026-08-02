# C2 — Graph IR (frozen contract)

**Protocol major: 1 · Status: v1.2 — FROZEN (2026-08-15: `edges[].speculative`, `edges[].deadline_ms`, `edges[].priority` and `context_budget` — all additive and all defaulting to prior behaviour, Phase 5; v1.1 2026-08-01 added the motor-gate invariant and `gate: "none"` as its explicit opt-out; base v1.0 frozen 2026-07-14). Changes: additive only; breaking = new major via RFC.**

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
- **`edges[].priority`** — v1.2, optional integer, higher wins. Orders
  preemption when two chains contend for one skill.
- **`context_budget`** — v1.2, optional. A cap, in estimated tokens, on the
  context a session of this graph may accumulate. See rule 8.

## Normative rules

1. The executor validates the IR against the schema and against the registry (every `resolve` must be satisfiable or degradable) BEFORE instantiating a session.
2. Schema compatibility of ports connected by an edge is validated at instantiation: `from.schema` must be compatible with `to.schema` (same ref and major).
3. Once compiled, a planner-generated graph is indistinguishable from a declared one: same permissions, same replay, same debugger.
4. `gate`s are a property of the graph, not skill code: the kernel holds the message, emits the question through `client.text_in` (schema `std/confirmation@1`), and resumes with the answer.
5. **Motor-gate invariant.** An edge whose destination resolves to a skill of type `motor` (one that acts on the world) always carries a `human-approval` gate, whatever the IR says. The kernel applies this at session instantiation, so it holds identically for declared graphs, planner output, a visual editor, or any future producer. In `local` and `site` modes an omitted gate is added; in `published` mode the session is refused instead, because on a public network silently repairing the graph would hide an authoring mistake. An explicitly declared gate is preserved, never doubled.

   The rule is aimed at **omission** — a graph reaching a skill that acts on the world without its author ever considering the question. An author who has considered it may say so with `gate: "none"`, and the kernel adds nothing. That is not a loophole but the point: it turns an invisible default into a deliberate, greppable line in the graph that a reviewer can find. Some motor skills are unusable otherwise — speech synthesis is `motor`, and an assistant that asked permission before every spoken clause would not be one.

6. **The speculation invariant.** An edge may declare `speculative: true`,
   asking the executor to deliver it on partial upstream output and to discard
   the result if the producer's final output differs. **A conforming
   implementation MUST refuse a graph that declares `speculative` on an edge
   whose destination resolves to a skill of type `motor`.**

   This is the motor-gate invariant (rule 5) seen from another angle: the type
   of skill that must wait for a human is the type that must never run ahead of
   certainty, because a discarded effect is not discarded. C1's five types are
   an *effect type system*, so "is this safe to run early?" is answered by the
   manifest — statically, for every skill in the graph — rather than by the
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
   receiving skill is expected to **degrade** — fewer beams, a smaller model, a
   coarser quantization — rather than abort, because a worse answer in time
   beats a better one after nobody is listening. An implementation MAY drop a
   `data` delivery whose deadline has already passed, and MUST NOT drop a
   terminal one (`done`, `error`, an approval request), since a consumer
   waiting forever is worse than a late answer.

8. **The context budget is the kernel's to enforce.** When a graph declares
   `context_budget`, the executor accounts every `data` delivery into a skill
   against it and refuses the delivery once it is exhausted. It lives here
   rather than in a skill's config for the same reason gates live in the
   executor: a budget enforced by the skill holds only for skills that
   remembered to implement it, and the failure mode differs everywhere — a
   truncation in one, a 413 in another, silent data loss in a third.

   Token counting is explicitly an **estimate**. A kernel has no tokenizer and
   should not grow one, since that would couple it to a model family. An
   implementation MUST document its estimator rather than present the figure as
   exact.
