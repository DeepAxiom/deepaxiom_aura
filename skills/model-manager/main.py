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
"""
import asyncio
import logging
from pathlib import Path

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("model-manager")

MODELS_DIR = Path.home() / ".aura" / "models"

CATALOG = [
    {"model_id": "qwen2.5-1.5b-instruct", "model_type": "llm", "tier": "mvp",
     "hf_repo": "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
     "hf_filename": "qwen2.5-1.5b-instruct-q4_k_m.gguf"},
    {"model_id": "gemma-3-1b-it", "model_type": "llm", "tier": "mvp",
     "hf_repo": "bartowski/gemma-3-1B-it-GGUF",
     "hf_filename": "gemma-3-1B-it-Q4_K_M.gguf"},
    {"model_id": "gemma-3-4b-it", "model_type": "llm", "tier": "recommended",
     "hf_repo": "bartowski/gemma-3-4B-it-GGUF",
     "hf_filename": "gemma-3-4B-it-Q4_K_M.gguf"},
    {"model_id": "deepseek-r1-qwen-7b", "model_type": "llm", "tier": "recommended",
     "hf_repo": "bartowski/DeepSeek-R1-Distill-Qwen-7B-GGUF",
     "hf_filename": "DeepSeek-R1-Distill-Qwen-7B-Q4_K_M.gguf"},
    {"model_id": "whisper-base", "model_type": "asr", "tier": "mvp",
     "hf_repo": "Systran/faster-whisper-base", "hf_filename": "(faster-whisper auto)"},
    {"model_id": "whisper-small", "model_type": "asr", "tier": "recommended",
     "hf_repo": "Systran/faster-whisper-small", "hf_filename": "(faster-whisper auto)"},
    {"model_id": "nomic-embed-text-v1.5", "model_type": "embedding", "tier": "recommended",
     "hf_repo": "nomic-ai/nomic-embed-text-v1.5-GGUF",
     "hf_filename": "nomic-embed-text-v1.5.Q8_0.gguf"},
    {"model_id": "piper-es-mx", "model_type": "tts", "tier": "recommended",
     "hf_repo": "rhasspy/piper-voices",
     "hf_filename": "es/es_MX/claude/high/es_MX-claude-high.onnx"},
]

skill = Skill()


def _list_models() -> list[dict]:
    MODELS_DIR.mkdir(parents=True, exist_ok=True)
    out = []
    for f in sorted(MODELS_DIR.rglob("*")):
        if f.is_file() and not f.name.startswith("."):
            out.append({"name": str(f.relative_to(MODELS_DIR)),
                        "size_mb": round(f.stat().st_size / 1e6, 1)})
    return out


def _search(query: str) -> list[dict]:
    from huggingface_hub import HfApi
    models = HfApi().list_models(search=query, limit=10, sort="downloads")
    return [{"hf_repo": m.id, "downloads": m.downloads or 0,
             "likes": m.likes or 0} for m in models]


def _download(repo: str, filename: str) -> str:
    from huggingface_hub import hf_hub_download
    return hf_hub_download(repo_id=repo, filename=filename, local_dir=str(MODELS_DIR))


def _delete(name: str) -> None:
    target = (MODELS_DIR / name).resolve()
    if not str(target).startswith(str(MODELS_DIR.resolve())):
        raise ValueError("path escapes the models directory")
    if not target.is_file():
        raise FileNotFoundError(f"no such model file: {name}")
    target.unlink()


@skill.on("command_in")
async def handle(ctx: Context) -> None:
    cmd = ctx.payload or {}
    action = (cmd.get("action") or "").strip()
    try:
        match action:
            case "list":
                await ctx.emit("result_out", {"action": action, "models": _list_models()})
            case "catalog":
                wanted = cmd.get("model_type")
                entries = [e for e in CATALOG
                           if not wanted or e["model_type"] == wanted]
                await ctx.emit("result_out", {"action": action, "catalog": entries})
            case "search":
                query = (cmd.get("query") or "").strip()
                if not query:
                    raise ValueError("search requires a query")
                await ctx.status("status_out", "working", f"searching Hugging Face for {query!r}...")
                results = await asyncio.to_thread(_search, query)
                await ctx.emit("result_out", {"action": action, "results": results})
            case "download":
                repo, filename = cmd.get("hf_repo"), cmd.get("hf_filename")
                if cmd.get("model_id") and not repo:
                    entry = next((e for e in CATALOG if e["model_id"] == cmd["model_id"]), None)
                    if entry is None:
                        raise ValueError(f"unknown catalog model_id: {cmd['model_id']}")
                    repo, filename = entry["hf_repo"], entry["hf_filename"]
                if not repo or not filename:
                    raise ValueError("download requires hf_repo + hf_filename (or a catalog model_id)")
                await ctx.status("status_out", "working", f"downloading {repo}/{filename}...")
                path = await asyncio.to_thread(_download, repo, filename)
                await ctx.emit("result_out", {"action": action, "path": path})
            case "delete":
                name = cmd.get("model_id") or ""
                _delete(name)
                await ctx.emit("result_out", {"action": action, "deleted": name})
            case _:
                raise ValueError(f"unknown action {action!r} (list|catalog|search|download|delete)")
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"{action or 'command'} failed: {exc}")


if __name__ == "__main__":
    skill.run()
