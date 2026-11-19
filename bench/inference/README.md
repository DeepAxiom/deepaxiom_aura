# Inference bench

**No number in this repository came from this bench yet: it has only been run
against the bundled mock.**

It has not been pointed at `llama-server`, at vLLM, or at anything with a GPU.
The engine commands below were written from each engine's documentation and
were not executed here. Expect to correct a flag the first time.

This is the measuring half of phase 0b in
`docs/plans/inferencia-memoria-contexto.md`: a load with the shape a robot
puts on an inference engine, and a calibration for the token budget.

| File | What it is |
|---|---|
| [`robot_bench.py`](robot_bench.py) | Load generator for any OpenAI-compatible `/v1/chat/completions` streaming endpoint. |
| [`calibrate_tokens.py`](calibrate_tokens.py) | Estimates what a cached input token and an output token cost relative to a fresh input token. |
| [`mock_engine.py`](mock_engine.py) | A fake engine, `sleep()` behind a queue, used only to validate the two above. |
| [`test_robot_bench.py`](test_robot_bench.py) | Tests for all three, run against the mock in-process. |

Standard library only, so it runs on a Jetson with nothing installed. Run on
Python 3.13 (Windows) and 3.11 (Linux). Nothing newer than 3.8 is used on
purpose, but 3.8 to 3.10 were not run.

## The workload

A robot has a short request that must come back on time, sent at a fixed rate
whatever else is going on, and long generations nobody is waiting on. The
question is what the second kind does to the first.

- **critical**: one short request every `--critical-period-ms` (default 200),
  `--critical-max-tokens` (16), each with a deadline `--critical-deadline-ms`
  (500) after it is sent. Open loop: the next one leaves on schedule whether or
  not the last one came back. The 500 ms default is arbitrary. Set it to the
  budget of the loop you care about.
- **background**: `--background-concurrency` (4) generations of
  `--background-max-tokens` (512), restarted as each one ends.

Two phases, back to back in one invocation, `--duration-s` each (30):
**baseline** is the critical stream alone, **loaded** is the critical stream
with the background running. In the loaded phase the background gets
`--warmup-s` (2) to occupy the engine before the first critical request.

## What is measured, and what is not

| | |
|---|---|
| Time to first token of each class, per phase | p50, p90, p99 and max, from just before the TCP connect to the first chunk that carries text. Client clock. |
| End-to-end latency | p50, p90, p99, to `[DONE]`. Completed requests only. |
| Deadline misses | First token after the deadline and completion after the deadline, counted separately, over requests sent. A request that failed or was cancelled missed both. |
| Requests sent, completed, failed, cancelled at the deadline, cut at the end of the phase; HTTP status counts | Counted. |
| Output tokens per second | Per class over the time the class was active, and per stream after its first token. |
| Prompt, cached and completion tokens | **Read from the engine, not measured.** Whatever it puts in `usage`; `n/a` when it reports none. |
| The engine's own queue, batch size, GPU utilisation, memory | **Not measured.** `--metrics` on `llama-server` and `/metrics` on vLLM have them; this bench does not read either. |
| How long an engine keeps generating after the client hangs up | **Not measured.** `--cancel-at-deadline` closes the connection. When the engine actually frees the slot is not observed. |
| Anything about the answers | **Not measured.** The text is counted and thrown away. |
| One engine against another | **Not what this is for.** It compares an engine with itself, loaded against baseline. A `llama-server` row and a vLLM row differ in quantization, batching and context, not only in engine. |

Percentiles are nearest-rank: the p-th percentile of n samples is the
`ceil(p/100 * n)`-th smallest. It is always a value that was observed, and
with fewer than 100 samples p99 is the maximum. The sample count is printed
beside every percentile. A class with no samples prints `n/a`, never a zero.

## Check the harness against the mock

This proves the harness works. It measures nothing.

