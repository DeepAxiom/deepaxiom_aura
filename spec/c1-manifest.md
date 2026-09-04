# C1 — Skill Manifest (frozen contract)

**Protocol major: 1 · Status: v1.7 — FROZEN (2026-09-04: `std/media-asset@1` added — additive, the media subsystem; v1.6 2026-09-02: `std/confirmation@1` gained optional `context_required`, so a gate can say what an approval must bind before a person answers rather than after — additive, C4 v1.7; v1.5 2026-08-02: `std/db-change@1` added — additive, Phase 3 Postgres CDC; 2026-08-02: `std/confirmation@1` gained optional `to_ref`/`to_port` — additive, Phase 2 session resume; 2026-08-01: `compensates` added — additive; 2026-08-01: the `std` namespace became executable JSON Schemas, `std/transcript@1` added, `std/audio-chunk@1` gained optional `channels`/`seq`/`ref` — all additive; 2026-07-30: `config` added; base v1.0 frozen 2026-07-14). Changes: additive only; breaking = new major via RFC.**

A **Skill** is the system's atomic unit of function: something the system *knows how to do*.
Every skill is described by a `skill.yaml` file conforming to `schemas/manifest.schema.json`.

## Skill types

| `type` | What it does | Examples |
|---|---|---|
| `sensorial` | Perceives: turns the world into data | ASR, OCR, camera, file reader |
| `cognitive` | Reasons: decides, plans, generates | LLM chat, planner, classifier |
| `motor` | Acts: produces effects in the world | send WhatsApp, write to an ERP, TTS |
| `memory` | Remembers: persists and retrieves context | history, semantic memory |
| `logical` | Transforms/validates: data → data, deterministic | JSON parser, validator, format translator |

## Fields

```yaml
id: "org/category/name"           # global identity, lowercase, no spaces
version: "1.0.0"                  # package semver
protocol: "1"                     # channel-protocol major it speaks

name: "Human-readable name"
description: "What it does. Planners READ this text to decide whether to use it."
capability: "logical.echo"        # resolvable taxonomy: <type>.<function>[.<subtype>]
type: logical                     # sensorial | cognitive | motor | memory | logical

format: source                    # source | wasm | model | projection
runtime:                          # only if format=source
  language: python
  version: ">=3.11"

ports:
  ingress:
    - name: text_in
      schema: "std/text@1"        # MANDATORY: semver schema reference
  egress:
    - name: text_out
      schema: "std/text@1"
    - name: status_out
      schema: "std/status@1"

requirements:                     # for resource admission and placement (optional)
  memory: "128Mi"
  accelerator: none               # none | gpu | npu | any

permissions:                      # capability-based: anything NOT listed is denied
  egress_http: []                 # allowed outbound HTTP domains
  filesystem: none                # none | read:<path> | write:<path>
  channels: declared-only         # may only speak through its declared ports

config:                           # optional (v1.1, additive): runtime-tunable parameters
  - key: "temperature"            # declared HERE = what CAN be tuned, not the current value
    type: float                   # string | int | float | bool | enum
    default: 0.7
    min: 0.0                      # optional, int/float only
    max: 2.0                      # optional, int/float only
    description: "Sampling temperature."
    restart_required: false       # true if applying it needs the skill process restarted

signature: null                   # required by the registry in published mode
```

## Normative rules

1. `id` + `version` identify an immutable package. Republishing the same version with different content is a registry error.
2. Every port MUST declare a `schema`. A schema `ns/name@MAJOR` resolves in the registry. Changes within a major MUST be additive; a breaking change requires a new major.
3. `permissions` is deny-by-default: the kernel/sandbox rejects any undeclared action.
4. `capability` is what the resolver looks up (`resolve: logical.echo`); `id` is just the concrete package that satisfies it.
5. In `local` mode the signature is not required. In `published` mode the registry rejects packages without a valid signature.

## Runtime config (optional, additive)

`config` declares what a skill's parameters *can* be — type, default, bounds —
so the UI and any config file can discover and validate them without reading
the skill's source. It does NOT declare their current value: a manifest is an
immutable package (rule 1), so a live value can't live inside it.

Effective values are resolved outside the manifest, by whichever kernel the
skill connects to, in ascending priority:

1. `default` from the declared `config` entry above (ships with the skill).
2. A value for that key in the kernel's `--config` file, if one was given at
   `aura up` — reproducible, git-friendly, re-read at every kernel start.
3. A value set live via `PUT /v1/skills/config?id=<id>` (the control-plane UI
   or any HTTP client) — persisted by the kernel, highest priority, survives
   restarts until changed again.

