"""
Test Expertise used to produce the captures in docs/media.

Three typed functions served from one process, so a node with no model, no
API key and no network still produces every kind of traffic the control plane
draws: a token stream with reported usage, a pass-through transform, and a
`motor.*` effect that the kernel gates and seals.

Nothing here is a model. `draft` replays canned sentences one token at a time
and reports token counts in `attest.usage`, which is exactly what a real
engine adapter does (see skills/llm-chat). The numbers on screen are therefore
real counters of this fake stream, not a simulation drawn by the UI.

    PYTHONPATH=sdk/python/src AURA_WS_URL=ws://localhost:9480/ws/skill \
        python docs/media/src/demo_expertise.py [--replicas N] [--coalesce]

  --replicas N   connect N copies of `draft` (shows per-replica load)
  --coalesce     wrap the token stream in aura.stream.DeltaCoalescer
  --tps F        tokens per second for `draft` (default 28)
"""
from __future__ import annotations

import argparse
import asyncio
import hashlib
import re
import tempfile
from pathlib import Path

from aura import Context, Skill
from aura.attest import Attestation
from aura.skill import run_all
from aura.stream import DeltaCoalescer

REPLIES = [
    "A refund over the limit landed on order 48213. The customer paid by card "
    "ending 4417 and asked for the money back two days after delivery. I drafted "
    "a notice for the finance channel and left the amount, the order and the "
    "reason in it so nobody has to open the ticket to decide.",
    "The nightly sync finished with three rows rejected. All three carry a "
    "postcode the address service does not know, so I held them back instead of "
    "writing a guess. The rest of the batch is committed and the three are "
    "listed below with the value each one failed on.",
    "Camera 2 lost its stream for eleven seconds at 14:02 and recovered on its "
    "own. No frames were dropped on the other lanes while it was down. I would "
    "not page anyone for this, but it is the second gap today from the same "
    "device and that is worth a line in the handover.",
]

BASE = {
    "version": "0.1.0",
    "protocol": "1",
    "format": "source",
    "runtime": {"language": "python", "version": ">=3.11"},
    "requirements": {"memory": "32Mi", "accelerator": "none"},
    "signature": None,
}


def manifest(**over: object) -> dict:
    out = dict(BASE)
    out.update(over)
    return out


def tokens(text: str) -> list[str]:
    """Split the way a tokenizer roughly does: word pieces with their space."""
    return re.findall(r"\s*\S{1,4}", text)


def build_draft(tps: float, coalesce: bool) -> Skill:
    skill = Skill(manifest=manifest(
        id="demo/cognitive/draft",
        name="Draft (test stream)",
        description="Test Expertise. Replays canned text one token at a time and "
                    "reports input, output and cached token counts in attest.usage. "
                    "Not a model.",
        capability="cognitive.demo.draft",
        type="cognitive",
        ports={
            "ingress": [{"name": "text_in", "schema": "std/text@1"}],
            "egress": [
                {"name": "text_out", "schema": "std/text@1"},
                {"name": "full_out", "schema": "std/text@1"},
            ],
        },
        permissions={"egress_http": [], "filesystem": "none", "channels": "declared-only"},
        config=[
            {"key": "tokens_per_second", "type": "float", "default": tps, "min": 1, "max": 400,
             "description": "How fast the canned reply is replayed."},
            {"key": "coalesce_ms", "type": "int", "default": 25 if coalesce else 0, "min": 0, "max": 500,
             "description": "DeltaCoalescer window. 0 emits one envelope per token."},
        ],
    ))
    turn = {"n": 0}

    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        prompt = str((ctx.payload or {}).get("text", ""))
        reply = REPLIES[turn["n"] % len(REPLIES)]
        turn["n"] += 1
        pieces = tokens(reply)
        delay = 1.0 / float(skill.config["tokens_per_second"])
        window = int(skill.config["coalesce_ms"]) / 1000.0

        async def send(text: str) -> None:
            await ctx.emit("text_out", {"text": text, "final": False})

        out = DeltaCoalescer(send, max_delay=window)
        for piece in pieces:
            if ctx.cancelled:
                await out.discard()
                return
            await out.add(piece)
            await asyncio.sleep(delay)
        await out.close()

        # A fixed "system prompt" of 180 tokens precedes every turn; from the
        # second turn of a process on it is reported as served from the prefix
        # cache, which is how an engine with a KV cache reports it.
        system = 180
        asked = max(1, len(tokens(prompt)))
        usage = {
            "input_tokens": system + asked,
            "output_tokens": len(pieces),
            "cached_input_tokens": system if turn["n"] > 1 else 0,
        }
        attest = Attestation(
            engine="demo-replay", model="canned-text", engine_version="0.1.0",
            params={"tokens_per_second": float(skill.config["tokens_per_second"])},
            output_sha256="sha256:" + hashlib.sha256(reply.encode()).hexdigest(),
            usage=usage,
        )
        await ctx.emit("text_out", {"text": "", "final": True}, attest=attest)
        await ctx.emit("full_out", {"text": reply, "final": True})

    return skill


def build_redact() -> Skill:
    skill = Skill(manifest=manifest(
        id="demo/logical/redact",
        name="Redact digits (test)",
        description="Test Expertise. Masks every run of four or more digits. "
                    "Pure function of its input.",
        capability="logical.demo.redact",
        type="logical",
        ports={
            "ingress": [{"name": "text_in", "schema": "std/text@1"}],
            "egress": [{"name": "text_out", "schema": "std/text@1"}],
        },
        permissions={"egress_http": [], "filesystem": "none", "channels": "declared-only"},
        config=[{"key": "mask", "type": "string", "default": "####",
                 "description": "What a digit run is replaced with."}],
    ))

    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        text = str((ctx.payload or {}).get("text", ""))
        masked = re.sub(r"\d{4,}", str(skill.config["mask"]), text)
        await ctx.emit("text_out", {"text": masked, "final": True})

    return skill


def build_notify() -> Skill:
    outbox = Path(tempfile.gettempdir()) / "aura-demo-outbox.log"
    skill = Skill(manifest=manifest(
        id="demo/motor/notify",
        name="Notify (test effect)",
        description="Test Expertise. Appends the text it receives to a local outbox "
                    "file. It acts on the world, so the kernel holds every delivery "
                    "for a human and seals it in the ledger.",
        capability="motor.demo.notify",
        type="motor",
        ports={
            "ingress": [{"name": "text_in", "schema": "std/text@1"}],
            "egress": [{"name": "text_out", "schema": "std/text@1"}],
        },
        permissions={"egress_http": [], "filesystem": "scoped", "channels": "declared-only"},
    ))

    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        text = str((ctx.payload or {}).get("text", ""))
        with outbox.open("a", encoding="utf-8") as fh:
            fh.write(text.replace("\n", " ") + "\n")
        await ctx.emit("text_out", {"text": f"\n\nnotice written ({len(text)} chars)", "final": True})

    return skill


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--replicas", type=int, default=1)
    ap.add_argument("--coalesce", action="store_true")
    ap.add_argument("--tps", type=float, default=28.0)
    args = ap.parse_args()
    skills = [build_draft(args.tps, args.coalesce) for _ in range(max(1, args.replicas))]
    skills += [build_redact(), build_notify()]
    run_all(skills)


if __name__ == "__main__":
    main()
