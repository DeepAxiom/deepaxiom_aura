#!/usr/bin/env python3
"""calibrate_tokens — what a cached input token and an output token cost an
engine, relative to a fresh input token.

A token budget that charges every token the same is wrong in both directions:
an output token costs a forward pass each, an input token is evaluated in
bulk, and an input token served from the prefix cache is barely evaluated at
all. The budget this calibrates has the form

    cost = fresh_input + alpha * cached_input + beta * output

and this script estimates alpha and beta for one engine, one model, one
machine.

Method
------
One request at a time against an otherwise idle engine. For each combination
of a prompt length and a `max_tokens`, a prompt nobody has sent before (it
starts with a nonce, so no prefix of it can be cached) is sent twice in a
row. The first send pays for the whole prompt. The second finds it in the
engine's prefix cache. Whether it really did is read from the engine's own
`usage.prompt_tokens_details.cached_tokens`, never assumed.

Then an ordinary least-squares fit of each request's wall time against what
the engine counted:

    wall_ms = c0 + a * fresh_input + b * cached_input + c * output

with fresh_input = prompt_tokens - cached_tokens, and alpha = b / a,
beta = c / a. c0 absorbs what every request costs regardless of size.

What this is and is not
-----------------------
- It is wall time of a single stream, standing in for GPU time. It includes
  HTTP, tokenization and sampling on the CPU. Run it with nothing else on the
  engine or the fit is measuring the other load.
- It says nothing about cost under batching, where requests share each
  forward pass and the ratio between input and output cost shifts.
- If the engine never reports a non-zero `cached_tokens`, there is no cached
  term to fit and no alpha is printed. What can still be measured, beta from
  the first sends alone, is printed and labelled as such.
- Against the bundled mock the result is the mock's own sleep() parameters
  read back. That proves the arithmetic and nothing else.

Standard library only; the least-squares solve is written out below.

    python calibrate_tokens.py --base-url http://127.0.0.1:8080/v1 --out calibration.json
"""
from __future__ import annotations

import argparse
import json
import math
import random
import secrets
import statistics
import sys
import time

from robot_bench import (Endpoint, UsageSupport, json_object_arg, positive_arg, utc_iso,
                         describe_engine, first_model_id, stream_request)

EXIT_NO_FIT = 3

MOCK_BANNER = (
    "#" * 78 + "\n"
    "#  MOCK ENGINE. Every number below is the mock's own sleep() parameters read\n"
    "#  back through the fit. It shows the script works. It is NOT a calibration\n"
    "#  of any model, engine or GPU, and must not be used to set a budget.\n"
    + "#" * 78)

_WORDS = (
    "the robot moved along aisle four and checked each shelf for a missing tote "
    "before it turned back toward the dock where a second unit was waiting with "
    "its arm raised over an open crate of small parts sorted by size and weight "
    "while the floor sensor reported dust on the north ramp and a door left ajar"
).split()


def log(message: str) -> None:
    sys.stderr.write(f"calibrate_tokens: {message}\n")
    sys.stderr.flush()


# --------------------------------------------------------------------------
# Least squares, by hand.
# --------------------------------------------------------------------------

class SingularFit(Exception):
    """The samples cannot tell the terms of the model apart."""


def _invert(matrix: list[list[float]]) -> list[list[float]]:
    """Gauss-Jordan inverse with partial pivoting. Small symmetric systems
    only: this is for a 3x3 or 4x4 normal matrix."""
    k = len(matrix)
    aug = [list(row) + [1.0 if i == j else 0.0 for j in range(k)]
           for i, row in enumerate(matrix)]
    largest = max(abs(v) for row in matrix for v in row) or 1.0
    for col in range(k):
        pivot = max(range(col, k), key=lambda r: abs(aug[r][col]))
        if abs(aug[pivot][col]) < 1e-10 * largest:
            raise SingularFit("the columns of the design are linearly dependent")
        aug[col], aug[pivot] = aug[pivot], aug[col]
        scale = aug[col][col]
        aug[col] = [v / scale for v in aug[col]]
        for row in range(k):
            if row != col and aug[row][col] != 0.0:
                factor = aug[row][col]
                aug[row] = [v - factor * p for v, p in zip(aug[row], aug[col])]
    return [row[k:] for row in aug]


