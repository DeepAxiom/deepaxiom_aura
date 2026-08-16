"""C5 — inference attestation: what a skill asserts about how it produced an output.

A skill that runs a model can say so, in a form the kernel binds to every
effect that output leads to:

    from aura import Attestation, Context, Skill

    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        reply = run_the_model(ctx.payload["text"])
        await ctx.emit(
            "text_out",
            {"text": reply, "final": True},
            attest=Attestation(
                engine="llama.cpp",
                model="Qwen/Qwen2.5-1.5B-Instruct-GGUF",
                model_revision="f1d2d2f924e986ac86fdf7b36c94bcdf32beec15",
                quantization="Q4_K_M",
                params={"temperature": 0.7, "seed": 42},
            ),
        )

The kernel hashes the record, stores it once, and cites the hash in the ledger
entry of any `motor.*` effect downstream. An auditor reading that entry later
can therefore ask not only "who authorized this" but "what argued for it".

## What this proves

The honest version, because the overclaim is tempting and would be caught:
**an attestation is an assertion, cryptographically bound to what it caused
and to when it was made — not proof that the assertion is true.** A skill that
lies about its model produces an unforgeable record of a lie. What the kernel
guarantees is that the claim cannot be altered afterwards, cannot be detached
from the effects it led to, and cannot be backdated. Making the claim itself
trustworthy needs hardware attestation, which the `tee` field is reserved for.

Attesting is optional and additive. A skill that never calls it behaves
exactly as it did before C5 existed.
"""
from __future__ import annotations

import hashlib
import json
import logging
import os
import shutil
import subprocess
import time
from dataclasses import dataclass, field
from typing import Any

from ._spec import ENERGY_SOURCES  # generated from spec/enums.yaml

_logger = logging.getLogger("aura.attest")

# Mirrors ledger.MaxAttestationBytes. Attestations carry model identity and
# sampling parameters — never prompts, never outputs.
MAX_ATTESTATION_BYTES = 8 * 1024


@dataclass
class Energy:
    """The energy one inference cost, and how that number came to exist.

    `source` is mandatory and load-bearing. A measured joule and a modelled
    joule differ by an order of magnitude in trustworthiness, and a field
    carrying only the number would launder an estimate into a measurement the
    first time it reached a sustainability report. Consumers that only accept
    hardware counters can filter; consumers that accept estimates know what
    they are accepting.
    """

    millijoules: float
    source: str
    basis: str = ""

    def to_wire(self) -> dict:
        if self.source not in ENERGY_SOURCES:
            raise ValueError(
                f"energy source {self.source!r} must be one of {ENERGY_SOURCES}")
        out: dict[str, Any] = {
            "millijoules": round(float(self.millijoules), 3),
            "source": self.source,
        }
        if self.basis:
            out["basis"] = self.basis
        return out


@dataclass
class Attestation:
    """What a skill claims about the inference behind an output."""

    engine: str
    model: str
    engine_version: str = ""
    model_revision: str = ""
    model_file: str = ""
    model_sha256: str = ""
    quantization: str = ""
    params: dict[str, Any] = field(default_factory=dict)
    prompt_sha256: str = ""
    output_sha256: str = ""
    energy: Energy | None = None
    tee: Any = None

    def to_wire(self) -> dict:
        if not self.engine:
            raise ValueError("an attestation must name the engine that ran the inference")
        if not self.model:
            raise ValueError("an attestation must name the model")
        wire: dict[str, Any] = {"engine": self.engine, "model": self.model}
        for key in (
            "engine_version", "model_revision", "model_file", "model_sha256",
            "quantization", "prompt_sha256", "output_sha256",
        ):
            value = getattr(self, key)
            if value:
                wire[key] = value
        if self.params:
            wire["params"] = self.params
        if self.energy is not None:
            wire["energy"] = self.energy.to_wire()
        if self.tee is not None:
            wire["tee"] = self.tee

        encoded = json.dumps(wire, separators=(",", ":"), sort_keys=True)
        if len(encoded.encode("utf-8")) > MAX_ATTESTATION_BYTES:
            raise ValueError(
                f"attestation is over the {MAX_ATTESTATION_BYTES}-byte cap — it carries "
                "model identity and sampling parameters, never prompts or outputs")
        return wire