A key not declared in `config` is rejected by both the file loader's
validation path and the HTTP endpoint — capability-based, like `permissions`:
nothing undeclared is accepted.

**Transport.** The kernel computes the effective config and includes it in
the `payload.config` field of the `status` envelope that acks a skill's
`register` (C3) — a skill reads its starting values off that ack. If a value
changes later while the skill is connected, the kernel pushes a `kind:
"config_update"` envelope (C3, additive) on that same connection, payload =
the full new effective config; a skill applies it live unless the changed
key(s) are all `restart_required: true`, in which case picking it up needs a
reconnect. A skill that never reads `config` at all keeps working exactly as
before — this is opt-in, not a breaking change to existing skills.

## Compensation (optional, additive; read by [C4](c4-ledger.md))

```yaml
compensates:
  port: undo_in                     # MUST be a declared ingress port
  schema: "myorg/erp-undo@1"        # SHOULD match that port's declared schema
```

A `motor` skill MAY declare how one of its effects is undone. `port` names an
ingress port the skill already declares under `ports.ingress`, carrying
whatever shape that skill needs to reverse itself — there is no standard
`compensation` schema, because undoing an invoice and undoing a spoken sentence
have nothing in common. A future `aura undo` (see
`aura undo`) replays a payload onto that port. Declaring this
changes nothing about how the skill runs today — the field is metadata, read by
the kernel's effect ledger ([C4](c4-ledger.md)) at the moment an effect is
sealed, so an entry in the ledger can say whether that specific effect is
reversible at all. A
skill with no `compensates` is simply recorded as irreversible, which is itself
useful information for an operator reading the ledger back.

Only `motor` skills may declare it — the field is meaningless on a skill that
does not act on the world, and the kernel rejects a manifest that tries.

## Minimal standard schemas (namespace `std`, v1)

Each is an executable JSON Schema in [`schemas/std/`](schemas/std/), exercised
by the conformance suite. The shapes below are a summary; those files are
normative.

- `std/text@1` — `{ "text": string, "final": bool? }`
- `std/transcript@1` — `{ "text": string, "final": bool, "replace": bool?, "utterance": string?, "confidence": number? }`
- `std/status@1` — `{ "state": "working"|"done"|"error", "detail": string? }`
- `std/document@1` — `{ "mime": string, "bytes_b64": string, "uri": string? }`
- `std/audio-chunk@1` — `{ "pcm_b64": string, "sample_rate": int, "channels": int?, "final": bool?, "seq": int?, "ref": string? }`
- `std/api-request@1` — `{ "params": obj?, "query": obj?, "headers": obj?, "body": any? }` (projections)
- `std/api-response@1` — `{ "ok": bool, "status": int, "body": any?, "dry_run": bool?, "request": obj?, "error": string? }` (projections)
- `std/confirmation@1` — `{ "question": string, "options": [string], "held": string, "to_ref": string?, "to_port": string?, "context_required": [string]? }` (gates)
- `std/plan@1` — `{ "reasoning": string, "graph": <C2 IR>, "inputs": [{ "port": string, "schema": string, "payload": any }] }` (planners)
- `std/db-change@1` — `{ "table": string, "op": "insert"|"update"|"delete", "columns": obj, "lsn": string? }` (CDC, e.g. `example/sensorial/postgres-cdc`)
- `std/media-asset@1` — `{ "uri": string, "mime": string?, "asset_id": string?, "state": "received"|"processing"|"ready"|"failed"?, "duration_ms": int?, "renditions": [obj]?, "frames": [string]?, "poster": string?, "error": string? }` (media, e.g. `deepaxiom/logical/media-transcode`)

**`text` and `transcript` are not interchangeable, and this is the trap.** In
`std/text@1`, `text` is a *delta*: a streaming producer emits one per token and
the consumer concatenates. In `std/transcript@1` it *replaces* the previous
hypothesis, because a recogniser re-decodes its whole buffer — hypothesis 3 is
not hypothesis 2 plus a suffix. Carrying speech partials as `std/text@1` would
make every consumer that concatenates produce garbage, so they are separate
schemas on separate ports.

In `std/audio-chunk@1`, samples are signed 16-bit little-endian PCM,
base64-encoded, and `final` marks the end of an *utterance or clause*, not of
the stream — that is `kind: "done"`. `seq` lets a receiver notice a gap when a
`realtime` channel drops a chunk. `ref` is reserved: it exists so samples can
one day travel out of band without changing the envelope, and a receiver may
ignore it.
