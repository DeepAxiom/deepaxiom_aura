# docs/media

Imagery for the top-level README. Everything here is either the real control
plane captured from a running node, or a diagram of a mechanism that exists in
this repo. Nothing is mocked up.

## What the captures show, and what they do not

The UI is real and so is every number on it: the captures were taken from a
node built from this tree (`aura 0.4.0`), with the UI embedded in the binary,
driven in Chromium by Playwright.

The traffic comes from three **test Expertise** in `src/demo_expertise.py`,
named as such on screen (`Draft (test stream)`, `Redact digits (test)`,
`Notify (test effect)`). `draft` is not a model: it replays canned sentences
one token at a time and reports token counts in `attest.usage` the way an
engine adapter does. So token totals, the prefix-cache ratio and replica load
are real counters of that synthetic stream — they say nothing about any
model's speed or quality. `notify` appends to a file in the temp directory;
the kernel gates and seals it because its capability is `motor.*`.

## UI stills (PNG, 1600×1000, dark and light)

| File | Shows |
|---|---|
| `pulse-dark.png`, `pulse-light.png` | Pulse while 14 sessions ramp over 3 replicas of `cognitive.demo.draft`: live sessions, token totals and rates, prefix-cache ratio, one load bar per replica, mailbox, speculation, store, event log, watchers, node identity. All from `GET /v1/stats`. |
| `studio-live-dark.png`, `studio-live-light.png` | Studio, graph `notice`, mid-stream: the Run panel on the right, the canvas following the same session through its event log — active nodes, envelopes counted per edge, the text `draft` is emitting shown under it. |
| `studio-gate-dark.png`, `studio-gate-light.png` | The same run a moment later: the kernel is holding the delivery into `motor.demo.notify`; the gated edge reads `held`, the node waits, the Run panel asks. |
| `session-waterfall-dark.png`, `session-waterfall-light.png` | Sessions: that session as a causal waterfall (parent = `cause_id`, streams collapsed to one row), the sealed delivery selected, its ledger entry linked. |
| `evidence-ledger-dark.png`, `evidence-ledger-light.png` | Evidence: the verify result from `GET /v1/ledger/verify`, the chain head, the ledger with one entry open (prev → hash, policy, payload hash, inference attestation). |
| `expertise-catalog-dark.png`, `expertise-catalog-light.png` | The Expertise catalog grouped by type, one entry open: ports and schemas, declared permissions, per-replica load, runtime config editor. |
| `approvals-inbox-dark.png` | Evidence → Approvals inbox with one pending gate, raised by calling `motor_demo_notify` through `/mcp` (a non-human client). |

## Animated captures (GIF, 1000×625, 12 fps, looping)

| File | Shows |
|---|---|
| `studio-live.gif` | Typing into the Run panel; tokens stream; edges carry envelopes; the kernel holds the motor delivery; Approve; delivered and sealed. |
| `pulse-load.gif` | Pulse while 15 sessions ramp up over three replicas and drain: replica bars, token rates, live-session count. |
| `session-waterfall.gif` | Filtering the session list, opening a session, selecting the stream row, the gate row and the sealed delivery, then following its receipt to the ledger entry. |
| `palette-build.gif` | Ctrl+K → "new graph"; two Expertise added by double-click quick-add; three edges wired by dragging ports; tidy and fit. |

The loops cut from the last frame back to the first; they are not cross-faded.

## The demo run (`aura demo`)

| File | Shows |
|---|---|
| `under-fire.gif`, `under-fire-es.gif` | One whole run of `aura demo`, from the first session opening to the result, in English and in Spanish. Two separate runs, not one run relabelled. 1000×625, 12 fps. |
| `under-fire.png`, `under-fire-es.png` | The screen as each of those two runs ended (1280×800). |
| `versus.gif`, `versus-es.gif` | One whole run of `aura demo --versus`, in each language: an ordinary agent on the left, a node on the right, four things done to both. Two separate runs. 1000×625, 12 fps. |
| `versus.png`, `versus-es.png` | The screen as each of those two runs ended (1280×800). |
| `social-preview.png` | `versus.png` scaled into 1280×640 for the repository's social preview. Nothing redrawn: the same screenshot, letterboxed. |

