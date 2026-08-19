"""
catalog.py — planner's view of the live skill catalog.

Fetches the running catalog from the kernel (`GET /v1/skills`) and formats
it into the compact lines the LLM prompt embeds. Kept separate from
`main.py` so both are testable without a live kernel connection.

`fetch_catalog` never lets a raw `urllib` exception escape: a kernel that is
unreachable (timeout, connection refused, DNS failure, ...) is an expected
runtime condition here, not a bug, so it is turned into the typed
`CatalogUnavailable` that `main.py`'s handler catches and reports through
`ctx.error(...)` instead of crashing the dispatch loop.
"""
import json
import logging
import os
import urllib.error
import urllib.request

from aura.skill import resolve_token

log = logging.getLogger("planner")

KERNEL_HTTP = os.getenv("AURA_HTTP_URL", "http://localhost:9080")


class CatalogUnavailable(Exception):
    """The kernel's live skill catalog could not be fetched."""


def fetch_catalog() -> list[dict]:
    # `/v1/skills` is behind the node's bearer token like every other route, so
    # this has to carry one. It did not, and the failure was invisible from
    # here: `aura do` reported "could not reach kernel catalog: HTTP Error 401"
    # against a node that was running perfectly well on its own machine.
    #
    # The token comes from the SDK's resolver — the same two places the CLI
    # reads — rather than a fourth copy of that logic.
    request = urllib.request.Request(f"{KERNEL_HTTP}/v1/skills")
    token = resolve_token()
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=10) as r:
            skills = json.load(r)
    except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        # URLError covers unreachable/refused/DNS; OSError catches raw socket
        # errors that don't get wrapped; ValueError catches a malformed (non
        # JSON) body. All of them mean the same thing to a caller: no catalog
        # right now.
        log.warning("failed to fetch skill catalog from %s: %s", KERNEL_HTTP, exc)
        raise CatalogUnavailable(f"could not reach kernel catalog: {exc}") from exc
    return [s for s in skills
            if s["capability"] not in ("cognitive.planner",)
            and s["ports"].get("ingress")]


def catalog_lines(catalog: list[dict]) -> str:
    """Format the catalog for embedding into the planning prompt."""
    lines = []
    for s in catalog:
        ingress = s["ports"]["ingress"][0]
        lines.append(f"- {s['capability']} :: {s['description']} :: {ingress['schema']}")
    return "\n".join(lines)
