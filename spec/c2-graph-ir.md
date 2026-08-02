# C2 — Graph IR (frozen contract)

**Protocol major: 1 · Status: v1.1 — FROZEN (2026-08-01: the motor-gate invariant, and `gate: "none"` as its explicit opt-out — additive enum value; base v1.0 frozen 2026-07-14). Changes: additive only; breaking = new major via RFC.**

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

## Normative rules

1. The executor validates the IR against the schema and against the registry (every `resolve` must be satisfiable or degradable) BEFORE instantiating a session.
2. Schema compatibility of ports connected by an edge is validated at instantiation: `from.schema` must be compatible with `to.schema` (same ref and major).
3. Once compiled, a planner-generated graph is indistinguishable from a declared one: same permissions, same replay, same debugger.
4. `gate`s are a property of the graph, not skill code: the kernel holds the message, emits the question through `client.text_in` (schema `std/confirmation@1`), and resumes with the answer.
5. **Motor-gate invariant.** An edge whose destination resolves to a skill of type `motor` (one that acts on the world) always carries a `human-approval` gate, whatever the IR says. The kernel applies this at session instantiation, so it holds identically for declared graphs, planner output, a visual editor, or any future producer. In `local` and `site` modes an omitted gate is added; in `published` mode the session is refused instead, because on a public network silently repairing the graph would hide an authoring mistake. An explicitly declared gate is preserved, never doubled.

   The rule is aimed at **omission** — a graph reaching a skill that acts on the world without its author ever considering the question. An author who has considered it may say so with `gate: "none"`, and the kernel adds nothing. That is not a loophole but the point: it turns an invisible default into a deliberate, greppable line in the graph that a reviewer can find. Some motor skills are unusable otherwise — speech synthesis is `motor`, and an assistant that asked permission before every spoken clause would not be one.
