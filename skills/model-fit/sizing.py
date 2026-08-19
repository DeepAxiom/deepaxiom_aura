"""How much memory a model needs, and whether this machine has it.

The arithmetic is deliberately simple and every constant is named, because the
useful output of a sizing tool is not a verdict — it is a verdict you can argue
with. A number with no visible assumptions is a number nobody can check, so
`score` returns the inputs it used alongside the answer.

Keyed by `model_id`, the same key `skills/model-manager/catalog.py` uses, so a
recommendation here names something that skill can actually fetch. Nothing
enforces that at runtime — they are separate processes — so `test_sizing.py`
asserts every id here exists there, which turns drift into a failed test rather
than a recommendation for a model nobody can download.
"""
from __future__ import annotations

GIB = 1024 ** 3

# Bits per weight, by quantization. Q4_K_M is ~4.5 in practice rather than 4:
# k-quants keep some tensors at higher precision, and pretending otherwise
# under-estimates every model in the catalogue by roughly a tenth.
BITS_PER_WEIGHT = {
    "q4_k_m": 4.5,
    "q4_0": 4.5,
    "q5_k_m": 5.5,
    "q6_k": 6.6,
    "q8_0": 8.5,
    "f16": 16.0,
}

# Runtime overhead beyond the weights: KV cache, compute buffers, the process
# itself. Scales with context, so it is computed rather than fixed.
BASE_OVERHEAD_BYTES = 400 * 1024 * 1024

# KV cache bytes per token, per billion parameters, at f16. A rough but stable
# proxy: real cost depends on layer count and head dimensions, which the
# catalogue does not carry and which do not vary enough to change a verdict.
KV_BYTES_PER_TOKEN_PER_B = 128 * 1024

# Memory bandwidth, GB/s, used to estimate tokens/sec. Generation is
# bandwidth-bound, not compute-bound: every token reads the whole model.
#
# Per-card, because a single "nvidia" number is wrong by a factor of four across
# the range and produces estimates nobody should act on — a GTX 1650 moves
# ~192 GB/s and a 4090 ~1008. Matched on a substring of the reported name;
# anything unrecognised falls back to a deliberately pessimistic default, since
# an estimate that disappoints is better than one that oversells.
GPU_BANDWIDTH_GBS = {
    "gtx 1650": 192.0, "gtx 1660": 336.0,
    "rtx 2060": 336.0, "rtx 2070": 448.0, "rtx 2080": 448.0,
    "rtx 3060": 360.0, "rtx 3070": 448.0, "rtx 3080": 760.0, "rtx 3090": 936.0,
    "rtx 4060": 272.0, "rtx 4070": 504.0, "rtx 4080": 717.0, "rtx 4090": 1008.0,
    "rtx 5070": 672.0, "rtx 5080": 960.0, "rtx 5090": 1792.0,
    "a100": 1555.0, "h100": 3350.0, "l40": 864.0,
}
BANDWIDTH_GBS = {
    "nvidia": 300.0,   # unrecognised discrete card, chosen low on purpose
    "apple": 200.0,    # unified memory, M-series average
    "cpu": 40.0,       # dual-channel DDR5
}


def gpu_bandwidth(gpu: dict) -> float:
    """Bandwidth for one GPU, by name where known and pessimistic where not."""
    name = (gpu.get("name") or "").lower()
    for needle, gbs in GPU_BANDWIDTH_GBS.items():
        if needle in name:
            return gbs
    return BANDWIDTH_GBS.get(gpu.get("vendor", "nvidia"), BANDWIDTH_GBS["nvidia"])

