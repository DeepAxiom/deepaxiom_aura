"""
model-manager download — the two calls into the Hugging Face Hub API.

Both are blocking network calls (`huggingface_hub` uses `requests`
underneath), so callers in main.py run them via `asyncio.to_thread`. The
`huggingface_hub` import stays lazy inside each function, same pattern as
the rest of this repo (e.g. skills/asr's lazy `faster_whisper` import), so
importing this module never requires the dependency to be installed.
"""
from pathlib import Path


def search_models(query: str) -> list[dict]:
    from huggingface_hub import HfApi
    models = HfApi().list_models(search=query, limit=10, sort="downloads")
    return [{"hf_repo": m.id, "downloads": m.downloads or 0,
             "likes": m.likes or 0} for m in models]


def download_model(models_dir: Path, repo: str, filename: str) -> str:
    from huggingface_hub import hf_hub_download
    return hf_hub_download(repo_id=repo, filename=filename, local_dir=str(models_dir))
