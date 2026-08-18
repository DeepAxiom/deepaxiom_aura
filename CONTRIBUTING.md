# Contributing to AURA

AURA is the kernel, SDK, and three frozen contracts (`spec/`) that
everything else in this repo — the skills, the control-plane UI — is built
on top of. This doc covers contributing to *that core*: the Go kernel, the
Python SDK, and the spec itself.

**Want to add a device integration or a new capability instead?** You need
none of this. Writing a skill requires no changes to the core at all: copy
[`skills/echo/`](skills/echo/) — the minimal worked example, no model and no
credentials — or a skill wrapping a real external system like
[`skills/postgres-cdc/`](skills/postgres-cdc/), and read
[Writing a skill](GUIDE.md#writing-a-skill-the-sdk). Most people who want to
build something on AURA want that, not this guide.

**This is pre-1.0 (v0.1.0).** The project's central bet — a runtime that is
real-time and persistent rather than run-shaped — is only partly true of the
code today. See [Milestone status](GUIDE.md#milestone-status) for what landed and what was
deferred. Connection liveness, transitive cancellation and QoS enforcement are
in; session resume is the biggest gap still open, and seven kernel packages
have no automated tests at all.

**Found a security issue?** Don't open a public issue for it — see
[`SECURITY.md`](SECURITY.md) for how to report it privately.

## Where things live

| Area | Language | License | Touch it for |
|---|---|---|---|
| `kernel/` | Go | AGPLv3 or commercial | Executor, routing, gateway, projections, registry, federation |
| `sdk/python/` | Python | Apache-2.0 | The `aura` package every skill imports |
| `sdk/node/` | TypeScript | Apache-2.0 | `@deepaxiom/aura` — types are generated from `spec/schemas`, never hand-edited |
| `spec/` | Markdown + JSON Schema | Apache-2.0 | C1 (manifest), C2 (graph IR), C3 (channel protocol) |
| `ui/` | TypeScript/React | AGPLv3 or commercial | Control-plane frontend |
| `skills/` | Python | AGPLv3 or commercial | First-party skills (echo, llm-chat, asr, tts, sentence-chunker, planner, model-manager, memory-context, postgres-cdc) |

See [`LICENSE.md`](LICENSE.md) for the exact, authoritative map before
opening a PR — what license your change falls under depends on which
directory it's in.

## The spec is frozen — read this before touching `spec/`

`spec/c1-manifest.md`, `c2-graph-ir.md`, and `c3-channel.md` are each
marked **FROZEN** at protocol major 1 — C1 at v1.2, C2 at v1.1, C3 at v1.3.
The version line at the top of each file is authoritative; if this paragraph
and that line disagree, that line is right. Frozen is not a suggestion:

- Changes within the current major must be **additive only** — new
  optional fields, new enum values, never removing or repurposing an
  existing one.
- A breaking change requires a **new protocol major**, proposed as an RFC
  (open an issue describing the change, the reason the existing contract
  can't express it, and the migration story) before any code lands.
- If you're adding a capability that fits within the existing contracts —
  which is almost always true, see the skill-authoring guide above — you
  don't need to touch `spec/` at all.

## Building and testing

```powershell
# kernel
cd kernel
go build -o aura.exe ./cmd/aura
go vet ./...
gofmt -l .          # should print nothing; gofmt -w . to fix
go test ./... -cover

# spec conformance suite (black-box, protocol-level; needs a running node)
python spec/conformance/runner.py

# documentation links (the docs once described a deleted directory)
python scripts/check_links.py

# SDK / skills
$env:PYTHONPATH = "sdk\python\src"
cd skills\llm-chat; pip install -r requirements.txt; python main.py

# control-plane UI
cd ui
npm install
npm run build        # emits into kernel/internal/gateway/ui/dist — rebuild
                      # the kernel after, so the embedded UI matches
```

A kernel change that touches routing, gating, or the channel protocol
should pass the conformance suite before a PR — that suite exists
specifically so behavior changes are visible as diffs against real
vectors, not just "it worked when I tried it."

## Before opening a PR

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs all of the
above and blocks the merge if any of it fails, so it is cheaper to run them
locally first.

- `go vet ./...`, `gofmt -l .` and `go test ./...` clean for any Go change.
- If you changed `ui/`, rebuild it and commit the regenerated
  `kernel/internal/gateway/ui/dist/` output alongside your source change —
  the kernel embeds that directory at build time, so a source-only PR
  leaves the running binary out of sync with what you wrote.
- Prefer a mock or a reproducible local test over "trust me, it works" —
  the first-party skills hold themselves to that standard (see
  [`skills/postgres-cdc/`](skills/postgres-cdc/), whose replication parser is
  tested as a pure function and whose end-to-end path is tested against a
  real Docker `postgres:16`, not mocked); PRs to the core should too.
- Keep commits scoped — one behavioral change per PR is easier to review
  and easier to revert if something's wrong.

## Licensing

By contributing, you agree your change is licensed under the terms of the
file it lands in (Apache-2.0 for `spec/`/`sdk/`, AGPLv3 for
`kernel/`/`ui/`/`skills/` unless you've arranged commercial terms) — see
[`LICENSE.md`](LICENSE.md) for the full, current map.