# Params in billions, and the quantization the catalogue ships for each model.
MODELS = {
    "qwen2.5-1.5b-instruct": {"params_b": 1.5, "quant": "q4_k_m", "quality": 2, "kind": "llm"},
    "gemma-3-1b-it":         {"params_b": 1.0, "quant": "q4_k_m", "quality": 2, "kind": "llm"},
    "gemma-3-4b-it":         {"params_b": 4.3, "quant": "q4_k_m", "quality": 3, "kind": "llm"},
    "deepseek-r1-qwen-7b":   {"params_b": 7.6, "quant": "q4_k_m", "quality": 4, "kind": "llm"},
    # An embedding model generates no tokens, so tokens/sec is not a number
    # about it. Scored for memory, reported with estimated_tps None.
    "nomic-embed-text-v1.5": {"params_b": 0.14, "quant": "q8_0", "quality": 3, "kind": "embedding"},
}


def weights_bytes(params_b: float, quant: str) -> int:
    bits = BITS_PER_WEIGHT.get(quant, 4.5)
    return int(params_b * 1e9 * bits / 8)


def required_bytes(params_b: float, quant: str, context: int) -> int:
    kv = int(KV_BYTES_PER_TOKEN_PER_B * params_b * context / 1000)
    return weights_bytes(params_b, quant) + kv + BASE_OVERHEAD_BYTES


def score(model_id: str, hw: dict, context: int = 8192) -> dict | None:
    """Score one model against detected hardware.

    Three verdicts, and the middle one is the one that matters: a model that
    does not fit in VRAM but fits in RAM *will run*, just slowly. Reporting it
    as a failure would hide the only option a laptop without a discrete GPU has.
    """
    spec = MODELS.get(model_id)
    if not spec:
        return None

    need = required_bytes(spec["params_b"], spec["quant"], context)
    vram = hw.get("vram_bytes") or 0
    ram = hw.get("ram_bytes") or 0

    # Never plan to use every byte: an OS that starts swapping mid-generation is
    # indistinguishable, to a user, from a hang.
    usable_ram = int(ram * 0.7)

    if vram and need <= vram:
        placement, bandwidth = "gpu", gpu_bandwidth(hw["gpus"][0])
        verdict = "fits"
    elif usable_ram and need <= usable_ram:
        placement, bandwidth = "cpu", BANDWIDTH_GBS["cpu"]
        verdict = "fits" if not vram else "fits-on-cpu"
    else:
        placement, bandwidth = "cpu", BANDWIDTH_GBS["cpu"]
        verdict = "too-big"

    # Tokens/sec ≈ bandwidth / bytes-read-per-token. Every token reads the
    # weights once, so the weights are the denominator.
    w = weights_bytes(spec["params_b"], spec["quant"])
    # Only for models that emit tokens. An embedding model runs once per input
    # and reports nothing here rather than a large, meaningless figure.
    tps = round((bandwidth * 1e9) / w, 1) if w and spec["kind"] == "llm" else None

    return {
        "model_id": model_id,
        "verdict": verdict,
        "placement": placement,
        "quality": spec["quality"],
        "params_b": spec["params_b"],
        "quant": spec["quant"],
        "required_bytes": need,
        "required_gib": round(need / GIB, 2),
        "kind": spec["kind"],
        "estimated_tps": tps,
        # The inputs, shipped with the answer, so the estimate can be argued
        # with instead of believed.
        "assumptions": {
            "bits_per_weight": BITS_PER_WEIGHT.get(spec["quant"], 4.5),
            "context_tokens": context,
            "bandwidth_gbs": bandwidth,
            "usable_ram_fraction": 0.7,
            "overhead_bytes": BASE_OVERHEAD_BYTES,
        },
    }


def rank(hw: dict, context: int = 8192) -> list[dict]:
    """Every model, best fit first.

    Ordered by what a person actually wants: something that runs, then the best
    of what runs, then the fastest of those. A model that does not fit is still
    listed — "you would need 6 GB more" is useful, and hiding it invites the
    question a second time.
    """
    scored = [s for s in (score(mid, hw, context) for mid in MODELS) if s]
    order = {"fits": 0, "fits-on-cpu": 1, "too-big": 2}
    scored.sort(key=lambda s: (order[s["verdict"]], -s["quality"], -(s["estimated_tps"] or 0)))
    return scored
