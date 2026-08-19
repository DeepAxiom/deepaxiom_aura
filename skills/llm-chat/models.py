"""Which model this skill loads, and where that answer comes from.

Before this, the answer was a constant: `llm-chat` downloaded Qwen2.5-1.5B from
Hugging Face on first run and used it forever. A model fetched through
`model-manager` sat in `~/.aura/models` and was never looked at, so "download a
model in the UI" and "use a model" were unrelated acts.

Resolution order, most explicit first:

1. ``AURA_MODEL_PATH`` — an absolute path. An operator pointing at a specific
   file means it, and nothing below should second-guess that.
2. ``model`` in the skill's C1 config — a filename inside the models directory.
   Settable live from the control plane.
3. ``active.json`` in the models directory — what `model-manager` last marked
   as active. This is the join; see skills/model-manager/active.py for the
   writer and the record's shape.
4. The built-in Hugging Face default, downloaded on demand. Unchanged, so a
   node with no models directory behaves exactly as it did before.

Each step is skipped rather than fatal when what it names is absent: a config
pointing at a deleted file should fall through to the next answer, not leave
the node without a chat skill.
"""
from __future__ import annotations

import json
import logging
import os
from pathlib import Path

log = logging.getLogger("llm-chat")

DEFAULT_REPO = "Qwen/Qwen2.5-1.5B-Instruct-GGUF"
DEFAULT_FILE = "qwen2.5-1.5b-instruct-q4_k_m.gguf"
ACTIVE_FILENAME = "active.json"


def models_dir() -> Path:
    return Path(os.path.expanduser(os.getenv("AURA_MODELS_DIR", "~/.aura/models")))


def _active_filename(directory: Path) -> str | None:
    """The filename model-manager last marked active, if any."""
    try:
        data = json.loads((directory / ACTIVE_FILENAME).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    name = data.get("model") if isinstance(data, dict) else None
    return name if isinstance(name, str) and name else None


def resolve(config_model: str = "") -> dict:
    """Where to load the model from, and why — the reason is for the log.

    Returns either ``{"path": ...}`` for a local file or
    ``{"repo": ..., "file": ...}`` for one to fetch, plus ``source``.
    """
    explicit = os.getenv("AURA_MODEL_PATH", "").strip()
    if explicit:
        return {"path": explicit, "source": "AURA_MODEL_PATH"}

    directory = models_dir()

    chosen = (config_model or "").strip()
    if chosen:
        candidate = directory / chosen
        if candidate.is_file():
            return {"path": str(candidate), "source": "skill config `model`"}
        log.warning("config names %r but it is not in %s — falling through", chosen, directory)

    active = _active_filename(directory)
    if active:
        candidate = directory / active
        if candidate.is_file():
            return {"path": str(candidate), "source": "model-manager active.json"}
        log.warning("active.json names %r but it is gone — falling through", active)

    return {
        "repo": os.getenv("AURA_MODEL_REPO", DEFAULT_REPO),
        "file": os.getenv("AURA_MODEL_FILE", DEFAULT_FILE),
        "source": "built-in default",
    }


def installed(directory: Path | None = None) -> list[str]:
    """Every file in the models directory — what `model` may be set to."""
    directory = directory or models_dir()
    try:
        return sorted(
            str(f.relative_to(directory))
            for f in directory.rglob("*")
            if f.is_file() and not f.name.startswith(".") and f.name != ACTIVE_FILENAME
        )
    except OSError:
        return []
