"""
connector — turn a short declarative spec into skills, one per operation.

Connecting an existing system used to need either a full OpenAPI document or a
hand-written Python skill. Neither is available for the common case: one loose
endpoint on a backend nobody documented. This reads a few lines of YAML and
registers each operation as an ordinary skill.

It is deliberately a skill, not a kernel feature. Adding connector kinds to the
kernel would grow the binary that ships to edge devices for integrations most
of them never use, and would put every new connector behind a kernel release.
As a skill it installs from the registry like anything else, and the binary
never changes.

The four safeties of a projection are all preserved:

  read-only by default   GET/HEAD become sensorial.*, everything else motor.*
  writes start disabled  and stay that way until promoted
  dry-run                returns the exact request it would have sent
  human approval         motor.* edges are gated by the kernel itself

Per-operation mode is declared C1 `config`, so `aura promote` is just a config
write: the kernel validates the value, persists it across restarts, and pushes
it to this process live. No new mechanism, and no restart to promote.

Usage:

    pip install -r skills/connector/requirements.txt
    export PYTHONPATH=sdk/python/src
    python skills/connector/main.py path/to/connector.yaml

See example.yaml for the spec format and README.md for the walkthrough.
"""
from __future__ import annotations

import json
import logging
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

import yaml

from aura import Context, Skill, run_all

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("connector")

MODES = ("disabled", "dry-run", "live")
_ENV_RE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")
_SLUG_RE = re.compile(r"[^a-z0-9-]+")


def _expand(value: str) -> str:
    """Resolve ${VAR} from the environment.

    Credentials belong in the environment, not in a spec file that ends up in
    git. A missing variable is an error rather than an empty header, because
    silently sending no credential produces a 401 that looks like the remote
    system's fault.
    """
    def sub(m: re.Match[str]) -> str:
        name = m.group(1)
        if name not in os.environ:
            raise SystemExit(
                f"connector spec references ${{{name}}} but that variable is not set"
            )
        return os.environ[name]

    return _ENV_RE.sub(sub, value)


def slug(text: str) -> str:
    return _SLUG_RE.sub("-", str(text).lower()).strip("-")


def load_spec(path: str) -> dict[str, Any]:
    spec = yaml.safe_load(Path(path).read_text(encoding="utf-8"))
    if not isinstance(spec, dict):
        raise SystemExit(f"{path}: expected a YAML mapping")
    for key in ("name", "base_url", "operations"):
        if key not in spec:
            raise SystemExit(f"{path}: missing mandatory key {key!r}")
    if not spec["operations"]:
        raise SystemExit(f"{path}: declares no operations")

    spec["name"] = slug(spec["name"])
    spec["base_url"] = _expand(str(spec["base_url"])).rstrip("/")
    spec["headers"] = {k: _expand(str(v)) for k, v in (spec.get("headers") or {}).items()}

    seen = set()
    for op in spec["operations"]:
        for key in ("op_id", "method", "path"):
            if key not in op:
                raise SystemExit(f"{path}: operation {op!r} missing {key!r}")
        op["op_id"] = slug(op["op_id"])
        if op["op_id"] in seen:
            raise SystemExit(f"{path}: duplicate op_id {op['op_id']!r}")
        seen.add(op["op_id"])
        op["method"] = str(op["method"]).upper()
        # Read/write is decided by the verb, same rule the OpenAPI projection
        # uses, unless the spec overrides it explicitly.
        if "write" not in op:
            op["write"] = op["method"] not in ("GET", "HEAD")
    return spec


def describe(spec: dict, op: dict) -> str:
    """The description the planner reads.

    The trailing `parameters:` list is a load-bearing contract: the planner
    parses it (see skills/planner/main.py `_declared_params`) to tell a real
    parameter from a key that belongs in the request body. Keep the format in
    sync with kernel/internal/projection/host.go, which emits the same shape.
    """
    summary = op.get("summary") or f"{op['method']} {op['path']}"
    desc = f"{summary} ({op['method']} {op['path']}, api \"{spec['name']}\")"
    params = [f"{p.get('in', 'query')}:{p['name']}" for p in op.get("params", [])]
    if params:
        desc += " parameters: " + ", ".join(params)
    return desc