```bash
# terminal 1: two slots, queue ordered by priority, non-preemptive
python bench/inference/mock_engine.py --port 8000 --slots 2 --queue priority \
    --ttft-ms 50 --tokens-per-s 200

# terminal 2
python bench/inference/robot_bench.py --base-url http://127.0.0.1:8000/v1 \
    --priority-mode vllm --background-concurrency 4 --background-max-tokens 400 \
    --duration-s 10 --out mock-priority.json
```

Restart the mock with `--queue fcfs`, run the bench with `--priority-mode none`,
and compare the two `loaded | critical` rows. Under `fcfs` the critical stream
waits behind the queued background. Under `priority` it jumps the queue and
still waits for a slot, because the mock, like vLLM today, reorders only the
requests that are waiting. Every table produced this way is headed
**MOCK ENGINE**.

## llama.cpp (`llama-server`)

Not executed here.

```bash
llama-server -hf Qwen/Qwen2.5-1.5B-Instruct-GGUF:Q4_K_M \
    --host 127.0.0.1 --port 8080 -ngl 99 -c 16384 -np 4 --metrics
```

- `-np 4` is four slots: four generations at once, the rest deferred.
- Check the per-slot context in the startup log. With several slots the
  context given by `-c` may be divided among them.
- `--metrics` exposes `/metrics`. The bench does not read it; scrape it by
  hand during the loaded phase to see how many requests the server itself
  counts as processing and as deferred.

```bash
python bench/inference/robot_bench.py --base-url http://127.0.0.1:8080/v1 \
    --priority-mode none --background-concurrency 8 --duration-s 60 \
    --extra-body '{"temperature": 0, "cache_prompt": true}' \
    --out llama-server-np4.json
```

`llama-server` has no per-request priority, so the mode is `none`. Eight
background generations against four slots keeps every slot taken and four
requests deferred; a critical request then waits its turn. Compare
`loaded | critical` with `baseline | critical`.

## vLLM v0.34.0: reproducing issue #40004

Not executed here. vLLM is Linux-only: it needs a Linux host with an NVIDIA
GPU and the NVIDIA Container Toolkit. The bench can run anywhere and reach it
over the network, in which case the network is inside every number.

