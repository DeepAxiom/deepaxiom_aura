"""
backends.py — planner's model backends and their degradation chain.

Three ways to turn a goal into steps, in increasing order of "no model
needed": `plan_openai` (OpenAI-compatible cloud), `plan_local` (local GGUF
via llama.cpp, same model family as llm-chat), `plan_keyword` (deterministic
no-model heuristic, the R15 degradation floor — always available). `llama_cpp`
is imported lazily inside `plan_local`, not at module load, so this module
stays importable (and unit-testable) in a lane without `llama-cpp-python`
installed — same pattern as skills/llm-chat and skills/asr.

`make_plan` is the orchestrator: given `AURA_PLANNER_BACKEND`, it tries the
right ordered sequence of backends and falls through to the next one on any
failure, ending in `keyword` if nothing else works.
"""
import json
import logging
import os
import re
import unicodedata
import urllib.request
from pathlib import Path

from aura.models import active_model_path, active_record, models_dir
from catalog import catalog_lines

log = logging.getLogger("planner")

MODELS_DIR = Path.home() / ".aura" / "models"
DEFAULT_FILE = os.getenv("AURA_MODEL_FILE", "qwen2.5-1.5b-instruct-q4_k_m.gguf")

_llm = None


def resolve_model(config_model: str = "") -> str:
    """Which GGUF the local backend loads, and why.

    The same order `skills/llm-chat/models.py` uses, so the two skills are
    configured the same way and a person only has to learn it once:

    1. ``AURA_MODEL_PATH`` — an explicit override, honoured verbatim.
    2. this skill's ``model`` config key — *this* skill's own choice, which is
       what makes it possible to plan with one model and chat with another.
    3. ``active.json`` — whatever `model-manager` marked active, the node-wide
       default. Leaving the config key empty is how you say "follow the node".
    4. the built-in default.

    Each step falls through rather than failing when what it names is absent,
    so a config pointing at a deleted model degrades instead of leaving
    `aura do` with no local backend at all.
    """
    explicit = os.getenv("AURA_MODEL_PATH", "").strip()
    if explicit:
        log.info("model: %s (via AURA_MODEL_PATH)", explicit)
        return explicit

    directory = models_dir()

    chosen = (config_model or "").strip()
    if chosen:
        candidate = directory / chosen
        if candidate.is_file():
            log.info("model: %s (via this skill's `model` config)", candidate)
            return str(candidate)
        log.warning("config names %r but it is not in %s — falling through", chosen, directory)

    active = active_model_path(directory)
    if active:
        log.info("model: %s (via model-manager active.json)", active)
        return str(active)
    if active_record(directory):
        log.warning("active.json names a model that is gone — falling through")

    fallback = MODELS_DIR / DEFAULT_FILE
    log.info("model: %s (built-in default)", fallback)
    return str(fallback)



def _words(text: str) -> set[str]:
    text = unicodedata.normalize("NFKD", text.lower())
    text = "".join(c for c in text if not unicodedata.combining(c))
    return {w[:4] for w in re.findall(r"[a-z0-9]{3,}", text)}


def plan_keyword(goal: str, catalog: list[dict]) -> tuple[str, list[dict]]:
    goal_words = _words(goal)
    best, best_score = None, 0
    for s in catalog:
        score = len(goal_words & _words(s["capability"] + " " + s["description"]))
        if score > best_score:
            best, best_score = s, score
    if best is None:
        raise ValueError("no catalog skill matches the goal")
    reasoning = (f"[keyword] skill {best['id']} ({best['capability']}) is the "
                 f"closest match for the goal; no model, so no parameter extraction")
    return reasoning, [{"capability": best["capability"], "payload": {}}]


PROMPT = """You are a planner. Turn the user's GOAL into a JSON plan.

CATALOG of available skills (capability :: description :: input schema):
{catalog}

Rules:
- Reply with ONLY a JSON object: {{"reasoning": "...", "steps": [{{"capability": "...", "payload": {{...}}}}]}}
- Copy the capability EXACTLY as written in the catalog, using as few steps as possible.
- Extract ONLY values that appear in the GOAL. Do NOT invent fields or example values.
- For *.api.* capabilities: payload = {{"params": {{...}}, "query": {{...}}, "body": {{...}}}}
  * "params" is ONLY for parameters listed as path:... in the description.
  * Resource data (name, fields, etc.) ALWAYS goes in "body".
  * Omit empty keys.

GOAL: {goal}"""


def plan_openai(goal: str, catalog: list[dict], config: dict) -> tuple[str, list[dict]]:
    body = json.dumps({
        "model": config["openai_model"],
        "max_tokens": config["max_tokens"],
        "temperature": config["temperature"],
        "response_format": {"type": "json_object"},
        "messages": [{"role": "user", "content": PROMPT.format(
            catalog=catalog_lines(catalog), goal=goal)}],
    }).encode()
    req = urllib.request.Request(
        "https://api.openai.com/v1/chat/completions", data=body,
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {os.environ['OPENAI_API_KEY']}"})
    with urllib.request.urlopen(req, timeout=60) as r:
        out = json.load(r)
    parsed = json.loads(out["choices"][0]["message"]["content"])
    return parsed.get("reasoning", ""), parsed["steps"]


def plan_local(goal: str, catalog: list[dict], config: dict) -> tuple[str, list[dict]]:
    global _llm
    if _llm is None:
        from llama_cpp import Llama
        path = resolve_model(config.get("model", ""))
        log.info("loading local model %s ...", path)
        # n_ctx/n_gpu_layers are restart_required (see skill.yaml): read once
        # here, at first load, and baked into this process-lifetime singleton
        # — a later config change only takes effect after a restart.
        _llm = Llama(model_path=path, n_ctx=config["n_ctx"],
                     n_gpu_layers=config["n_gpu_layers"], verbose=False)
    out = _llm.create_chat_completion(
        messages=[{"role": "user", "content": PROMPT.format(
            catalog=catalog_lines(catalog), goal=goal)}],
        max_tokens=config["max_tokens"], temperature=config["temperature"],
        response_format={"type": "json_object"},
    )
    parsed = json.loads(out["choices"][0]["message"]["content"])
    steps = parsed["steps"]
    if not isinstance(steps, list) or not steps:
        raise ValueError("model produced no steps")
    return parsed.get("reasoning", ""), steps


_BACKEND_FNS = {"openai": plan_openai, "local": plan_local, "keyword": plan_keyword}


def make_plan(goal: str, catalog: list[dict], *, backend: str,
              config: dict) -> tuple[str, str, list[dict]]:
    """Returns (backend_used, reasoning, steps) with a degradation chain."""
    order = {
        "auto": (["openai"] if os.getenv("OPENAI_API_KEY") else []) + ["local", "keyword"],
        "openai": ["openai"], "local": ["local", "keyword"], "keyword": ["keyword"],
    }.get(backend, ["keyword"])
    last_err = None
    for name in order:
        try:
            fn = _BACKEND_FNS[name]
            reasoning, steps = fn(goal, catalog) if name == "keyword" else fn(goal, catalog, config)
            return name, reasoning, steps
        except Exception as exc:  # noqa: BLE001
            last_err = exc
            log.warning("backend %s failed (%s); degrading", name, exc)
    raise RuntimeError(f"all backends failed: {last_err}")
