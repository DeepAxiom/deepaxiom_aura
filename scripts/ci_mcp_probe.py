#!/usr/bin/env python3
"""A two-tool MCP server over stdio, for the CI job that checks `aura guard`.

Deliberately minimal and dependency-free: the job it supports is a claim about
how guard *classifies* tools, and a real dependency would mean the claim could
fail for reasons that have nothing to do with the classification.

One tool annotates `readOnlyHint`, the other annotates nothing. Guard must type
both as `motor` by default, and only `--trust-annotations` may move the first
to `sensorial`.
"""
from __future__ import annotations

import json
import sys

TOOLS = [
    {
        "name": "write_thing",
        "description": "Writes something. Annotates nothing at all.",
        "inputSchema": {"type": "object", "properties": {"v": {"type": "string"}}},
    },
    {
        "name": "read_thing",
        "description": "Claims to only read.",
        "inputSchema": {"type": "object", "properties": {}},
        "annotations": {"readOnlyHint": True},
    },
]


def main() -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            continue
        if "id" not in req:
            continue  # a notification expects no reply
        method = req.get("method")
        if method == "initialize":
            result = {
                "protocolVersion": "2025-06-18",
                "serverInfo": {"name": "ci-probe", "version": "1"},
            }
        elif method == "tools/list":
            result = {"tools": TOOLS}
        elif method == "tools/call":
            params = req.get("params") or {}
            result = {"content": [{"type": "text",
                                   "text": f"ran {params.get('name')}"}]}
        else:
            result = {}
        sys.stdout.write(
            json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": result}) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
