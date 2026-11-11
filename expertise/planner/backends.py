"""
backends.py — planner's model backends and their degradation chain.

Three ways to turn a goal into steps, in increasing order of "no model
needed": `plan_openai` (OpenAI-compatible cloud), `plan_local` (local GGUF
via llama.cpp, same model family as llm-chat), `plan_keyword` (deterministic
no-model heuristic, the R15 degradation floor — always available). `llama_cpp`
is imported lazily inside `plan_local`, not at module load, so this module
stays importable (and unit-testable) in a lane without `llama-cpp-python`
installed — same pattern as expertise/llm-chat and expertise/asr.

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

    The same order `expertise/llm-chat/models.py` uses, so the two skills are
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


def remote_endpoint() -> tuple[str, str, str] | None:
    """Where the remote backend sends a plan request: (base_url, key, model).

    The same two doors `aura.llm.ChatBackend` has, in the same order, so the
    planner and llm-chat cannot end up on different endpoints by accident:

    1. ``OPENAI_API_KEY`` — a hosted API, at ``OPENAI_BASE_URL`` if set. That
       variable used to be ignored here: the URL was a literal, so a node
       configured for Gemini chatted with Gemini and planned against OpenAI
       with a Gemini key, and the failure read as a bad credential.
    2. ``AURA_INFERENCE_URL`` — an inference server this node runs itself. No
       key. This is what lets the planner stop loading its own copy of the
       weights: it becomes one more client of the server llm-chat already
       uses.

    None when neither is configured.
    """
    key = os.getenv("OPENAI_API_KEY", "").strip()
    if key:
        return os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1").strip(), key, ""
    server = os.getenv("AURA_INFERENCE_URL", "").strip()
    if server:
        return server, "", os.getenv("AURA_INFERENCE_MODEL", "").strip()
    return None


def plan_openai(goal: str, catalog: list[dict], config: dict) -> tuple[str, list[dict]]:
    endpoint = remote_endpoint()
    if endpoint is None:
        raise RuntimeError("no remote endpoint: set OPENAI_API_KEY or AURA_INFERENCE_URL")
    base_url, key, server_model = endpoint
    body = json.dumps({
        "model": server_model or config["openai_model"],
        "max_tokens": config["max_tokens"],
        "temperature": config["temperature"],
        "response_format": {"type": "json_object"},
        "messages": [{"role": "user", "content": PROMPT.format(
            catalog=catalog_lines(catalog), goal=goal)}],
    }).encode()
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/chat/completions", data=body, headers=headers)
    with urllib.request.urlopen(req, timeout=60) as r:
        out = json.load(r)
    parsed = json.loads(out["choices"][0]["message"]["content"])
    return parsed.get("reasoning", ""), parsed["steps"]


def plan_grammar(catalog: list[dict]) -> str:
    """A GBNF grammar under which the model can only name a real capability.

    The prompt says "copy the capability EXACTLY as written", and a small
    model mostly does. When it does not — `motor.email.sent` for
    `motor.email.send` — the plan fails at compile, after the tokens were
    spent, and the user is told planning failed. Constraining the decoder
    moves that from a rule the model is asked to follow to one it cannot
    break: the `capability` string is an alternation over the live catalog,
    so an invented name is not a sequence of tokens the sampler can emit.

    Everything else stays ordinary JSON, because the payload's shape depends
    on the skill and the compile step already validates it.

    Key order is fixed (`reasoning` then `steps`, `capability` then
    `payload`): a grammar that allowed any order would have to enumerate
    them, and the consumer reads by key anyway.
    """
    capabilities = sorted({s["capability"] for s in catalog})
    if not capabilities:
        raise ValueError("an empty catalog has no grammar")
    alternatives = " | ".join(_gbnf_literal(json.dumps(c)) for c in capabilities)
    return "\n".join([
        r'root ::= "{" ws "\"reasoning\"" ws ":" ws string ws "," ws "\"steps\"" ws ":" ws steps ws "}"',
        r'steps ::= "[" ws step (ws "," ws step)* ws "]"',
        r'step ::= "{" ws "\"capability\"" ws ":" ws capability ws "," ws "\"payload\"" ws ":" ws object ws "}"',
        f"capability ::= {alternatives}",
        r'value ::= object | array | string | number | "true" | "false" | "null"',
        r'object ::= "{" ws ( member (ws "," ws member)* )? ws "}"',
        r'member ::= string ws ":" ws value',
        r'array ::= "[" ws ( value (ws "," ws value)* )? ws "]"',
        r'string ::= "\"" ( [^"\\\x00-\x1f] | "\\" ( ["\\/bfnrt] | "u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] ) )* "\""',
        r'number ::= "-"? ( "0" | [1-9] [0-9]* ) ( "." [0-9]+ )? ( [eE] [-+]? [0-9]+ )?',
        r'ws ::= [ \t\n]*',
    ]) + "\n"


def _gbnf_literal(text: str) -> str:
    """Quote a string as a GBNF terminal."""
    return '"' + text.replace("\\", "\\\\").replace('"', '\\"') + '"'


def _local_constraint(catalog: list[dict]) -> dict:
    """The decoding constraint for the local backend, strongest first.

    A catalog-specific grammar when llama.cpp will take it; the generic "some
    JSON object" mode it always used otherwise. Never an error: a planner that
    refused to plan because a grammar failed to parse would be worse than the
    one that occasionally misspells a capability.
    """
    try:
        from llama_cpp import LlamaGrammar

        return {"grammar": LlamaGrammar.from_string(plan_grammar(catalog), verbose=False)}
    except Exception as exc:  # noqa: BLE001
        log.info("planning without a capability grammar (%s)", exc)
        return {"response_format": {"type": "json_object"}}


def plan_local(goal: str, catalog: list[dict], config: dict) -> tuple[str, list[dict]]:
    global _llm
    if _llm is None:
        from llama_cpp import Llama
        path = resolve_model(config.get("model", ""))
        log.info("loading local model %s ...", path)
        # n_ctx/n_gpu_layers are restart_required (see expertise.yaml): read once
        # here, at first load, and baked into this process-lifetime singleton
        # — a later config change only takes effect after a restart.
        _llm = Llama(model_path=path, n_ctx=config["n_ctx"],
                     n_gpu_layers=config["n_gpu_layers"], verbose=False)
    out = _llm.create_chat_completion(
        messages=[{"role": "user", "content": PROMPT.format(
            catalog=catalog_lines(catalog), goal=goal)}],
        max_tokens=config["max_tokens"], temperature=config["temperature"],
        **_local_constraint(catalog),
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
        # A configured remote endpoint — hosted or this node's own server —
        # goes first: with a server running, loading a second copy of the
        # weights in this process is the thing to avoid.
        "auto": (["openai"] if remote_endpoint() else []) + ["local", "keyword"],
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
