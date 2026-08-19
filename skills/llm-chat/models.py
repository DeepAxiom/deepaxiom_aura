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

import logging
import os
from pathlib import Path

from aura import models as active_models

log = logging.getLogger("llm-chat")

DEFAULT_REPO = "Qwen/Qwen2.5-1.5B-Instruct-GGUF"
DEFAULT_FILE = "qwen2.5-1.5b-instruct-q4_k_m.gguf"
ACTIVE_FILENAME = active_models.ACTIVE_FILENAME


def models_dir() -> Path:
    """Re-exported so this module stays the one place llm-chat asks."""
    return active_models.models_dir()


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

    # The SDK owns this read: the planner does it too, and a convention two
    # processes must agree on drifts when each keeps its own copy.
    active = active_models.active_model_path(directory)
    if active:
        return {"path": str(active), "source": "model-manager active.json"}
    if active_models.active_record(directory):
        log.warning("active.json names a model that is gone — falling through")

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
