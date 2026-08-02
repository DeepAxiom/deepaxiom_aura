"""
planner — cognitive.planner (V1 planner ported to SDK v5, targeting the C2 IR).

Deliberate division of labour:
  - The LLM only CHOOSES (which skills) and EXTRACTS (parameters) — the fuzzy part.
  - This code COMPILES the plan into valid C2 IR — the exact part.
  - Safety policy: every edge into a motor.* skill gets a human-approval
    gate, decided here, never by the LLM.

Backends (env AURA_PLANNER_BACKEND):
  auto    → openai if OPENAI_API_KEY is set, else local, else keyword
  local   → local GGUF via llama.cpp (same model as llm-chat)
  openai  → OPENAI_API_KEY (+ AURA_PLANNER_OPENAI_MODEL, default gpt-4o-mini)
  keyword → deterministic no-model heuristic (degradation fallback, R15)
"""
import asyncio
import json
import logging
import os
import re
import unicodedata
import urllib.request
from pathlib import Path

from aura import Context, Skill, new_id

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("planner")

KERNEL_HTTP = os.getenv("AURA_HTTP_URL", "http://localhost:9080")
BACKEND = os.getenv("AURA_PLANNER_BACKEND", "auto")
MODELS_DIR = Path.home() / ".aura" / "models"
DEFAULT_FILE = os.getenv("AURA_MODEL_FILE", "qwen2.5-1.5b-instruct-q4_k_m.gguf")

skill = Skill()
_llm = None


def fetch_catalog() -> list[dict]:
    with urllib.request.urlopen(f"{KERNEL_HTTP}/v1/skills", timeout=10) as r:
        skills = json.load(r)
    return [s for s in skills
            if s["capability"] not in ("cognitive.planner",)
            and s["ports"].get("ingress")]


_API_KEYS = {"params", "query", "headers", "body"}
_PARAMS_RE = re.compile(r"parameters:\s*(.+)$")


def _declared_params(target: dict) -> set[str]:
    """Projections list their real parameters in the description."""
    m = _PARAMS_RE.search(target.get("description", ""))
    if not m:
        return set()
    return {p.split(":", 1)[1].strip()
            for p in m.group(1).split(",") if ":" in p}


def _normalize_api_payload(cap: str, payload: dict, target: dict) -> dict:
    """Fuzzy LLM → exact code: misplaced keys in api payloads (loose at the
    root, or under params without being declared parameters) move to body."""
    if ".api." not in cap or not isinstance(payload, dict):
        return payload or {}
    declared = _declared_params(target)
    fixed = {k: v for k, v in payload.items() if k in _API_KEYS}
    loose = {k: v for k, v in payload.items() if k not in _API_KEYS}
    params = fixed.get("params")
    if isinstance(params, dict):
        intruders = {k: v for k, v in params.items() if k not in declared}
        if intruders:
            loose.update(intruders)
            fixed["params"] = {k: v for k, v in params.items() if k in declared}
    if loose:
        body = fixed.get("body")
        fixed["body"] = {**loose, **body} if isinstance(body, dict) else loose
    return fixed


def _repair_capability(cap: str, by_cap: dict) -> str:
    """Fuzzy LLM → exact code: repair minor capability hallucinations
    (e.g. wrong type prefix) when there is exactly one real candidate."""
    if cap in by_cap:
        return cap
    tail = cap.split(".", 1)[-1]
    candidates = [c for c in by_cap if c.split(".", 1)[-1] == tail]
    if not candidates:
        last = cap.rsplit(".", 1)[-1]
        candidates = [c for c in by_cap if c.rsplit(".", 1)[-1] == last]
    if len(candidates) == 1:
        log.info("capability repaired: %s -> %s", cap, candidates[0])
        return candidates[0]
    return cap