def sha256_text(text: str) -> str:
    """Hash a prompt or an output for `prompt_sha256` / `output_sha256`.

    Binds an attestation to the input it was made about without storing that
    input — the same reason the ledger keeps `payload_sha256` and never the
    payload. A prompt is user data and must not become permanently
    undeletable.
    """
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def sha256_file(path: str, *, max_bytes: int = 0) -> str:
    """Hash a weights file, so `model_sha256` pins the actual artifact.

    `max_bytes` caps how much is read. Hashing a 40 GB GGUF on every startup
    is not free, and a caller that would rather pin the first N bytes than pin
    nothing can say so — the field is a fingerprint, and a partial one that
    exists beats a complete one nobody computes. When capped, the caller
    should say so in the model_file name or skip the field; a truncated hash
    presented as a whole-file hash would be a quiet lie.
    """
    h = hashlib.sha256()
    read = 0
    with open(path, "rb") as fh:
        while True:
            chunk = fh.read(1 << 20)
            if not chunk:
                break
            if max_bytes and read + len(chunk) > max_bytes:
                h.update(chunk[: max_bytes - read])
                break
            h.update(chunk)
            read += len(chunk)
    return h.hexdigest()


class EnergyMeter:
    """Measures the energy an inference cost, or estimates it and says so.

    Tries hardware counters first and falls back to an analytical estimate,
    never silently: `Energy.source` records which happened, and an estimate
    carries the inputs it was computed from so a reader can judge it rather
    than trust it.

    Usage::

        with EnergyMeter() as meter:
            reply = run_the_model(prompt)
        attestation.energy = meter.result()

    On a machine with no readable counters this still produces a number, and
    that number is explicitly labelled `estimated`. The alternative — omitting
    energy on most hardware — would mean the field only ever appears on
    datacentre GPUs, which is where it matters least.
    """

    # Fallback power draw when nothing can be measured, in watts. Deliberately
    # a whole-package figure for a busy consumer machine rather than a precise
    # one: an estimate's job here is an order of magnitude plus an honest
    # label, and a suspiciously precise default would invite the false
    # confidence the `source` field exists to prevent.
    DEFAULT_WATTS = 45.0

    def __init__(self, *, fallback_watts: float | None = None) -> None:
        self._watts = fallback_watts or self.DEFAULT_WATTS
        self._start = 0.0
        self._elapsed = 0.0
        self._start_energy: tuple[str, float] | None = None
        self._end_energy: tuple[str, float] | None = None

    def __enter__(self) -> "EnergyMeter":
        self._start = time.perf_counter()
        self._start_energy = _read_energy_counter()
        return self

    def __exit__(self, *exc: object) -> None:
        self._elapsed = time.perf_counter() - self._start
        self._end_energy = _read_energy_counter()

    def result(self) -> Energy:
        start, end = self._start_energy, self._end_energy
        if start and end and start[0] == end[0] and end[1] >= start[1]:
            source, joules = end[0], end[1] - start[1]
            # A counter that did not move over a real interval is a counter
            # that is not actually reporting; fall through to the estimate
            # rather than claim a measured zero.
            if joules > 0:
                return Energy(millijoules=joules * 1000.0, source=source)

        joules = self._watts * self._elapsed
        return Energy(
            millijoules=joules * 1000.0,
            source="estimated",
            basis=f"{self._elapsed:.2f}s wall x {self._watts:.0f}W assumed package draw",
        )


def _read_energy_counter() -> tuple[str, float] | None:
    """Read a cumulative energy counter in joules, if the platform has one."""
    for reader in (_read_rapl, _read_nvml):
        try:
            value = reader()
        except Exception:  # noqa: BLE001 - a counter that errors is a counter we do not have
            continue
        if value is not None:
            return value
    return None


def _read_rapl() -> tuple[str, float] | None:
    """Intel/AMD RAPL, exposed by Linux as a microjoule counter in sysfs."""
    path = "/sys/class/powercap/intel-rapl:0/energy_uj"
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        return ("rapl", int(fh.read().strip()) / 1_000_000.0)


def _read_nvml() -> tuple[str, float] | None:
    """NVIDIA total energy consumption, via nvidia-smi.

    Queried through the CLI rather than the NVML bindings on purpose: the SDK
    has no runtime dependencies and is not going to acquire one for an
    optional field. `nvidia-smi` is present wherever the driver is.
    """
    exe = shutil.which("nvidia-smi")
    if not exe:
        return None
    out = subprocess.run(  # noqa: S603 - fixed executable, fixed arguments
        [exe, "--query-gpu=total_energy_consumption", "--format=csv,noheader,nounits"],
        capture_output=True, text=True, timeout=5, check=False,
    )
    if out.returncode != 0:
        return None
    first = out.stdout.strip().splitlines()
    if not first:
        return None
    # Reported in millijoules.
    return ("nvml", float(first[0].strip()) / 1000.0)
