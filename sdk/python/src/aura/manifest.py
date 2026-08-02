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


def load_manifest(path: str = "skill.yaml") -> dict[str, Any]:
    p = Path(path)
    if not p.exists():
        raise FileNotFoundError(
            f"{path} not found — every skill needs a C1 manifest next to its code"
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
