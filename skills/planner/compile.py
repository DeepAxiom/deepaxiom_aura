"""
compile.py — compiles an LLM-produced plan into valid C2 IR.

Deliberate division of labour (see skills/planner/main.py's module
docstring): the LLM only CHOOSES (which skills) and EXTRACTS (parameters) —
the fuzzy part. This module COMPILES that choice into exact IR — the exact
part.

Safety policy, load-bearing: every edge into a `motor.*` skill gets a
human-approval gate. That decision is made here, from the CATALOG's own
declared `type` for the target skill — never from anything the LLM put in a
step (there is no field a step can set to skip it; `compile_plan` does not
even look at `step` for gating). A hallucinated or adversarial step cannot
turn this off. See test_compile.py for the adversarial case this protects.
"""
import logging
import re

from aura import new_id

log = logging.getLogger("planner")

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
                 catalog: list[dict], cause: str, skill_id: str) -> dict:
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
        # Gate decided from the catalog's declared type only. `step` is never
        # consulted here, on purpose — see module docstring.
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
        "origin": {"kind": "planner", "skill": skill_id, "cause": cause},
        "nodes": nodes, "edges": edges,
    }
    return {"reasoning": reasoning, "graph": graph, "inputs": inputs}
