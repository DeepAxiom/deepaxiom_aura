"""The active model: one downloaded file, named as the one to use.

Downloading a model and using it were two disconnected acts. `model-manager`
put files in `~/.aura/models`; `llm-chat` independently downloaded a hardcoded
default from Hugging Face and never looked at that directory. You could fetch
a model through the UI and it would change nothing.

This is the join, and it is deliberately a file rather than a message: the two
skills are separate processes with separate lifetimes, either can restart, and
a choice that survives both has to be written down. `~/.aura/models/active.json`

    {"model": "gemma-3-4B-it-Q4_K_M.gguf",
     "model_id": "gemma-3-4b-it",
     "set_at": 1755600000}

`model` is the filename inside the models directory — not an absolute path, so
the record stays valid if the directory moves. `model_id` is the catalogue key
when it came from the catalogue, absent for an arbitrary Hugging Face file.

Read by skills/llm-chat/models.py. The two are separate processes, so nothing
enforces the shape at runtime; the test in each asserts it against the other.
"""
from __future__ import annotations

import json
import time
from pathlib import Path

FILENAME = "active.json"


def read_active(models_dir: Path) -> dict | None:
    """The active record, or None when there is none or it is unreadable.

    Unreadable is deliberately not an error: a corrupt pointer to a model
    should degrade to "no model chosen" — which every reader already handles —
    rather than take down a skill at startup.
    """
    try:
        raw = (models_dir / FILENAME).read_text(encoding="utf-8")
    except OSError:
        return None
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return None
    return data if isinstance(data, dict) and data.get("model") else None


def set_active(models_dir: Path, filename: str, model_id: str | None = None) -> dict:
    """Record `filename` as the model to use, if it is actually there.

    The existence check is the point. A pointer to a file that was never
    downloaded moves the failure from here — where the message can say which
    file and which directory — to a model loader whose error will be about
    something else entirely.
    """
    target = (models_dir / filename).resolve()
    # The same containment check delete_model makes, for the same reason: a
    # filename is attacker-supplied input from anything that can send a command.
    if not str(target).startswith(str(models_dir.resolve())):
        raise ValueError("path escapes the models directory")
    if not target.is_file():
        raise ValueError(f"{filename!r} is not in {models_dir} — download it first")

    record = {"model": filename, "set_at": int(time.time())}
    if model_id:
        record["model_id"] = model_id

    models_dir.mkdir(parents=True, exist_ok=True)
    (models_dir / FILENAME).write_text(
        json.dumps(record, indent=2) + "\n", encoding="utf-8")
    return record


def clear_active(models_dir: Path) -> None:
    """Forget the choice. Called when the active model is the one deleted."""
    try:
        (models_dir / FILENAME).unlink()
    except OSError:
        pass
