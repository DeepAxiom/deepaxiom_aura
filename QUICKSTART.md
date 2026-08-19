# Quick start

From nothing to a running node with a skill connected, in three terminals.
Every command here has been run end to end on a clean machine.

[Versión en español](QUICKSTART-ES.md) · [Full guide](GUIDE.md) · [README](README.md)

**You need:** Go 1.25+ and Python 3.11+. No CGO, no Docker, no database, no
account.

---

## 1 · Build and start the node

```bash
cd kernel
go build -o aura ./cmd/aura
./aura up
```

It prints a banner. Two lines from it matter:

```
  ui        http://localhost:9080
  open      http://localhost:9080/#token=FM-cEnU-zwwENsS0iBb3Xy2SeQ...
```

**Open the `open` URL in a browser.** That is the control plane — the canvas,
the reference, chat, sessions. The token rides in the fragment because browsers
never send a fragment to a server, so it stays out of access logs; the page
stores it and strips it from the address bar.

The node binds `127.0.0.1` and writes its token to `~/.aura/node.token`. Leave
this terminal running.

---

## 2 · Connect a skill

**A fresh node has an empty catalogue.** `aura up` is the kernel and no skills:
skills are separate processes that connect *to* it. Nothing will run until one
does.

```bash
cd skills/echo
PYTHONPATH=../../sdk/python/src python main.py
```

You should see `registered:` in the output. No token setup — the SDK reads
`~/.aura/node.token` the same way the CLI does.

> Run it from the skill's own directory. The SDK loads `skill.yaml` from the
> working directory, so starting it from the repo root cannot find the manifest.

Leave this terminal running too.

---

## 3 · Talk to it

```bash
./kernel/aura status
```

```
skills connected: 1
  · example/logical/echo                     logical    logical.echo
```

```bash
./kernel/aura chat --graph echo "hello aura"
```

```
hello aura
```

That round trip went client → kernel → graph executor → skill → back, over a
WebSocket, with every envelope durably logged before it was acknowledged.

---

## What to do next

| | |
|---|---|
| **Draw a graph** | Open the **Canvas** view. Drag a skill from the palette, wire `client.text_out` to its input, and register it. |
| **Read the reference** | The **Reference** view has every command, all five contracts, every schema — offline, no network. |
| **Put your own app behind it** | [`examples/expose-app/`](examples/expose-app/) is six lines: expose two existing functions, mark one `write: true`, and the kernel gates and seals it. |
| **Guard an agent you already run** | `aura guard --config claude_desktop_config.json` puts your MCP servers behind a checkpoint. No runtime to stand up. |
| **See what was sealed** | `aura verify` recomputes the ledger's hash chain and signatures from the database file alone, with no kernel running. |

---

## Things that will bite you

**Flags go before the message.**

```bash
./kernel/aura chat --graph echo "hello"    # yes
./kernel/aura chat "hello" --graph echo    # the flag is ignored
```

**`aura chat` without `--graph` uses the `chat` graph, which needs an LLM.**
Without one it fails with `no connected skill provides capability
"cognitive.llm.chat"`. Start [`skills/llm-chat/`](skills/llm-chat/) for that —
it downloads a model. The `echo` graph is the one that works cold.

**Windows PowerShell** uses different syntax for environment variables:

```powershell
$env:PYTHONPATH = "../../sdk/python/src"; python main.py
```

**Nothing happens / `skills connected: 0`.** Check the skill's terminal. If it
says `401`, it could not find a credential — see below. If it says
`ConnectionRefused`, the node is not up.

---

## Credentials, in one paragraph

A node mints a bearer token and every route is behind it, `/ws/skill`
included. The CLI and both SDKs look in the same two places, in order:
`AURA_TOKEN`, then `~/.aura/node.token`. Same user, same machine, default data
directory — nothing to configure.

You only need to think about it when one of those is not true:

```bash
# the node keeps its data elsewhere
./aura up --data /srv/aura
export AURA_TOKEN=$(cat /srv/aura/node.token)

# or, for a single-user machine you do not want to think about at all
./aura up --no-auth
```

`--no-auth` is loopback-only and prints a warning. It is fine for local
development and wrong for anything else — see
[Security model](GUIDE.md#security-model).

---

## Running it as a service

```bash
docker compose up
```

One kernel, a persistent named volume, and the strict policy in
[`deploy/aura.policy.yaml`](deploy/aura.policy.yaml) — deny-by-default for
anything acting on the world, no graph-level waivers, and an effect the ledger
cannot seal is refused rather than delivered. `aura up` prints the policy's hash
at startup so the one in force can be compared against the one in git.

The example app that exposes its own functions is behind a profile, because it
needs a credential minted from a running node:

```bash
docker compose up -d aura
docker compose logs aura | grep -o '#token=[^ ]*'
AURA_SKILL_TOKEN=<that token> docker compose --profile demo up app
```

The container is distroless, non-root and CGO-free. Two endpoints an
orchestrator wants: `GET /readyz` (open, answers only once the store and ledger
are usable) and `GET /metrics` (Prometheus, authenticated). The image's
healthcheck is `aura ready`, which turns the first into an exit code — there is
no shell or curl inside to probe with.

**The data directory is not a cache.** It holds the node identity, the effect
ledger and the broker's encrypted secrets — and the broker's key is *derived*
from the identity, so a restore without `identity/` yields ciphertext nobody can
open. Back it up like a database.

**This is pre-1.0.** One process, no failover: if the node dies, live routing
state dies with it while the event log survives. Answer that before you deploy —
if your app degrades, this is deployable today; if it stops, read
[the roadmap](ROADMAP.md) first.