def least_squares(rows: list[list[float]], y: list[float]) -> dict:
    """Ordinary least squares of y on the columns of `rows`.

    Solved through the normal equations, (X'X) w = X'y. That squares the
    condition number, which matters when a column of ones sits beside a
    column of token counts in the thousands — so every column is first
    divided by its largest magnitude, and the coefficients scaled back.

    Returns the coefficients, their standard errors (None when there are no
    degrees of freedom left to estimate them from), the residual sum of
    squares and R^2. Raises SingularFit when the samples do not separate the
    terms: two columns that move together, or fewer samples than terms.
    """
    n = len(rows)
    k = len(rows[0]) if rows else 0
    if n < k or k == 0:
        raise SingularFit(f"{n} sample(s) cannot determine {k} coefficient(s)")
    scale = [max(abs(row[j]) for row in rows) for j in range(k)]
    if any(s == 0.0 for s in scale):
        raise SingularFit("one of the terms is zero in every sample")
    x = [[row[j] / scale[j] for j in range(k)] for row in rows]
    gram = [[sum(r[i] * r[j] for r in x) for j in range(k)] for i in range(k)]
    moment = [sum(r[i] * yi for r, yi in zip(x, y)) for i in range(k)]
    inverse = _invert(gram)
    scaled = [sum(inverse[i][j] * moment[j] for j in range(k)) for i in range(k)]
    coef = [scaled[j] / scale[j] for j in range(k)]

    fitted = [sum(c * v for c, v in zip(coef, row)) for row in rows]
    rss = sum((yi - fi) ** 2 for yi, fi in zip(y, fitted))
    mean = sum(y) / n
    tss = sum((yi - mean) ** 2 for yi in y)
    dof = n - k
    stderr: list[float | None] = [None] * k
    if dof > 0:
        variance = rss / dof
        stderr = [math.sqrt(max(variance * inverse[j][j], 0.0)) / scale[j] for j in range(k)]
    return {
        "coef": coef, "stderr": stderr, "n": n, "dof": dof, "rss": rss,
        "rms": math.sqrt(rss / n),
        "r2": 1.0 - rss / tss if tss > 0 else None,
        "mean_y": mean,
    }


# --------------------------------------------------------------------------
# Collecting the samples.
# --------------------------------------------------------------------------

def make_prompt(target_tokens: int, nonce: str, rng: random.Random) -> str:
    """About `target_tokens` tokens of filler that begins with a nonce.

    The nonce comes first because a prefix cache matches from the first token:
    with it there, no earlier request — from this run or any before it — can
    have left a prefix this prompt shares. "About" because one common word is
    roughly one token and no more exactly than that; the fit uses the engine's
    count, not this target.
    """
    words = [rng.choice(_WORDS) for _ in range(max(target_tokens - 12, 1))]
    return f"[{nonce}] " + " ".join(words) + " Continue this text for as long as you can."


def collect(endpoint: Endpoint, model: str, args) -> list[dict]:
    rng = random.Random(args.seed)
    run_id = secrets.token_hex(4)
    usage_support = UsageSupport()

    def send(prompt: str, max_tokens: int, klass: str) -> dict:
        body = dict(args.extra_body)
        body.update({"model": model, "max_tokens": max_tokens,
                     "messages": [{"role": "user", "content": prompt}]})
        return stream_request(endpoint, body, klass=klass,
                              stall_timeout_s=args.request_timeout_s,
                              usage_support=usage_support)

    # Not measured: the first request to an engine pays for things no later
    # one does, and it settles whether `stream_options` is accepted.
    warm = send(make_prompt(32, f"{run_id}-warmup", rng), 1, "warmup")
    if warm["outcome"] != "completed":
        raise RuntimeError(f"the engine refused a one-token request: {warm['error']}")

    plan = [(length, max_tokens) for _ in range(args.repeats)
            for length in args.prompt_tokens for max_tokens in args.max_tokens]
    # Shuffled so that anything drifting over the run — a GPU warming up and
    # throttling — does not line up with prompt length and pass for a cost.
    rng.shuffle(plan)

    samples = []
    for i, (length, max_tokens) in enumerate(plan):
        prompt = make_prompt(length, f"{run_id}-{i}", rng)
        for which in ("first", "second"):
            rec = send(prompt, max_tokens, which)
            usage = rec["usage"] or {}
            samples.append({
                "pair": i, "send": which, "target_prompt_tokens": length,
                "max_tokens": max_tokens, "outcome": rec["outcome"], "error": rec["error"],
                "wall_ms": rec["e2e_ms"], "ttft_ms": rec["ttft_ms"],
                "prompt_tokens": usage.get("prompt_tokens"),
                "cached_tokens": usage.get("cached_tokens"),
                "completion_tokens": usage.get("completion_tokens"),
                "engine_timings": rec["engine_timings"],
            })
            log(f"{len(samples)}/{2 * len(plan)}  prompt~{length} max_tokens={max_tokens} "
                f"{which}: " + (f"{rec['e2e_ms']:.0f} ms" if rec["e2e_ms"] is not None
                                else f"{rec['outcome']} ({rec['error']})"))
    return samples


