"""Naming the delivery a piece of work is being done for.

An Expertise that calls a model is doing so *for* something: the delivery its
handler was invoked with. When inference goes through the node rather than
straight to an engine, saying which delivery that is lets the node do three
things it could not do before — count the request against the right graph,
give it the priority and the deadline that delivery is actually under, and
close it at the engine the moment the chain is cancelled.

Nothing here is a credential. The four header values are a *reference*: the
node looks the delivery up in its own record and decides what it means. A
caller that names a delivery made to someone else is refused, and one that
names none is simply served as unattributed work.

Config (env):
  AURA_INFERENCE_URL=node   call models through this Expertise's own node.
                            The address is derived from AURA_WS_URL, so there
                            is one setting for "where is my node" rather than
                            two that can disagree.
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from urllib.parse import urlsplit, urlunsplit

#: The value of AURA_INFERENCE_URL that means "through my node".
NODE = "node"

#: Where a node serves its OpenAI-compatible routes.
INFERENCE_PATH = "/v1/inference"

_DEFAULT_WS_URL = "ws://localhost:9080/ws/skill"


@dataclass(frozen=True)
class DeliveryRef:
    """The delivery a handler is answering: enough for the node to find it.

    Get one from `ctx.delivery`. It is immutable and holds no connection, so it
    is safe to hand to a worker thread — which is where model calls run.
    """

    session: str
    id: str
    node: str = ""
    port: str = ""

    def headers(self) -> dict[str, str]:
        """The reference as request headers, for a call to the node's
        inference routes. Empty values are left out."""
        named = {
            "X-Aura-Session": self.session,
            "X-Aura-Delivery": self.id,
            "X-Aura-Node": self.node,
            "X-Aura-Port": self.port,
        }
        return {name: value for name, value in named.items() if value}


def via_node() -> bool:
    """Whether this process is configured to call models through its node."""
    return os.getenv("AURA_INFERENCE_URL", "").strip().lower() == NODE


def node_inference_url(ws_url: str | None = None) -> str:
    """The node's inference base URL, derived from where its socket is.

    `ws://host:9080/ws/skill` becomes `http://host:9080/v1/inference`, and
    `wss://` becomes `https://`. Derived rather than configured: an Expertise
    already has to know where its node is to connect at all, and a second
    variable for the same host is a second thing to get wrong.
    """
    parts = urlsplit((ws_url or os.getenv("AURA_WS_URL", "") or _DEFAULT_WS_URL).strip())
    scheme = {"ws": "http", "wss": "https"}.get(parts.scheme, parts.scheme or "http")
    return urlunsplit((scheme, parts.netloc, INFERENCE_PATH, "", ""))


def node_headers(delivery: DeliveryRef | None = None) -> dict[str, str]:
    """Headers for a request to the node's inference routes: this process's
    own node credential, and the delivery the request is for when there is one.

    Only ever call this for a request that is going *to the node*. The
    credential it attaches is the one this process registered with; sent to
    anything else it would be a leaked token.
    """
    # Imported here: `skill` imports this module for `Context.delivery`.
    from .skill import resolve_token

    headers: dict[str, str] = {}
    token = resolve_token()
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if delivery is not None:
        headers.update(delivery.headers())
    return headers
