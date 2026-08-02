# C1/C2/C3 Conformance Suite

Validates that a kernel (or an SDK's artifacts) complies with the frozen contracts.
**Any implementation that passes this suite is officially conformant** — this is what
allows alternative kernels and SDKs to exist without fragmenting the ecosystem.

## Usage

```powershell
# 1. Start a kernel (clean data or not; the suite uses unique ids)
..\..\kernel\aura.exe up

# 2. In another terminal
pip install websockets jsonschema
python runner.py --port 9080
```

Exits 0 when conformant; 1 with the list of failures.

## Coverage

59 checks in total, in these sections:

| Section | Checks |
|---|---|
| Vectors vs the C1/C2/C3 JSON Schemas | valid/invalid manifests, IRs, and envelopes (15) |
| Vectors vs the `std` payload schemas | valid/invalid text, transcript, audio-chunk (10) |
| Health | protocol and IR majors declared |
| C1 | rejection of invalid manifests (missing description, unknown protocol, port without schema); registration and catalog |
| C2 | rejection of invalid IR (duplicate refs, ghost refs, unknown major); graph registration |
| C3 | round-trip with causality (`cause_id`), complete envelope to the skill, FIFO, at-least-once dedup by `idem`, event log with causal closure |
| Resolution | missing capability → explained error; incompatible schemas on an edge → session rejected |
| Gates | `confirm_request` → approve delivers / deny produces an error |
| C3 — cancel | cancel reaches a skill past the first hop, carries the `cause_id` that skill recognises, and nothing from the cancelled chain reaches the client |

## Vector convention

- `valid-*.json` — the schema MUST accept them.
- `invalid-*.json` — the schema MUST reject them.
- `semantic-invalid-*.json` — well-formed per the schema, but every
  implementation MUST reject them (cross-reference rules JSON Schema cannot
  express: duplicate refs, ghost endpoints).

The runner is **black-box and speaks the raw protocol** (WS + HTTP, no SDK):
if your kernel passes, any conformant SDK will be able to talk to it.