These are not the control plane and not the test Expertise above: the screen
is the demo's own page, embedded in the binary, and the figures on it are that
run's. The agent on the left of the comparison is
`kernel/internal/demo/plain.go`, a real process; what is fair about setting a
node beside it, and what is not, is in
[the guide](../../GUIDE.md#the-same-agent-twice-aura-demo---versus). A node and a standby from this tree, 300 relay sessions and 24 operation
sessions, the simulated engine — the footer of the screen says so. What the
demo starts, what each of its six beats requires and what it leaves out is in
[the guide](../../GUIDE.md#a-node-under-fire-aura-demo).

**Three times faster than it happened.** A frame was taken every 250 ms and the
GIF plays twelve a second, so 51 seconds of run are 18 seconds of GIF. The
comparison's runs took 36 and 37 seconds, 13 of GIF. The clock
in the top-right corner of the screen shows the real elapsed time throughout.
Nothing else was done to it: no frame was dropped, reordered or retouched.

**Frames rather than video, and why.** The first capture recorded a video in
real time, like the scenes above. The browser encoding it took a core from the
node being measured, on a four-core laptop, and the run it filmed came out
several times slower than one nobody was filming — the standby took nine
seconds instead of half of one. That recording was thrown away. A screenshot
every 250 ms costs almost nothing, and the two runs kept here took 51 and 52
seconds, the same as a run with no browser at all.

## Mechanism diagrams (animated SVG, CSS animation only)

No script, no external reference, own background, `prefers-color-scheme`
inside the file — they animate when embedded with `<img>` on GitHub in either
theme. Each has a `<title>` and `<desc>`. 12 s cycle.

| File | Mechanism | Read from |
|---|---|---|
| `mechanism-connection.svg` | A batch run (fires, runs once, returns whole, tears down, polls again) beside a connection that stays open and carries typed envelopes token by token. | `README.md` §1, `spec/c3-channel.md` (Envelope) |
| `mechanism-gate-ledger.svg` | An envelope bound for a `motor.*` Expertise held at the Effect Checkpoint; `confirm_request` to a person; on approve, an entry appended whose `prev` is the previous entry's hash; the delivered envelope carries `receipt`. A denial is also one entry. | `spec/c4-ledger.md`, `kernel/internal/executor/session.go`, `kernel/internal/ledger/ledger.go` |
| `mechanism-mailbox.svg` | Inline dispatch, where one slow session blocks the connection's read loop for the others, beside per-session mailboxes (depth 1024), where it fills only its own queue; a full mailbox makes the read loop wait and drops nothing. | header comment of `kernel/internal/executor/mailbox.go` |
| `mechanism-coalescing.svg` | 14 token deltas becoming 5 text envelopes plus the final marker: first delta sent at once, later ones flushed at `max_delay` (25 ms) or `max_chars` (256), same concatenated text. | `sdk/python/src/aura/stream.py` |
| `mechanism-trace.svg` | A caller's W3C `traceparent` on the first envelope, the same trace id on every envelope of the chain, spans joining the caller's trace; with no `trace` supplied, none appears on the wire. | `spec/c3-channel.md` "Trace context", `kernel/internal/executor/trace.go`, `kernel/internal/otel/` |

Illustrative, not measured: the sentence, capability names and hash values in
the diagrams, and the delta timings in the coalescing one (hand-traced through
`DeltaCoalescer.add`). Simplified: ledger entry fields are abbreviated; six
mailbox cells stand for 1024.

## Regenerating

```bash
# 1. a node from this tree, with the UI built into it
(cd ui && npm ci && npm run build)
(cd kernel && go build -o ../aura ./cmd/aura)
./aura up --port 9480 --no-auth --data /tmp/aura-media

# 2. traffic: three replicas of the test stream, the gated effect, two graphs
pip install websockets pyyaml
PYTHONPATH=sdk/python/src AURA_WS_URL=ws://localhost:9480/ws/skill \
    python docs/media/src/demo_expertise.py --replicas 3 &
AURA_PORT=9480 python docs/media/src/drive.py register

# 3. capture (Playwright lives outside the repo on purpose)
mkdir /tmp/cap && cd /tmp/cap && npm init -y && npm i playwright && npx playwright install chromium
node <repo>/docs/media/src/capture.mjs http://localhost:9480 /tmp/cap/out all
cp /tmp/cap/out/*.png <repo>/docs/media/

# 4. GIFs: two-pass palette, ffmpeg from a container
bash <repo>/docs/media/src/make_gifs.sh /tmp/cap/out <repo>/docs/media
```

The demo's capture is separate, and needs no node of yours — the demo starts its own:

```bash
(cd kernel && go build -o ../aura ./cmd/aura)
./aura demo --listen 127.0.0.1:7700 &        # waits for the page before it starts
cd /tmp/cap                                   # the Playwright directory from step 3
node <repo>/docs/media/src/capture_demo.mjs http://127.0.0.1:7700 /tmp/cap/demo-en
bash <repo>/docs/media/src/make_demo_gif.sh /tmp/cap/demo-en /tmp/cap/gif-en
# DEMO_LANG=es for the Spanish one: stop the demo, start it again, capture again.
# The comparison: start `./aura demo --versus ...` instead, and capture with
# DEMO_SCENE=versus; make_demo_gif.sh takes `versus` as its third argument.
```

The capture exits non-zero if the run it filmed did not survive every beat, so a
failed run cannot be published as a passing one by accident.

| Source | Role |
|---|---|
| `src/demo_expertise.py` | The three test Expertise. `--replicas`, `--tps`, `--coalesce`. |
| `src/drive.py` | Registers graphs `draft-only` and `notice`; opens sessions (`run`); raises a gate through `/mcp` (`mcp`). |
| `src/capture.mjs` | Playwright: the stills in both themes, and one video per animated scene with `scenes.json` (offset and length). |
| `src/make_gifs.sh` | `palettegen` then `paletteuse` per scene; `FPS`, `COLORS`, `WIDTH` env vars. |
| `src/capture_demo.mjs` | Playwright: opens the demo's screen held, starts the run, takes a frame every `EVERY_MS` until it ends, and a still in each language. `DEMO_LANG`, `PW_CHANNEL`. |
| `src/make_demo_gif.sh` | The same two passes over those frames; `FPS`, `COLORS`, `WIDTH`. |

The SVGs are hand-written; edit them directly.
