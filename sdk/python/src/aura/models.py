"""Where a skill finds the model the operator chose.

`model-manager` marks one downloaded file as active by writing
``~/.aura/models/active.json``. More than one skill needs to read that — the
chat skill loads it, the planner reasons with it — and a convention two
processes must agree on is exactly the kind of thing that drifts when each
keeps its own copy of how to read it.

It lives here rather than in a skill for the same reason `aura.llm.ChatBackend`
does: the SDK already owns the helpers for skills that load models. It is not a
frozen contract — C1 through C5 say nothing about it — so it is documented as a
convention and may change with the SDK's own version.

The writer stays in ``skills/model-manager/active.py``: one thing chooses, many
things read.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

ACTIVE_FILENAME = "active.json"


def models_dir() -> Path:
    """Where models live. ``AURA_MODELS_DIR`` overrides the default."""
    return Path(os.path.expanduser(os.getenv("AURA_MODELS_DIR", "~/.aura/models")))


def active_record(directory: Path | None = None) -> dict | None:
    """The active record, or None when there is none or it cannot be read.

    Unreadable is deliberately not an error. A corrupt pointer should degrade
    to "nothing is chosen" — which every caller already handles, because that
    is also the state of a fresh node — rather than take down a skill during
    its import.
    """
    directory = directory or models_dir()
    try:
        raw = (directory / ACTIVE_FILENAME).read_text(encoding="utf-8")
    except OSError:
        return None
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return None
    if not isinstance(data, dict):
        return None
    return data if isinstance(data.get("model"), str) and data["model"] else None


def active_model_path(directory: Path | None = None) -> Path | None:
    """The active model as a path, only if the file is actually there.

    The existence check is the point: a record naming a model that was deleted
    must not be handed to a loader, whose error would be about something else
    entirely. Callers fall through to their own default instead.
    """
    directory = directory or models_dir()
    record = active_record(directory)
    if not record:
        return None
    candidate = directory / record["model"]
    return candidate if candidate.is_file() else None
