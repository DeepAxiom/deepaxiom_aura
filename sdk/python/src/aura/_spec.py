"""Code generated from spec/enums.yaml and spec/VERSION. DO NOT EDIT.

Regenerate with: python scripts/gen_ssot.py
"""
from __future__ import annotations

VERSION = "0.3.0"

#: The skill type whose edges act on the world.
EFFECT_TYPE = "motor"

#: C1 `type` — the five kinds of skill.
SKILL_TYPES = (
    "sensorial",
    "cognitive",
    "motor",
    "memory",
    "logical",
)

#: C1 `format` — how a skill is packaged.
SKILL_FORMATS = (
    "source",
    "wasm",
    "model",
    "projection",
)

#: C3 `kind` — every envelope kind.
ENVELOPE_KINDS = (
    "data",
    "done",
    "error",
    "status",
    "register",
    "confirm_request",
    "confirm_response",
    "cancel",
    "config_update",
)

#: C3 rule 4 — per-edge delivery classes.
QOS_CLASSES = (
    "reliable",
    "realtime",
    "bulk",
)

#: C2 — what an edge may declare about approval.
GATES = (
    "human-approval",
    "none",
)

#: C4 — what a node policy may decide about an effect.
POLICY_DECISIONS = (
    "allow",
    "gate",
    "deny",
)

#: C4 — what became of an effect the ledger sealed.
EFFECT_OUTCOMES = (
    "delivered",
    "denied",
)

#: C1 `capability` — <type>.<function>[.<subtype>].
CAPABILITY_PATTERN = r"^(sensorial|cognitive|motor|memory|logical)(\.[a-z0-9_-]+)+$"
