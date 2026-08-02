"""
ChatBackend — a minimal local-or-cloud streaming chat client shared by
first-party skills (llm-chat, and vision-reasoner).

Local by default: any GGUF via llama.cpp, downloaded from Hugging Face on
first use, no account and no network required after that. Set OPENAI_API_KEY
to switch the same skill to any OpenAI-compatible endpoint instead — OpenAI
itself, or Google's Gemini via its OpenAI-compatibility layer
(OPENAI_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai/).
Nothing else about the skill changes: same ports, same schemas, same graph.

Config (env):
  OPENAI_API_KEY    presence alone switches this backend to cloud mode
  OPENAI_BASE_URL   default: https://api.openai.com/v1
  OPENAI_MODEL      default: gpt-4o-mini
  AURA_MODEL_PATH   absolute path to a local .gguf (skips download)
"""
from __future__ import annotations

import json
import logging
import os
import urllib.request
from collections.abc import Iterator
from pathlib import Path

_logger = logging.getLogger("aura.llm")

MODELS_DIR = Path.home() / ".aura" / "models"


class ChatBackend:
    """Streams chat completions from a local GGUF model or an OpenAI-compatible API."""

    def __init__(
        self,
        *,
        model_repo: str = "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
        model_file: str = "qwen2.5-1.5b-instruct-q4_k_m.gguf",
        ctx: int = 8192,
    ) -> None:
        self._api_key = os.getenv("OPENAI_API_KEY")
        self._base_url = os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
        self._model = os.getenv("OPENAI_MODEL", "gpt-4o-mini")
        self._model_repo = model_repo
        self._model_file = model_file
        self._model_path = os.getenv("AURA_MODEL_PATH")
        self._ctx = ctx
        self._llm = None

    @property
    def is_cloud(self) -> bool:
        return bool(self._api_key)

    def stream_chat(
        self, messages: list[dict], *, max_tokens: int = 1024, temperature: float = 0.7,
    ) -> Iterator[str]:
        """Yield response text chunks as they arrive."""
        if self.is_cloud:
            yield from self._stream_cloud(messages, max_tokens, temperature)
        else:
            yield from self._stream_local(messages, max_tokens, temperature)

    def _resolve_model(self) -> str:
        if self._model_path:
            if not Path(self._model_path).exists():
                raise FileNotFoundError(f"AURA_MODEL_PATH does not exist: {self._model_path}")
            return self._model_path
        MODELS_DIR.mkdir(parents=True, exist_ok=True)
        local = MODELS_DIR / self._model_file
        if local.exists():
            return str(local)
        _logger.info("downloading default model %s/%s ...", self._model_repo, self._model_file)
        from huggingface_hub import hf_hub_download

        path = hf_hub_download(
            repo_id=self._model_repo, filename=self._model_file, local_dir=str(MODELS_DIR),
        )
        _logger.info("model downloaded to %s", path)
        return path

    def _load_local(self):
        if self._llm is None:
            from llama_cpp import Llama

            model_path = self._resolve_model()
            _logger.info("loading %s (ctx=%d) ...", model_path, self._ctx)
            self._llm = Llama(
                model_path=model_path, n_ctx=self._ctx, n_gpu_layers=-1, verbose=False,
            )
            _logger.info("model ready")
        return self._llm

    def _stream_local(self, messages: list[dict], max_tokens: int, temperature: float) -> Iterator[str]:
        llm = self._load_local()
        stream = llm.create_chat_completion(
            messages=messages, max_tokens=max_tokens, temperature=temperature, stream=True,
        )
        for chunk in stream:
            token = chunk["choices"][0]["delta"].get("content")
            if token:
                yield token

    def _stream_cloud(self, messages: list[dict], max_tokens: int, temperature: float) -> Iterator[str]:
        body = json.dumps({
            "model": self._model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "stream": True,
        }).encode("utf-8")
        req = urllib.request.Request(
            f"{self._base_url.rstrip('/')}/chat/completions",
            data=body,
            headers={
                "Authorization": f"Bearer {self._api_key}",
                "Content-Type": "application/json",
            },
        )
        with urllib.request.urlopen(req) as resp:
            for raw_line in resp:
                line = raw_line.decode("utf-8").strip()
                if not line.startswith("data: "):
                    continue
                data = line[len("data: "):]
                if data == "[DONE]":
                    break
                choice = json.loads(data)["choices"][0]
                token = choice.get("delta", {}).get("content")
                if token:
                    yield token
