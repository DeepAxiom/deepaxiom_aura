"""
model-manager catalog — the curated list of models AURA knows how to fetch.

Kept as plain data in code, deliberately with no external file/URL loading
mechanism: this is a small, hand-picked list a developer edits when adding a
model, not a set of values an operator is expected to tune at runtime (that
is what skill.yaml's `config` block is for).
"""

CATALOG = [
    {"model_id": "qwen2.5-1.5b-instruct", "model_type": "llm", "tier": "mvp",
     "hf_repo": "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
     "hf_filename": "qwen2.5-1.5b-instruct-q4_k_m.gguf"},
    {"model_id": "gemma-3-1b-it", "model_type": "llm", "tier": "mvp",
     "hf_repo": "bartowski/gemma-3-1B-it-GGUF",
     "hf_filename": "gemma-3-1B-it-Q4_K_M.gguf"},
    {"model_id": "gemma-3-4b-it", "model_type": "llm", "tier": "recommended",
     "hf_repo": "bartowski/gemma-3-4B-it-GGUF",
     "hf_filename": "gemma-3-4B-it-Q4_K_M.gguf"},
    {"model_id": "deepseek-r1-qwen-7b", "model_type": "llm", "tier": "recommended",
     "hf_repo": "bartowski/DeepSeek-R1-Distill-Qwen-7B-GGUF",
     "hf_filename": "DeepSeek-R1-Distill-Qwen-7B-Q4_K_M.gguf"},
    {"model_id": "whisper-base", "model_type": "asr", "tier": "mvp",
     "hf_repo": "Systran/faster-whisper-base", "hf_filename": "(faster-whisper auto)"},
    {"model_id": "whisper-small", "model_type": "asr", "tier": "recommended",
     "hf_repo": "Systran/faster-whisper-small", "hf_filename": "(faster-whisper auto)"},
    {"model_id": "nomic-embed-text-v1.5", "model_type": "embedding", "tier": "recommended",
     "hf_repo": "nomic-ai/nomic-embed-text-v1.5-GGUF",
     "hf_filename": "nomic-embed-text-v1.5.Q8_0.gguf"},
    {"model_id": "piper-es-mx", "model_type": "tts", "tier": "recommended",
     "hf_repo": "rhasspy/piper-voices",
     "hf_filename": "es/es_MX/claude/high/es_MX-claude-high.onnx"},
]


def filter_catalog(model_type: str | None) -> list[dict]:
    """Catalog entries matching `model_type`, or the whole catalog if unset."""
    if not model_type:
        return list(CATALOG)
    return [e for e in CATALOG if e["model_type"] == model_type]


def find_model(model_id: str) -> dict | None:
    """Look up one catalog entry by model_id, or None if it isn't in CATALOG."""
    return next((e for e in CATALOG if e["model_id"] == model_id), None)