def manifest_for(spec: dict, op: dict) -> dict[str, Any]:
    kind = "motor" if op["write"] else "sensorial"
    api = spec["name"].replace("-", "_")
    fn = op["op_id"].replace("-", "_")
    host = urllib.parse.urlparse(spec["base_url"]).netloc
    return {
        "id": f"connector/{kind}/{spec['name']}-{op['op_id']}",
        "version": "1.0.0",
        "protocol": "1",
        "name": f"{spec['name']} {op['op_id']}",
        "description": describe(spec, op),
        "capability": f"{kind}.api.{api}.{fn}",
        "type": kind,
        "format": "projection",
        "runtime": {"language": "python", "version": ">=3.11"},
        "ports": {
            "ingress": [{"name": "request_in", "schema": "std/api-request@1"}],
            "egress": [{"name": "response_out", "schema": "std/api-response@1"}],
        },
        "requirements": {"memory": "32Mi", "accelerator": "none"},
        "permissions": {
            "egress_http": [host],
            "filesystem": "none",
            "channels": "declared-only",
        },
        "config": [
            {
                "key": "mode",
                "type": "enum",
                # Writes start disabled: connecting a system must never be the
                # same action as authorising it to change that system.
                "default": "disabled" if op["write"] else "live",
                "options": list(MODES),
                "description": (
                    "disabled refuses the call; dry-run returns the exact request "
                    "it would send without sending it; live performs it."
                ),
            },
            {
                "key": "timeout_seconds",
                "type": "float",
                "default": 30.0,
                "min": 0.1,
                "max": 600.0,
                "description": "How long to wait for the remote system.",
            },
        ],
        "signature": None,
    }


def build_url(spec: dict, op: dict, req: dict) -> str:
    path = op["path"]
    for key, value in (req.get("params") or {}).items():
        path = path.replace("{" + str(key) + "}", urllib.parse.quote(str(value), safe=""))
    url = spec["base_url"] + path
    query = req.get("query") or {}
    if query:
        url += ("&" if "?" in url else "?") + urllib.parse.urlencode(query, doseq=True)
    return url


def call(spec: dict, op: dict, req: dict, mode: str, timeout: float) -> dict[str, Any]:
    """Perform (or refuse, or rehearse) one operation. Returns std/api-response@1."""
    if mode == "disabled":
        return {"ok": False, "status": 0,
                "error": f"operation {op['op_id']!r} is disabled — promote it first "
                         f"(set its 'mode' config to dry-run or live)"}

    url = build_url(spec, op, req)
    body = req.get("body")
    data = json.dumps(body).encode() if body is not None else None

    if mode == "dry-run":
        return {"ok": True, "status": 0, "dry_run": True,
                "body": {"would_send": {"method": op["method"], "url": url, "body": body}}}

    headers = {**spec["headers"]}
    if data is not None:
        headers.setdefault("Content-Type", "application/json")
    # Caller-supplied headers come last, but must not override configured
    # credentials — a graph should not be able to strip the connector's auth.
    for key, value in (req.get("headers") or {}).items():
        if key.lower() not in {h.lower() for h in spec["headers"]}:
            headers[key] = str(value)

    request = urllib.request.Request(url, data=data, method=op["method"], headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as resp:
            raw = resp.read(4 << 20)  # cap the response like the projection host
            return {"ok": 200 <= resp.status < 400, "status": resp.status,
                    "body": _decode(raw)}
    except urllib.error.HTTPError as exc:
        return {"ok": False, "status": exc.code, "body": _decode(exc.read(4 << 20)),
                "error": exc.reason}
    except Exception as exc:  # noqa: BLE001
        return {"ok": False, "status": 0, "error": f"{type(exc).__name__}: {exc}"}


def _decode(raw: bytes) -> Any:
    if not raw:
        return None
    try:
        return json.loads(raw)
    except ValueError:
        return raw.decode("utf-8", errors="replace")


def build(spec: dict, op: dict) -> Skill:
    skill = Skill(manifest=manifest_for(spec, op))

    @skill.on("request_in")
    async def handle(ctx: Context, _op=op, _s=skill) -> None:
        import asyncio
        payload = ctx.payload if isinstance(ctx.payload, dict) else {}
        # Read the mode per call, never cache it: the kernel pushes config
        # changes live, which is what makes promotion take effect without a
        # restart.
        result = await asyncio.to_thread(
            call, spec, _op, payload,
            _s.config["mode"], float(_s.config["timeout_seconds"]),
        )
        await ctx.emit("response_out", result)

    return skill


def main() -> None:
    path = (sys.argv[1] if len(sys.argv) > 1
            else os.environ.get("AURA_CONNECTOR_SPEC", "connector.yaml"))
    if not Path(path).exists():
        raise SystemExit(
            f"{path} not found — pass a connector spec "
            f"(see skills/connector/example.yaml)"
        )
    spec = load_spec(path)
    skills = [build(spec, op) for op in spec["operations"]]

    log.info("connector %r -> %d operation(s) against %s",
             spec["name"], len(skills), spec["base_url"])
    for s in skills:
        log.info("  %-45s mode=%s", s.manifest["capability"], s.config["mode"])
    run_all(skills)


if __name__ == "__main__":
    main()
