"""aura — SDK v5 for building Skills against the AURA kernel."""
from .attest import (
    ENERGY_SOURCES,
    Attestation,
    Energy,
    EnergyMeter,
    sha256_file,
    sha256_text,
)
from .envelope import Envelope, new_id, PROTOCOL_MAJOR
from .llm import ChatBackend, compact_history
from .manifest import (LEGACY_MANIFEST_FILE, MANIFEST_FILE, SKILL_TYPES,
                       default_manifest_path, load_manifest, validate_manifest)
from .models import active_model_path, active_record, models_dir
from .skill import Context, Expertise, Skill, run_all
from .stream import DeltaCoalescer

Axonuron = Skill
StreamContext = Context

__all__ = [
    "active_model_path",
    "active_record",
    "models_dir",
    "Expertise", "Skill", "Context", "Envelope", "new_id", "PROTOCOL_MAJOR", "ChatBackend",
    "load_manifest", "validate_manifest", "SKILL_TYPES", "run_all",
    "Axonuron", "StreamContext",
    # C5 — inference attestation.
    "Attestation", "Energy", "EnergyMeter", "ENERGY_SOURCES",
    "sha256_text", "sha256_file",
    # Streaming helpers.
    "DeltaCoalescer", "compact_history",
    # The manifest file, under its current name and the one it replaced.
    "MANIFEST_FILE", "LEGACY_MANIFEST_FILE", "default_manifest_path",
]
