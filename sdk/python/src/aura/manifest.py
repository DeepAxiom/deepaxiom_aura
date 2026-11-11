"""C1 manifest loading and client-side validation."""
from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import yaml

from ._spec import SKILL_TYPES  # generated from spec/enums.yaml

_ID_RE = re.compile(r"^[a-z0-9-]+/[a-z0-9-]+/[a-z0-9-]+$")
_SEMVER_RE = re.compile(r"^\d+\.\d+\.\d+$")
_SCHEMA_RE = re.compile(r"^[a-z0-9-]+/[a-z0-9_-]+@\d+$")


# The C1 manifest file, under both of its names. What this runtime runs was
# called a skill when the file was named; "skill" has since come to mean a
# prompt handed to a language model, which these are not, so the file is now
# `expertise.yaml`. The old name keeps working: a package written before the
# rename loads unchanged, and the contents of the file did not change at all.
MANIFEST_FILE = "expertise.yaml"
LEGACY_MANIFEST_FILE = "skill.yaml"


def default_manifest_path(directory: str | Path = ".") -> str:
    """The manifest in `directory`: `expertise.yaml`, else `skill.yaml`.

    Returns the current name when neither exists, so the error a caller then
    gets names the file they should create rather than the one being retired.
    """
    base = Path(directory)
    for name in (MANIFEST_FILE, LEGACY_MANIFEST_FILE):
        if (base / name).exists():
            return str(base / name) if str(base) != "." else name
    return str(base / MANIFEST_FILE) if str(base) != "." else MANIFEST_FILE


def load_manifest(path: str | None = None) -> dict[str, Any]:
    if path is None:
        path = default_manifest_path()
    p = Path(path)
    if not p.exists():
        raise FileNotFoundError(
            f"{path} not found — every Expertise needs a C1 manifest next to its code "
            f"({MANIFEST_FILE}; {LEGACY_MANIFEST_FILE} is still read)"
        )
    manifest = yaml.safe_load(p.read_text(encoding="utf-8"))
    validate_manifest(manifest)
    return manifest


def validate_manifest(m: dict[str, Any]) -> None:
    for key in ("id", "version", "protocol", "name", "description",
                "capability", "type", "format", "ports"):
        if key not in m:
            raise ValueError(f"manifest missing mandatory field {key!r}")
    if not _ID_RE.match(m["id"]):
        raise ValueError(f"invalid id {m['id']!r} (want org/category/name)")
    if not _SEMVER_RE.match(str(m["version"])):
        raise ValueError(f"invalid version {m['version']!r} (want semver)")
    if m["type"] not in SKILL_TYPES:
        raise ValueError(f"invalid type {m['type']!r} (want one of {SKILL_TYPES})")
    if not str(m["capability"]).startswith(m["type"] + "."):
        raise ValueError(
            f"capability {m['capability']!r} must start with type {m['type']!r}"
        )
    ports = m["ports"]
    if not ports.get("ingress") and not ports.get("egress"):
        raise ValueError("skill declares no ports")
    for direction in ("ingress", "egress"):
        for port in ports.get(direction, []):
            if "schema" not in port:
                raise ValueError(
                    f"port {port.get('name')!r}: schema is mandatory (C1 rule 2)"
                )
            if not _SCHEMA_RE.match(port["schema"]):
                raise ValueError(
                    f"port {port.get('name')!r}: schema must be ns/name@major, "
                    f"got {port['schema']!r}"
                )
    for param in m.get("config", []):
        for key in ("key", "type", "default"):
            if key not in param:
                raise ValueError(f"config param {param!r} missing {key!r}")
        if param["type"] not in ("string", "int", "float", "bool", "enum"):
            raise ValueError(
                f"config param {param['key']!r}: invalid type {param['type']!r}"
            )
        if param["type"] == "enum" and not param.get("options"):
            raise ValueError(
                f"config param {param['key']!r}: type enum requires 'options'"
            )
