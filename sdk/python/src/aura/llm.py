"""
ChatBackend — a minimal local-or-cloud streaming chat client shared by
first-party skills (llm-chat, and memory-context's summarizer).

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
import re
import urllib.request
from collections.abc import Iterator
from pathlib import Path

from .attest import Attestation

_logger = logging.getLogger("aura.llm")

MODELS_DIR = Path.home() / ".aura" / "models"

# Weight formats that execute arbitrary code when loaded.
#
# `torch.load` on a pickle runs whatever the file says to run, which has been
# a live supply-chain vector on public model hubs rather than a theoretical
# one. GGUF and safetensors are both pure data formats, so refusing the rest
# costs this backend nothing: everything it can actually run is already safe.
#
# The check exists here, at the point of use, rather than only in a scanner —
# a policy enforced somewhere else is a policy that stops applying the moment
# someone sets AURA_MODEL_PATH by hand.
_EXECUTABLE_WEIGHT_SUFFIXES = frozenset({".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle"})
_SAFE_WEIGHT_SUFFIXES = frozenset({".gguf", ".safetensors"})

# A Hugging Face commit sha: 40 hex characters.
_REVISION_RE = re.compile(r"^[0-9a-f]{40}$")


class UnsafeWeightsError(RuntimeError):
    """Raised when a weights file would execute code on load."""


def check_weights_safe(path: str | Path) -> None:
    """Refuse weight formats that deserialize to executable code.

    Raises UnsafeWeightsError rather than warning. A warning on a
    code-execution path is a warning nobody reads until afterwards, and the
    two formats this backend actually supports are both safe — so there is no
    legitimate case being blocked.
    """
    suffix = Path(path).suffix.lower()
    if suffix in _EXECUTABLE_WEIGHT_SUFFIXES:
        raise UnsafeWeightsError(
            f"refusing to load {Path(path).name}: {suffix} is a pickle-based format that "
            f"executes arbitrary code when deserialized. Use a .gguf or .safetensors "
            f"conversion of this model instead."
        )
    if suffix not in _SAFE_WEIGHT_SUFFIXES:
        _logger.warning(
            "weights file %s has an unrecognised extension (%s); expected one of %s",
            Path(path).name, suffix or "none", sorted(_SAFE_WEIGHT_SUFFIXES),
        )


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
        # C5 provenance, filled in as the model is resolved. Empty until then:
        # an attestation naming a revision the backend never actually resolved
        # would be a guess dressed as a fact.
        self._resolved_path: str | None = None
        self._revision: str = ""

    @property
    def is_cloud(self) -> bool:
        return bool(self._api_key)

    def attestation(self, **params: object) -> Attestation:
        """Build the C5 attestation for an inference this backend just ran.

        Pass the sampling parameters actually used, so the record describes
        the call rather than the defaults::

            att = backend.attestation(temperature=0.3, max_tokens=512, seed=7)
            await ctx.emit("text_out", payload, attest=att)

        The cloud path cannot pin weights — a provider's `gpt-4o-mini` is a
        moving target with no revision anyone outside can name — so the record
        says so instead of inventing a version. That gap is a real property of
        hosted inference and the attestation should show it rather than paper
        over it.
        """
        if self.is_cloud:
            return Attestation(
                engine="openai-compatible",
                model=self._model,
                # The endpoint is the closest thing to provenance available,
                # and it distinguishes OpenAI from Gemini from a local vLLM.
                engine_version=self._base_url,
                params=dict(params),
            )

        att = Attestation(
            engine="llama.cpp",
            model=self._model_repo,
            model_revision=self._revision,
            params=dict(params),
        )
        if self._resolved_path:
            att.model_file = Path(self._resolved_path).name
            att.quantization = _quantization_of(att.model_file)
        try:
            from llama_cpp import __version__ as _llama_version

            att.engine_version = _llama_version
        except Exception:  # noqa: BLE001 - version is nice to have, never required
            pass
        return att

    def stream_chat(
        self, messages: list[dict], *, max_tokens: int = 1024, temperature: float = 0.7,
        top_p: float | None = None, top_k: int | None = None,
        repeat_penalty: float | None = None,
    ) -> Iterator[str]:
        """Yield response text chunks as they arrive.

        `top_p`/`top_k`/`repeat_penalty` default to `None` (backend default,
        unchanged from before these existed). The cloud path only forwards
        `top_p` — the OpenAI-compatible API rejects unknown fields with a
        400, so `top_k`/`repeat_penalty` are dropped there if set.
        """
        if self.is_cloud:
            yield from self._stream_cloud(
                messages, max_tokens, temperature, top_p, top_k, repeat_penalty)
        else:
            yield from self._stream_local(
                messages, max_tokens, temperature, top_p, top_k, repeat_penalty)

    def _resolve_model(self) -> str:
        if self._model_path:
            if not Path(self._model_path).exists():
                raise FileNotFoundError(f"AURA_MODEL_PATH does not exist: {self._model_path}")
            check_weights_safe(self._model_path)
            self._resolved_path = self._model_path
            # A hand-supplied path carries no Hub provenance, and the
            # attestation says nothing rather than guessing a revision.
            self._revision = ""
            return self._model_path

        MODELS_DIR.mkdir(parents=True, exist_ok=True)
        local = MODELS_DIR / self._model_file
        if local.exists():
            check_weights_safe(local)
            self._resolved_path = str(local)
            self._revision = self._cached_revision()
            return str(local)

        _logger.info("downloading default model %s/%s ...", self._model_repo, self._model_file)
        from huggingface_hub import hf_hub_download

        # Resolve the repo's current commit BEFORE downloading, and pin the
        # download to it. Two reasons, both about provenance rather than
        # convenience: a bare `main` download races a push and gives you bytes
        # you cannot name afterwards, and an attestation citing "main" says
        # nothing an auditor can act on. Pinning makes `model_revision` an
        # immutable identifier for exactly these bytes.
        revision = self._head_revision()
        path = hf_hub_download(
            repo_id=self._model_repo, filename=self._model_file,
            revision=revision or None, local_dir=str(MODELS_DIR),
        )
        check_weights_safe(path)
        self._resolved_path = path
        self._revision = revision
        if revision:
            self._write_revision(revision)
            _logger.info("model downloaded to %s (revision %s)", path, revision[:12])
        else:
            _logger.info("model downloaded to %s (revision unknown)", path)
        return path

    def _head_revision(self) -> str:
        """The repo's current commit sha, or "" if it cannot be resolved."""
        try:
            from huggingface_hub import HfApi

            sha = HfApi().model_info(self._model_repo).sha or ""
        except Exception as exc:  # noqa: BLE001 - offline or private repo: not fatal
            _logger.debug("could not resolve revision for %s: %s", self._model_repo, exc)
            return ""
        return sha if _REVISION_RE.match(sha) else ""

    # The revision is cached beside the weights so a restart attests the same
    # provenance it downloaded with, instead of silently dropping to "unknown"
    # the moment the file is already on disk.
    def _revision_marker(self) -> Path:
        return MODELS_DIR / (self._model_file + ".revision")

    def _cached_revision(self) -> str:
        try:
            sha = self._revision_marker().read_text(encoding="utf-8").strip()
        except OSError:
            return ""
        return sha if _REVISION_RE.match(sha) else ""

    def _write_revision(self, revision: str) -> None:
        try:
            self._revision_marker().write_text(revision, encoding="utf-8")
        except OSError as exc:  # noqa: BLE001 - a cache miss next time, nothing worse
            _logger.debug("could not record revision marker: %s", exc)

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

    def _stream_local(
        self, messages: list[dict], max_tokens: int, temperature: float,
        top_p: float | None, top_k: int | None, repeat_penalty: float | None,
    ) -> Iterator[str]:
        llm = self._load_local()
        kwargs = {}
        if top_p is not None:
            kwargs["top_p"] = top_p
        if top_k is not None:
            kwargs["top_k"] = top_k
        if repeat_penalty is not None:
            kwargs["repeat_penalty"] = repeat_penalty
        stream = llm.create_chat_completion(
            messages=messages, max_tokens=max_tokens, temperature=temperature, stream=True,
            **kwargs,
        )
        for chunk in stream:
            token = chunk["choices"][0]["delta"].get("content")
            if token:
                yield token

    def _stream_cloud(
        self, messages: list[dict], max_tokens: int, temperature: float,
        top_p: float | None, top_k: int | None, repeat_penalty: float | None,
    ) -> Iterator[str]:
        if top_k is not None or repeat_penalty is not None:
            _logger.debug(
                "cloud backend does not support top_k/repeat_penalty, dropping "
                "(top_k=%r, repeat_penalty=%r)", top_k, repeat_penalty,
            )
        payload = {
            "model": self._model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "stream": True,
        }
        if top_p is not None:
            payload["top_p"] = top_p
        body = json.dumps(payload).encode("utf-8")
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


def _quantization_of(filename: str) -> str:
    """Recover the quantization from a GGUF file name.

    The convention (`…-q4_k_m.gguf`, `…-Q8_0.gguf`, `…-f16.gguf`) is universal
    enough on the Hub to be worth parsing: quantization changes the output
    distribution and is the single most common undeclared difference between
    "the same model" in two places, so an attestation that omits it is missing
    the field most likely to explain a behavioural discrepancy.

    Returns "" rather than guessing when the name does not follow it.
    """
    match = re.search(r"[.\-_]((?:iq|q)\d+[a-z0-9_]*|f16|f32|bf16)(?=\.gguf$)",
                      filename, re.IGNORECASE)
    return match.group(1).upper() if match else ""
