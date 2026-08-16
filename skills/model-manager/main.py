"""
model-manager — motor.models.manage (V1 model-manager ported to SDK v5).

Command payload (deepaxiom/model-command@1):
  { "action": "list" | "catalog" | "search" | "download" | "delete",
    "query": str?,                      # search
    "hf_repo": str?, "hf_filename": str?,   # download (any HF file)
    "model_id": str?,                   # download from catalog / delete by name
    "model_type": str? }                # catalog filter: llm|embedding|tts|asr|vision

Being a motor skill, planner- and MCP-generated graphs gate every command
behind human approval — downloads and deletes always ask.

Domain logic lives in catalog.py (curated model list + filtering),
storage.py (local filesystem list/delete) and download.py (Hugging Face
Hub search + download) — this module is just command parsing and the
`@skill.on` handler.
"""
import asyncio
import logging
import os
from pathlib import Path

from aura import Context, Skill

from catalog import filter_catalog, find_model
from download import download_model, search_models
from storage import delete_model, list_models

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("model-manager")

skill = Skill()


def _models_dir() -> Path:
    # models_dir is restart_required (see skill.yaml), but re-reading
    # skill.config here each call is cheap and keeps a single source of
    # truth instead of caching a copy that could drift from it.
    return Path(os.path.expanduser(skill.config["models_dir"]))


@skill.on("command_in")
async def handle(ctx: Context) -> None:
    cmd = ctx.payload or {}
    action = (cmd.get("action") or "").strip()
    try:
        match action:
            case "list":
                models = list_models(_models_dir())
                await ctx.emit("result_out", {"action": action, "models": models})
            case "catalog":
                entries = filter_catalog(cmd.get("model_type"))
                await ctx.emit("result_out", {"action": action, "catalog": entries})
            case "search":
                query = (cmd.get("query") or "").strip()
                if not query:
                    raise ValueError("search requires a query")
                await ctx.status("status_out", "working", f"searching Hugging Face for {query!r}...")
                results = await asyncio.to_thread(search_models, query)
                await ctx.emit("result_out", {"action": action, "results": results})
            case "download":
                repo, filename = cmd.get("hf_repo"), cmd.get("hf_filename")
                if cmd.get("model_id") and not repo:
                    entry = find_model(cmd["model_id"])
                    if entry is None:
                        raise ValueError(f"unknown catalog model_id: {cmd['model_id']}")
                    repo, filename = entry["hf_repo"], entry["hf_filename"]
                if not repo or not filename:
                    raise ValueError("download requires hf_repo + hf_filename (or a catalog model_id)")
                await ctx.status("status_out", "working", f"downloading {repo}/{filename}...")
                path = await asyncio.to_thread(download_model, _models_dir(), repo, filename)
                await ctx.emit("result_out", {"action": action, "path": path})
            case "delete":
                name = cmd.get("model_id") or ""
                delete_model(_models_dir(), name)
                await ctx.emit("result_out", {"action": action, "deleted": name})
            case _:
                raise ValueError(f"unknown action {action!r} (list|catalog|search|download|delete)")
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"{action or 'command'} failed: {exc}")


if __name__ == "__main__":
    skill.run()
