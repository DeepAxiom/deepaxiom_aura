"""C3 envelope — the unit of everything that flows through the system."""
from __future__ import annotations

import os
import time
from dataclasses import dataclass, field
from typing import Any

PROTOCOL_MAJOR = "1"

KIND_DATA = "data"
KIND_DONE = "done"
KIND_ERROR = "error"
KIND_STATUS = "status"
KIND_REGISTER = "register"
KIND_CONFIRM_REQUEST = "confirm_request"
KIND_CONFIRM_RESPONSE = "confirm_response"
KIND_CANCEL = "cancel"
KIND_CONFIG_UPDATE = "config_update"

_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def new_id() -> str:
    """Lexicographically sortable unique id (ULID-like)."""
    now = int(time.time() * 1000)
    ts = []
    for _ in range(10):
        ts.append(_ALPHABET[now & 31])
        now >>= 5
    rnd = [_ALPHABET[b & 31] for b in os.urandom(16)]
    return "".join(reversed(ts)) + "".join(rnd)


@dataclass
class Envelope:
    kind: str
    id: str = field(default_factory=new_id)
    v: str = PROTOCOL_MAJOR
    cause_id: str = ""
    session: str = ""
    node: str = ""
    port: str = ""
    seq: int = 0
    idem: str = ""
    schema: str = ""
    payload: Any = None
    # C4, additive: set only on an envelope the kernel delivered into a motor
    # skill — the ledger entry hash that sealed that effect. A skill has no
    # reason to set this itself; it only ever reads one the kernel attached.
    receipt: str = ""
    # C5, additive: what this skill asserts about how it produced the payload —
    # engine, model, revision, quantization, sampling parameters, seed. The
    # kernel content-addresses it and binds the hash into every effect this
    # output causally leads to, so "on what basis did this happen" becomes
    # answerable after the fact. See aura.attest.Attestation.
    #
    # It rides beside the payload rather than inside it because it is metadata
    # about the payload's production: a consumer validating against the port's
    # schema must not have to know it exists.
    attest: Any = None

    def to_wire(self) -> dict:
        wire: dict[str, Any] = {"v": self.v, "id": self.id, "kind": self.kind}
        for key in ("cause_id", "session", "node", "port", "idem", "schema", "receipt"):
            value = getattr(self, key)
            if value:
                wire[key] = value
        if self.seq:
            wire["seq"] = self.seq
        if self.payload is not None:
            wire["payload"] = self.payload
        if self.attest is not None:
            wire["attest"] = self.attest
        return wire

    @classmethod
    def from_wire(cls, wire: dict) -> "Envelope":
        return cls(
            kind=wire.get("kind", ""),
            id=wire.get("id", ""),
            v=wire.get("v", PROTOCOL_MAJOR),
            cause_id=wire.get("cause_id", ""),
            session=wire.get("session", ""),
            node=wire.get("node", ""),
            port=wire.get("port", ""),
            seq=wire.get("seq", 0),
            idem=wire.get("idem", ""),
            schema=wire.get("schema", ""),
            payload=wire.get("payload"),
            receipt=wire.get("receipt", ""),
            attest=wire.get("attest"),
        )
