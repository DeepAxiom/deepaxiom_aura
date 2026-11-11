"""
model-manager storage — local filesystem operations against models_dir.

`delete_model` resolves the requested path and checks it still lives under
`models_dir` before unlinking anything — the only thing standing between a
`model_id` like "../../.ssh/id_rsa" and a delete outside the models
directory. Keep this check exactly as-is if this module changes again.
"""
from pathlib import Path


def list_models(models_dir: Path) -> list[dict]:
    models_dir.mkdir(parents=True, exist_ok=True)
    out = []
    for f in sorted(models_dir.rglob("*")):
        if f.is_file() and not f.name.startswith("."):
            out.append({"name": str(f.relative_to(models_dir)),
                        "size_mb": round(f.stat().st_size / 1e6, 1)})
    return out


def delete_model(models_dir: Path, name: str) -> None:
    target = (models_dir / name).resolve()
    if not str(target).startswith(str(models_dir.resolve())):
        raise ValueError("path escapes the models directory")
    if not target.is_file():
        raise FileNotFoundError(f"no such model file: {name}")
    target.unlink()
