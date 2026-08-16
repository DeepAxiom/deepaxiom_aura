"""
planner — cognitive.planner (V1 planner ported to SDK v5, targeting the C2 IR).

Deliberate division of labour:
  - The LLM only CHOOSES (which skills) and EXTRACTS (parameters) — the fuzzy part.
  - This code COMPILES the plan into valid C2 IR — the exact part.
  - Safety policy: every edge into a motor.* skill gets a human-approval
    gate, decided here, never by the LLM.

Domain logic lives in catalog.py (live catalog fetch + prompt formatting),
backends.py (the openai/local/keyword degradation chain) and compile.py (IR
compilation + the motor.* gating policy) — this module is just the
`goal_in` handler orchestrating catalog -> backends -> compile.

Backends (env AURA_PLANNER_BACKEND):
  auto    → openai if OPENAI_API_KEY is set, else local, else keyword
  local   → local GGUF via llama.cpp (same model as llm-chat)
  openai  → OPENAI_API_KEY (+ config `openai_model`, default gpt-4o-mini)
  keyword → deterministic no-model heuristic (degradation fallback, R15)

Runtime-tunable (C1 `config`, see skill.yaml): temperature, max_tokens and
openai_model (hot — applied on the next goal) and n_ctx/n_gpu_layers
(restart_required — the local model is loaded once per process and only
picks up a new value on the next start). AURA_PLANNER_BACKEND stays an env
var on purpose: it picks which code path runs, not a tunable value.
"""
import asyncio
import json
import logging
import os

from aura import Context, Skill

from backends import make_plan
from catalog import fetch_catalog
from compile import compile_plan

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("planner")

BACKEND = os.getenv("AURA_PLANNER_BACKEND", "auto")

skill = Skill()


@skill.on("goal_in")
async def handle(ctx: Context) -> None:
    goal = ((ctx.payload or {}).get("text") or "").strip()
    if not goal:
        return
    await ctx.status("status_out", "working", "reading skill catalog...")
    try:
        # fetch_catalog lives inside this try on purpose: a kernel that is
        # unreachable right now (timeout, connection refused, ...) is a
        # planning failure like any other, reported through ctx.error below —
        # it must never propagate as an uncaught exception with no
        # status_out/error reaching the caller.
        catalog = await asyncio.to_thread(fetch_catalog)
        await ctx.status("status_out", "working",
                         f"planning with {len(catalog)} available skills...")
        backend, reasoning, steps = await asyncio.to_thread(
            make_plan, goal, catalog, backend=BACKEND, config=dict(skill.config))
        log.info("raw steps (%s): %s", backend, json.dumps(steps, ensure_ascii=False))
        plan = compile_plan(goal, reasoning, steps, catalog, cause=ctx.id,
                            skill_id=skill.manifest["id"])
        await ctx.status("status_out", "working", f"plan generated (backend: {backend})")
        await ctx.emit("plan_out", plan)
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"planning failed: {exc}")


if __name__ == "__main__":
    skill.run()