The issue is
[vllm-project/vllm#40004](https://github.com/vllm-project/vllm/issues/40004),
"Priority scheduling supports preemption of requests in the running queue by
requests in the waiting queue": with `--scheduling-policy priority`, vLLM
orders the requests that are waiting, and a request that is already running
is not interrupted for a more urgent one. The reproduction is the same load
against the same pinned image, once per policy.

The version is pinned because vLLM changes flags and formats between releases.

**Run 1, default policy (first come, first served).**

```bash
docker run --rm --runtime nvidia --gpus all --ipc=host -p 8000:8000 \
    -v ~/.cache/huggingface:/root/.cache/huggingface \
    vllm/vllm-openai:v0.34.0 \
    --model Qwen/Qwen2.5-1.5B-Instruct \
    --max-num-seqs 4 --enable-prompt-tokens-details

python bench/inference/robot_bench.py --base-url http://127.0.0.1:8000/v1 \
    --priority-mode none --background-concurrency 8 --duration-s 60 \
    --out vllm-0.34.0-fcfs.json
```

**Run 2, priority policy.** Stop the container and start it again with one
more flag, then send priorities.

```bash
docker run --rm --runtime nvidia --gpus all --ipc=host -p 8000:8000 \
    -v ~/.cache/huggingface:/root/.cache/huggingface \
    vllm/vllm-openai:v0.34.0 \
    --model Qwen/Qwen2.5-1.5B-Instruct \
    --max-num-seqs 4 --enable-prompt-tokens-details \
    --scheduling-policy priority

python bench/inference/robot_bench.py --base-url http://127.0.0.1:8000/v1 \
    --priority-mode vllm --background-concurrency 8 --duration-s 60 \
    --out vllm-0.34.0-priority.json
```

`--max-num-seqs 4` caps what runs at once, so eight background generations
saturate it: four running, four waiting. Without a cap that small the default
is large enough that a handful of requests never queue and the run shows
nothing.

**Which rows to compare.** In the first table of each run, the rows
`baseline | critical` and `loaded | critical`, columns `TTFT p50 ms` and `p99`.
The bold line under the tables is that ratio. In the second table, the column
`missed deadline: first token` of the same two rows.

| In run 2, loaded critical time to first token is... | Reading |
|---|---|
| far above run 2's baseline, and below run 1's loaded critical | Priority moved the request to the front of the waiting queue, and it still waited for a running generation to end. That is #40004. |
| about run 2's baseline | Running requests were preempted, or the engine was never saturated. Check the next paragraph before concluding anything. |
| about run 1's loaded critical | Priority had no visible effect. Check that the container really has `--scheduling-policy priority`. |

**Check that the engine was saturated** before reading any of it. In the
`loaded | background` row, time to first token should be on the order of a
whole background generation, because those requests queue behind each other.
If it is close to the critical baseline, nothing was waiting and the run says
nothing about the issue: raise `--background-concurrency` or lower
`--max-num-seqs`. A third run with `--background-concurrency 2`, below the
cap, separates waiting for a seat from sharing a batch.

Three things about `--priority-mode vllm`:

- It sends an integer `priority` in which LOWER is served earlier: critical 0,
  background 10. `--priority-mode sglang` is the opposite convention, HIGHER
  earlier: critical 10, background 0.
- vLLM honours the field only when started with `--scheduling-policy
  priority`. Without it, it raises an error for any non-zero priority, so in
  run 1 the mode must be `none`.
- The bench sends a one-token request of each class before timing anything.
  If the engine refuses one, the run stops with the engine's own message and
  writes no output.

To report a result upstream, paste the Markdown the bench prints and attach
the JSON. The JSON carries the exact command line, the engine's `/v1/models`
and `/version`, and every request. It also carries `llama-server`'s `/props`
when there is one, which can include local paths: read it before publishing.

## Reading the output

Stdout is Markdown, for pasting into an issue. Progress goes to stderr.

**Read the baseline row first.** If `baseline | critical` already misses
deadlines, or its time to first token is far above what one request alone
takes, the critical stream saturates the engine by itself: a request every
200 ms that takes 450 ms to serve needs more than two slots before any
background exists. The loaded phase means nothing until the baseline is
healthy. Lengthen `--critical-period-ms` or lower `--critical-max-tokens`.

- **First table**: latency. `TTFT n` and `e2e n` are the sample counts behind
  the percentiles to their right. `out tok/s (class)` is all the output tokens
  of the class over the time it was active; `tok/s per stream p50` is the
  decode rate of one stream after its first token. A `~` means some tokens
  were counted from stream chunks because the engine reported no usage.
- **Second table**: outcomes. `cut at phase end` are requests the bench hung
  up on when the phase ended; almost always background, and not failures. The
  three token columns are sums of what the engine reported.
- **Notes** below say what applies to this run: a refused `stream_options`, a
  load generator that fell behind its own schedule, requests that were never
  sent because `--max-inflight-critical` was reached, and the first distinct
  error of each class.

With `--cancel-at-deadline`, a critical request with no first token by its
deadline is cut off and has no time to first token at all. The percentiles
then cover only the requests that made it. Read them with `TTFT n` against
`sent`.

`--out` is JSON: the command line (API key removed), start and end times, the
load generator's platform and CPU count, what the engine says about itself,
the summary rows, and one record per request with every timestamp.

## Calibrating the token budget

The plan's token budget charges `fresh_input + alpha * cached_input + beta *
output`. `calibrate_tokens.py` estimates alpha and beta for one engine, one
model, one machine.

It sends one request at a time. For each prompt length and each `max_tokens`
it builds a prompt nobody has sent before, sends it, and sends it again. The
second send should hit the prefix cache; whether it did is read from the
engine's `usage.prompt_tokens_details.cached_tokens`, not assumed. It then
fits, by ordinary least squares,

    wall_ms = c0 + a * fresh_input + b * cached_input + c * output

and reports `alpha = b / a` and `beta = c / a` with the residual and the
number of samples.

```bash
python bench/inference/calibrate_tokens.py --base-url http://127.0.0.1:8080/v1 \
    --prompt-tokens 256,1024,2048 --max-tokens 16,64,256 --repeats 3 \
    --extra-body '{"temperature": 0}' --out calibration.json
```

Before trusting it:

- The engine must be idle. This is wall time of one stream standing in for
  GPU time, and anything else running is in the fit.
- The longest prompt plus the largest `max_tokens` must fit the context. With
  `llama-server`, start it with `-np 1` for this so the whole context belongs
  to one slot and the repeat lands on it.
- vLLM reports cached tokens only with `--enable-prompt-tokens-details`.
- It says nothing about cost under batching, where requests share each
  forward pass.
- The plan's fourth term, tokens held in the cache over time, is not
  estimated here.

It refuses rather than guesses. If the engine never reports a non-zero
`cached_tokens` there is no cached term to fit: no alpha is printed, the exit
code is 3, and what is left to measure (beta, from the first sends alone) is
printed and labelled as that. The same when the fresh-input cost is not
distinguishable from zero, or the samples do not separate the terms. Against
the mock the output opens and closes with a banner saying the numbers are the
mock's own parameters read back.

## Options worth knowing

- `--model` defaults to the first model `/v1/models` lists.
- `--api-key` is sent as a Bearer token and never written to the output.
- `--extra-body` is a JSON object merged into every request. Sampling
  settings go here; the bench sets none of its own. Its own fields (`model`,
  `messages`, `max_tokens`, `stream`, `priority`) win over it. In Windows
  PowerShell 5.1, escape the inner quotes: `'{\"temperature\": 0}'`.
- `--seed` seeds the choice of prompts and is also sent to the engine as
  `seed`. Without it the prompts use seed 0 and no `seed` is sent.
- `--prompt-file` replaces the built-in prompts. JSON, every key optional:

  ```json
  {
    "critical":   {"system": "One word: STOP or GO.", "user": ["Obstacle at 0.4 m.", "Path clear."]},
    "background": {"system": "", "user": "Write the full shift report."}
  }
  ```

  `user` is one string or a list; with a list each request takes one at
  random. The built-in critical prompt is a fixed system message and a short
  sensor line that changes every request, which is the shape a prefix cache is
  built for.
- If the engine answers a request carrying `stream_options` with a 400, the
  bench retries it once without the field. If that works it stops asking, and
  the token columns read `n/a`.

## The SDK side

[`ChatBackend.stream_chat`](../../sdk/python/src/aura/llm.py) takes the same
two things an Expertise would attach to a request:

- `priority`, in the runtime's convention (HIGHER wins). It is sent only to a
  self-hosted server and only when `AURA_INFERENCE_PRIORITY` is `vllm`
  (negated) or `sglang` (as is). Never to a hosted API.
- `deadline`, absolute unix milliseconds. The stream is cut once it passes and
  no socket wait outlasts it. No engine is sent a deadline: none reads one.

## Tests

```bash
cd bench/inference
python -m unittest discover -p 'test_*.py' -v
```

About 20 seconds, loopback only, no dependencies. They start the mock
in-process and check that the harness reports what the mock is known to do:
first come first served delays the critical stream under saturation; priority
with a free slot does not; priority with every slot busy still waits. Timings
are asserted as relations with wide margins, not as values.

## Known limits

- The load generator is Python threads. At high token rates it can be the
  bottleneck. Each critical record carries `send_lag_ms`, how late the request
  left against its schedule, and the output says so when that exceeds half a
  period.
- One connection per request, so connect time is inside time to first token.
  Negligible on loopback, not across a network.
- The mock runs every generation at full speed however many are running. It
  is a model of a queue, not of batching, and it stands in for no engine's
  performance.
- What the mock does with a non-zero `priority` under `fcfs` (refuse it)
  copies what vLLM is documented to do. That was not checked against vLLM.