# --------------------------------------------------------------------------
# From samples to a report.
# --------------------------------------------------------------------------

def _usable(sample: dict) -> bool:
    return (sample["outcome"] == "completed" and sample["wall_ms"] is not None
            and sample["prompt_tokens"] is not None
            and sample["completion_tokens"] is not None)


def analyse(samples: list[dict]) -> dict:
    """Decide what the samples support, and fit that and nothing more."""
    usable = [s for s in samples if _usable(s)]
    reported = [s for s in usable if s["cached_tokens"] is not None]
    nonzero = [s for s in reported if s["cached_tokens"] > 0]
    out: dict = {
        "samples": len(samples), "usable": len(usable),
        "cached_reported": len(reported), "cached_nonzero": len(nonzero),
        "fit": None, "alpha": None, "beta": None, "cold_fit": None, "cold_beta": None,
        "refusal": None, "notes": [],
    }
    if not usable:
        out["refusal"] = (
            "No request came back with token counts. Either every request failed, or the "
            "engine reports no `usage` when streaming. There is nothing to fit against.")
        return out

    if not nonzero:
        if not reported:
            out["refusal"] = (
                "The engine did not report `cached_tokens` on any response, so this script "
                "cannot tell a cached input token from a fresh one. No alpha is reported. "
                "(vLLM reports it only when started with --enable-prompt-tokens-details.)")
        else:
            out["refusal"] = (
                "The engine reported `cached_tokens` = 0 on every response, including each "
                "second send of an identical prompt: its prefix cache is off or was never "
                "hit. There is no cached term to fit. No alpha is reported.")
        _fit_cold(usable, out)
        return out

    missing = len(usable) - len(reported)
    if missing:
        out["notes"].append(
            f"{missing} response(s) carried no `cached_tokens` field and were counted as 0 "
            f"cached. Some engines omit the field when nothing was cached; if this one "
            f"omits it for another reason, the fit is wrong.")
    rows, y = [], []
    for s in usable:
        cached = s["cached_tokens"] or 0
        rows.append([1.0, float(s["prompt_tokens"] - cached), float(cached),
                     float(s["completion_tokens"])])
        y.append(s["wall_ms"])
    try:
        fit = least_squares(rows, y)
    except SingularFit as exc:
        out["refusal"] = (
            f"The samples do not separate the three costs ({exc}). The fit needs prompts of "
            f"at least two lengths and at least two different output lengths that the "
            f"engine actually produced. No alpha or beta is reported.")
        return out
    fit["terms"] = ["per_request_ms", "fresh_input_ms", "cached_input_ms", "output_ms"]
    out["fit"] = fit
    a, se_a = fit["coef"][1], fit["stderr"][1]
    if a <= 0 or (se_a is not None and a < 2 * se_a):
        out["refusal"] = (
            "The cost of a fresh input token is not distinguishable from zero in this "
            "sample. alpha and beta are ratios against it and would be noise, so neither "
            "is reported. Use longer prompts (--prompt-tokens) or more --repeats.")
        return out
    out["alpha"] = fit["coef"][2] / a
    out["beta"] = fit["coef"][3] / a
    if fit["coef"][2] < 0:
        out["notes"].append(
            "The fitted cost of a cached token is negative, which no engine has: it is "
            "zero within the noise of this sample. Read alpha as 'about zero', not as the "
            "number printed.")
    return out