def compile_plan(goal: str, reasoning: str, steps: list[dict],
                 catalog: list[dict], cause: str) -> dict:
    by_cap = {s["capability"]: s for s in catalog}
    graph_id = "plan-" + new_id()[:10].lower()
    nodes, edges, inputs = [], [], []

    for i, step in enumerate(steps, 1):
        cap = _repair_capability(step["capability"], by_cap)
        target = by_cap.get(cap)
        if target is None:
            raise ValueError(f"plan references an unknown capability: {step['capability']}")
        ref = f"s{i}"
        ingress = target["ports"]["ingress"][0]
        nodes.append({"ref": ref, "resolve": cap})

        edge_in = {"from": f"client.step{i}_out", "to": f"{ref}.{ingress['name']}"}
        if target["type"] == "motor":
            edge_in["gate"] = "human-approval"
        edges.append(edge_in)

        for egress in target["ports"].get("egress", []):
            edges.append({"from": f"{ref}.{egress['name']}", "to": "client.text_in"})

        inputs.append({
            "port": f"step{i}_out",
            "schema": ingress["schema"],
            "payload": _normalize_api_payload(cap, step.get("payload") or {}, target),
        })

    graph = {
        "ir": "1", "graph_id": graph_id,
        "origin": {"kind": "planner", "skill": skill.manifest["id"], "cause": cause},
        "nodes": nodes, "edges": edges,
    }
    return {"reasoning": reasoning, "graph": graph, "inputs": inputs}


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


def _catalog_lines(catalog: list[dict]) -> str:
    lines = []
    for s in catalog:
        ingress = s["ports"]["ingress"][0]
        lines.append(f"- {s['capability']} :: {s['description']} :: {ingress['schema']}")
    return "\n".join(lines)


def plan_openai(goal: str, catalog: list[dict]) -> tuple[str, list[dict]]:
    body = json.dumps({
        "model": os.getenv("AURA_PLANNER_OPENAI_MODEL", "gpt-4o-mini"),
        "response_format": {"type": "json_object"},
        "messages": [{"role": "user", "content": PROMPT.format(
            catalog=_catalog_lines(catalog), goal=goal)}],
    }).encode()
    req = urllib.request.Request(
        "https://api.openai.com/v1/chat/completions", data=body,
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {os.environ['OPENAI_API_KEY']}"})
    with urllib.request.urlopen(req, timeout=60) as r:
        out = json.load(r)
    parsed = json.loads(out["choices"][0]["message"]["content"])
    return parsed.get("reasoning", ""), parsed["steps"]


def plan_local(goal: str, catalog: list[dict]) -> tuple[str, list[dict]]:
    global _llm
    if _llm is None:
        from llama_cpp import Llama
        path = os.getenv("AURA_MODEL_PATH") or str(MODELS_DIR / DEFAULT_FILE)
        log.info("loading local model %s ...", path)
        _llm = Llama(model_path=path, n_ctx=8192, n_gpu_layers=-1, verbose=False)
    out = _llm.create_chat_completion(
        messages=[{"role": "user", "content": PROMPT.format(
            catalog=_catalog_lines(catalog), goal=goal)}],
        max_tokens=1024, temperature=0.1,
        response_format={"type": "json_object"},
    )
    parsed = json.loads(out["choices"][0]["message"]["content"])
    steps = parsed["steps"]
    if not isinstance(steps, list) or not steps:
        raise ValueError("model produced no steps")
    return parsed.get("reasoning", ""), steps


def make_plan(goal: str, catalog: list[dict]) -> tuple[str, str, list[dict]]:
    """Returns (backend_used, reasoning, steps) with a degradation chain."""
    order = {
        "auto": (["openai"] if os.getenv("OPENAI_API_KEY") else []) + ["local", "keyword"],
        "openai": ["openai"], "local": ["local", "keyword"], "keyword": ["keyword"],
    }.get(BACKEND, ["keyword"])
    last_err = None
    for backend in order:
        try:
            fn = {"openai": plan_openai, "local": plan_local, "keyword": plan_keyword}[backend]
            reasoning, steps = fn(goal, catalog)
            return backend, reasoning, steps
        except Exception as exc:  # noqa: BLE001
            last_err = exc
            log.warning("backend %s failed (%s); degrading", backend, exc)
    raise RuntimeError(f"all backends failed: {last_err}")


@skill.on("goal_in")
async def handle(ctx: Context) -> None:
    goal = ((ctx.payload or {}).get("text") or "").strip()
    if not goal:
        return
    await ctx.status("status_out", "working", "reading skill catalog...")
    catalog = await asyncio.to_thread(fetch_catalog)
    await ctx.status("status_out", "working",
                     f"planning with {len(catalog)} available skills...")
    try:
        backend, reasoning, steps = await asyncio.to_thread(make_plan, goal, catalog)
        log.info("raw steps (%s): %s", backend, json.dumps(steps, ensure_ascii=False))
        plan = compile_plan(goal, reasoning, steps, catalog, cause=ctx.id)
        await ctx.status("status_out", "working", f"plan generated (backend: {backend})")
        await ctx.emit("plan_out", plan)
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"planning failed: {exc}")


if __name__ == "__main__":
    skill.run()
