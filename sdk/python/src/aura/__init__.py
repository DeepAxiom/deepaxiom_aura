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
from .llm import ChatBackend
from .manifest import load_manifest, validate_manifest, SKILL_TYPES
from .models import active_model_path, active_record, models_dir
from .skill import Context, Skill, run_all

Axonuron = Skill
StreamContext = Context

__all__ = [
    "active_model_path",
    "active_record",
    "models_dir",
    "Skill", "Context", "Envelope", "new_id", "PROTOCOL_MAJOR", "ChatBackend",
    "load_manifest", "validate_manifest", "SKILL_TYPES", "run_all",
    "Axonuron", "StreamContext",
    # C5 — inference attestation.
    "Attestation", "Energy", "EnergyMeter", "ENERGY_SOURCES",
    "sha256_text", "sha256_file",
]