def _fit_cold(usable: list[dict], out: dict) -> None:
    """beta alone, from the first sends: what is left to measure when the
    engine will not say what it cached. Each first send began with a nonce,
    so its whole prompt is taken to be fresh."""
    cold = [s for s in usable if s["send"] == "first"]
    rows = [[1.0, float(s["prompt_tokens"]), float(s["completion_tokens"])] for s in cold]
    try:
        fit = least_squares(rows, [s["wall_ms"] for s in cold])
    except SingularFit:
        return
    fit["terms"] = ["per_request_ms", "input_ms", "output_ms"]
    out["cold_fit"] = fit
    a, se_a = fit["coef"][1], fit["stderr"][1]
    if a > 0 and (se_a is None or a >= 2 * se_a):
        out["cold_beta"] = fit["coef"][2] / a


def _fit_lines(fit: dict, labels: list[str]) -> list[str]:
    lines = []
    for label, coef, se in zip(labels, fit["coef"], fit["stderr"]):
        lines.append(f"  {label:<28}{coef:>12.4f} ms" + (f"   (se {se:.4f})" if se is not None
                                                        else "   (se n/a)"))
    share = fit["rms"] / fit["mean_y"] * 100.0 if fit["mean_y"] else None
    lines.append(
        f"  residual: RMS {fit['rms']:.2f} ms"
        + (f" ({share:.1f} % of the mean wall time)" if share is not None else "")
        + (f", R^2 {fit['r2']:.4f}" if fit["r2"] is not None else "")
        + f", n = {fit['n']}")
    return lines


def report(result: dict) -> str:
    samples = result["samples"]
    analysis = result["analysis"]
    out: list[str] = []
    if result["engine"].get("is_mock"):
        out += [MOCK_BANNER, ""]
    out.append(f"calibrate_tokens: {result['model']} at {result['engine']['base_url']}, "
               f"{result['started_utc']}")
    out.append(f"requests: {analysis['samples']} sent, {analysis['usable']} usable "
               f"(completed, with token counts); cached_tokens reported on "
               f"{analysis['cached_reported']}, non-zero on {analysis['cached_nonzero']}")
    out.append("")

    groups: dict = {}
    for s in samples:
        if _usable(s):
            groups.setdefault((s["target_prompt_tokens"], s["max_tokens"], s["send"]),
                              []).append(s)
    if groups:
        out.append("| prompt target | max_tokens | send | n | prompt tok | cached tok | "
                   "output tok | wall ms (median) |")
        out.append("|---|---|---|---|---|---|---|---|")
        for (length, max_tokens, which), group in sorted(groups.items()):
            cached = [s["cached_tokens"] for s in group if s["cached_tokens"] is not None]
            out.append(
                f"| {length} | {max_tokens} | {which} | {len(group)} | "
                f"{statistics.median(s['prompt_tokens'] for s in group):g} | "
                + (f"{statistics.median(cached):g}" if cached else "n/a") + " | "
                f"{statistics.median(s['completion_tokens'] for s in group):g} | "
                f"{statistics.median(s['wall_ms'] for s in group):.1f} |")
        out.append("")

    if analysis["fit"] is not None:
        out.append("fit: wall_ms = c0 + a*fresh_input + b*cached_input + c*output   "
                   "(ordinary least squares)")
        out += _fit_lines(analysis["fit"], [
            "c0  per request", "a   fresh input token", "b   cached input token",
            "c   output token"])
        out.append("")
    if analysis["alpha"] is not None:
        out.append(f"alpha (cached input / fresh input) = {analysis['alpha']:.4f}")
        out.append(f"beta  (output / fresh input)       = {analysis['beta']:.4f}")
        out.append(f"from {analysis['fit']['n']} samples, residual RMS "
                   f"{analysis['fit']['rms']:.2f} ms")
    if analysis["refusal"]:
        out.append("NOT REPORTED: " + analysis["refusal"])
    if analysis["cold_fit"] is not None:
        out.append("")
        out.append("What the first sends alone support (their prompt is taken to be all "
                   "fresh, because each began with a nonce):")
        out.append("fit: wall_ms = c0 + a*input + c*output   (ordinary least squares)")
        out += _fit_lines(analysis["cold_fit"], [
            "c0  per request", "a   input token", "c   output token"])
        if analysis["cold_beta"] is not None:
            out.append(f"beta  (output / fresh input)       = {analysis['cold_beta']:.4f}")
        else:
            out.append("beta is not reported either: the cost of an input token is not "
                       "distinguishable from zero in these samples.")
    for note in analysis["notes"]:
        out.append("note: " + note)
    failed = [s for s in samples if s["outcome"] != "completed"]
    if failed:
        out.append(f"note: {len(failed)} request(s) did not complete; the first said: "
                   f"{failed[0]['error']}")
    out.append("")
    out.append("This is wall time of one stream on an engine assumed idle, standing in for "
               "GPU time. It says nothing about cost under batching.")
    if result["engine"].get("is_mock"):
        out += ["", MOCK_BANNER]
    return "\n".join(out) + "\n"


def _int_list(text: str) -> list[int]:
    try:
        values = [int(part) for part in text.split(",") if part.strip()]
    except ValueError:
        raise argparse.ArgumentTypeError("must be comma-separated integers") from None
    if not values or any(v < 1 for v in values):
        raise argparse.ArgumentTypeError("must be one or more positive integers")
    return values


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="calibrate_tokens.py",
        description="Estimate alpha and beta of cost = fresh_input + alpha*cached_input + "
                    "beta*output for one engine, from single-stream wall time.")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080/v1")
    parser.add_argument("--model", default=None,
                        help="default: the first model /v1/models lists")
    parser.add_argument("--api-key", default=None)
    parser.add_argument("--prompt-tokens", type=_int_list, default=[256, 1024, 2048],
                        help="approximate prompt lengths, comma-separated (default "
                             "256,1024,2048). The longest must fit the model's context "
                             "together with the largest --max-tokens")
    parser.add_argument("--max-tokens", type=_int_list, default=[16, 64, 256],
                        help="output lengths to ask for, comma-separated (default 16,64,256)")
    parser.add_argument("--repeats", type=int, default=2,
                        help="times each combination is measured, with a new prompt each "
                             "time (default 2)")
    parser.add_argument("--extra-body", type=json_object_arg, default={},
                        help="JSON object merged into every request, e.g. "
                             "'{\"temperature\": 0}'")
    parser.add_argument("--seed", type=int, default=None,
                        help="seeds the filler text and the order of the requests. The "
                             "nonce that keeps prompts out of the cache is new every run "
                             "regardless")
    parser.add_argument("--request-timeout-s", type=positive_arg, default=300.0)
    parser.add_argument("--out", default=None, help="write every sample and the fit as JSON")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(sys.argv[1:] if argv is None else argv)
    if args.repeats < 1:
        log("--repeats must be at least 1")
        return 2
    started = time.time()
    try:
        endpoint = Endpoint(args.base_url, args.api_key)
    except ValueError as exc:
        log(str(exc))
        return 2
    engine = describe_engine(endpoint)
    model = args.model or first_model_id(engine)
    if not model:
        log(f"no --model given and {endpoint.base_url}/models did not name one")
        return 2
    if engine["is_mock"]:
        log("the engine is the bundled mock: the result is its own parameters read back")
    try:
        samples = collect(endpoint, model, args)
    except RuntimeError as exc:
        log(str(exc))
        return 2

    analysis = analyse(samples)
    config = {k: v for k, v in vars(args).items() if k != "api_key"}
    result = {
        "tool": "calibrate_tokens", "schema": 1,
        "started_utc": utc_iso(started), "finished_utc": utc_iso(time.time()),
        "engine": engine, "model": model, "config": config,
        "model_form": "wall_ms = c0 + a*fresh_input + b*cached_input + c*output; "
                      "alpha = b/a; beta = c/a",
        "analysis": analysis, "samples": samples,
    }
    if args.out:
        with open(args.out, "w", encoding="utf-8") as handle:
            json.dump(result, handle, indent=1)
            handle.write("\n")
        log(f"{len(samples)} samples written to {args.out}")
    sys.stdout.write(report(result))
    sys.stdout.flush()
    return 0 if analysis["alpha"] is not None else EXIT_NO_FIT


if __name__ == "__main__":
    sys.exit(main())
