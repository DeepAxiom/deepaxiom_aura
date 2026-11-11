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
  OPENAI_API_KEY      presence alone switches this backend to cloud mode
  OPENAI_BASE_URL     default: https://api.openai.com/v1
  OPENAI_MODEL        default: gpt-4o-mini
  AURA_MODEL_PATH     absolute path to a local .gguf (skips download)
  AURA_INFERENCE_URL  an OpenAI-compatible inference server this node runs
                      itself (llama-server, vLLM, ...), e.g.
                      http://127.0.0.1:8080/v1. No key needed. See below.
  AURA_INFERENCE_MODEL  model name to send there (default: OPENAI_MODEL)
  AURA_INFERENCE_EXTRA  JSON object merged into every request body sent
                        there — server-specific knobs such as
                        {"cache_prompt": true}
  AURA_LLM_TIMEOUT    seconds a remote read may stall (default 120)
  AURA_LLM_CACHE_MB   in-process KV state cache for the local backend, in MiB
                      (default 0 = off). See _attach_state_cache.

One set of weights for the whole node
-------------------------------------
In-process llama.cpp is one context: one sequence at a time, and one copy of
the weights *per skill process*. llm-chat, the planner and memory-context's
summarizer each loading a 4B model is three copies on a card that fits one,
and N conversations on one context means N-1 of them evict each other's KV
cache every turn.

`AURA_INFERENCE_URL` is the way out, and it is deliberately not a new
protocol: point every first-party skill at one server that already does slots,
continuous batching and per-slot prefix caching, and they all become clients
of the same weights. The request is the same OpenAI-compatible call the cloud
path makes, so nothing else about a skill changes. What it buys is only real
if the prompt prefix is stable between turns — see `compact_history`.
"""
from __future__ import annotations

import json
import logging
import os
import re
import threading
import urllib.error
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


def compact_history(messages: list[dict], limit: int) -> int:
    """Trim a conversation in place so its *prefix stays stable*, and return
    how many messages were dropped.

    The obvious trim — drop the oldest turn every time the list is over the
    limit — is the worst thing that can be done to a KV cache. Once a
    conversation reaches the limit it then loses its second and third message
    on *every* turn, so every prompt differs from the previous one right after
    the system message, and the model re-evaluates the whole window each time.
    The cache hit rate goes from "everything but the new turn" to "the system
    prompt", exactly when the context is at its longest and re-evaluating it
    costs the most. The same holds for a hosted model's prompt cache, which
    is billed on that prefix.

    So this trims in blocks: nothing is dropped until the list exceeds
    `limit`, and then the oldest half goes at once. Between two compactions
    the prompt only ever grows at the end, which is the shape every prefix
    cache — llama.cpp's, a server slot's, a provider's — is built for. The
    price is that the window sawtooths between limit/2 and limit instead of
    sitting at limit; a model that sees between 20 and 40 messages and answers
    in a fraction of the time is the better trade.

    `messages[0]` is kept when it is the system prompt. Turns are dropped in
    pairs so the list never starts on an assistant message, which several
    chat templates reject outright.
    """
    if limit < 2 or len(messages) <= limit:
        return 0
    head = 1 if messages and messages[0].get("role") == "system" else 0
    keep = max(2, limit // 2)
    drop = len(messages) - head - keep
    drop -= drop % 2
    if drop <= 0:
        return 0
    del messages[head:head + drop]
    return drop


def _usage_from(raw: object) -> dict:
    """Normalize an OpenAI-shaped `usage` object to the C5 `usage` block.

    Three numbers and nothing else, because those are the three every engine
    can state and every budget needs: what went in, what came out, and how
    much of what went in was served from a cache rather than evaluated.
    """
    if not isinstance(raw, dict):
        return {}
    out: dict[str, int] = {}
    for src, dst in (("prompt_tokens", "input_tokens"),
                     ("completion_tokens", "output_tokens")):
        value = raw.get(src)
        if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
            out[dst] = value
    details = raw.get("prompt_tokens_details")
    cached = details.get("cached_tokens") if isinstance(details, dict) else None
    if isinstance(cached, int) and not isinstance(cached, bool) and cached >= 0:
        out["cached_input_tokens"] = cached
    return out


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
        # A self-hosted inference server. OPENAI_API_KEY still wins when both
        # are set: a deployment that already chose a hosted model keeps it, and
        # adding the new variable never silently moves traffic.
        self._inference_url = (
            "" if self._api_key else os.getenv("AURA_INFERENCE_URL", "").strip())
        self._extra_body: dict = {}
        if self._inference_url:
            self._base_url = self._inference_url
            self._model = os.getenv("AURA_INFERENCE_MODEL", "").strip() or self._model
            self._extra_body = _extra_body_from_env()
        self._timeout = _float_env("AURA_LLM_TIMEOUT", 120.0)
        # Whether the endpoint accepts `stream_options`. Assumed until an
        # endpoint says otherwise with a 400, then remembered: asking again on
        # every reply would cost a failed round trip per message forever.
        self._usage_supported = True
        # One sequence at a time on the in-process context. See _stream_local.
        self._local_gate = threading.Lock()
        self._cache_mb = int(_float_env("AURA_LLM_CACHE_MB", 0.0))
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
        """True when inference happens over HTTP rather than in this process —
        a hosted API or a self-hosted server. The name predates the second
        case and is kept because skills already branch on it."""
        return bool(self._api_key) or bool(self._inference_url)

    def attestation(self, *, usage: dict | None = None, **params: object) -> Attestation:
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
                usage=dict(usage) if usage else None,
            )

        att = Attestation(
            engine="llama.cpp",
            model=self._model_repo,
            model_revision=self._revision,
            params=dict(params),
            usage=dict(usage) if usage else None,
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
        stop: threading.Event | None = None,
        usage: dict | None = None,
    ) -> Iterator[str]:
        """Yield response text chunks as they arrive.

        `top_p`/`top_k`/`repeat_penalty` default to `None` (backend default,
        unchanged from before these existed). The cloud path only forwards
        `top_p` — the OpenAI-compatible API rejects unknown fields with a
        400, so `top_k`/`repeat_penalty` are dropped there if set.

        `stop` abandons the generation: checked while waiting for the model
        and between chunks, so a caller that no longer wants the reply does
        not hold the model against one that does.

        `usage`, if a dict is passed, is filled in when the stream ends with
        what the engine actually counted — `input_tokens`, `output_tokens`
        and, where the engine reports it, `cached_input_tokens`. A dict the
        caller owns rather than an attribute on the backend, because one
        backend serves many concurrent replies and "the last usage" would
        belong to whichever finished most recently. Left empty when the
        engine reports nothing: a count this code made up would be worse than
        none, since budgets get set from it.
        """
        if self.is_cloud:
            yield from self._stream_cloud(
                messages, max_tokens, temperature, top_p, top_k, repeat_penalty,
                stop=stop, usage=usage)
        else:
            yield from self._stream_local(
                messages, max_tokens, temperature, top_p, top_k, repeat_penalty,
                stop=stop, usage=usage)

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
            self._attach_state_cache(self._llm)
            _logger.info("model ready")
        return self._llm

    def _attach_state_cache(self, llm) -> None:
        """Keep evaluated KV state for more than one conversation.

        llama.cpp reuses the longest prefix shared with whatever is *currently
        in the context*, which is perfect for one conversation and useless for
        two: session B's turn evicts session A's state, and A pays for its
        whole history again. A state cache saves the context keyed by its
        token prefix and restores the best match, so interleaved conversations
        each resume from their own prefix.

        Off unless AURA_LLM_CACHE_MB is set. It trades RAM for latency, the
        right size depends on context length and model, and a node with one
        conversation gains nothing from it — none of which this code can know.
        A failure to attach is logged and ignored: the cache is an
        optimisation, and a model that answers slowly beats one that does not
        load.
        """
        if self._cache_mb <= 0:
            return
        try:
            from llama_cpp import LlamaRAMCache

            llm.set_cache(LlamaRAMCache(capacity_bytes=self._cache_mb << 20))
            _logger.info("KV state cache: %d MiB", self._cache_mb)
        except Exception as exc:  # noqa: BLE001
            _logger.warning("could not attach a KV state cache (%s); continuing without", exc)

    def _stream_local(
        self, messages: list[dict], max_tokens: int, temperature: float,
        top_p: float | None, top_k: int | None, repeat_penalty: float | None,
        *, stop: threading.Event | None = None, usage: dict | None = None,
    ) -> Iterator[str]:
        llm = self._load_local()
        kwargs = {}
        if top_p is not None:
            kwargs["top_p"] = top_p
        if top_k is not None:
            kwargs["top_k"] = top_k
        if repeat_penalty is not None:
            kwargs["repeat_penalty"] = repeat_penalty

        # One llama.cpp context evaluates one sequence. Two replies generating
        # on it at once — two sessions, each in its own worker thread — is not
        # slow, it is undefined: they interleave tokens into the same KV state
        # and, depending on the build, corrupt both replies or crash the
        # process. Nothing serialised this before, and it only looked fine
        # because a single-user node never has two replies in flight.
        #
        # Waited for in short slices rather than blocked on, so a reply that
        # was cancelled while queueing behind another gives up its place
        # instead of generating words nobody will read once its turn comes.
        while not self._local_gate.acquire(timeout=0.05):
            if stop is not None and stop.is_set():
                return
        try:
            if stop is not None and stop.is_set():
                return
            stream = llm.create_chat_completion(
                messages=messages, max_tokens=max_tokens, temperature=temperature, stream=True,
                **kwargs,
            )
            produced = 0
            try:
                for chunk in stream:
                    if stop is not None and stop.is_set():
                        break
                    token = chunk["choices"][0]["delta"].get("content")
                    if token:
                        produced += 1
                        yield token
            finally:
                # Closing the engine's generator is what actually stops it
                # evaluating; dropping the reference only stops us listening.
                close = getattr(stream, "close", None)
                if callable(close):
                    close()
            if usage is not None:
                usage.update(_local_usage(llm, produced))
        finally:
            self._local_gate.release()

    def _stream_cloud(
        self, messages: list[dict], max_tokens: int, temperature: float,
        top_p: float | None, top_k: int | None, repeat_penalty: float | None,
        *, stop: threading.Event | None = None, usage: dict | None = None,
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
        # Server-specific knobs for a self-hosted endpoint. Never sent to a
        # hosted API: those reject unknown fields with a 400.
        for key, value in self._extra_body.items():
            payload.setdefault(key, value)

        with self._open_stream(payload) as resp:
            for raw_line in resp:
                if stop is not None and stop.is_set():
                    # Leaving the `with` closes the socket, which is the only
                    # cancel a streaming HTTP completion has — and the one
                    # that stops the provider generating (and billing).
                    break
                line = raw_line.decode("utf-8").strip()
                if not line.startswith("data: "):
                    continue
                data = line[len("data: "):]
                if data == "[DONE]":
                    break
                event = json.loads(data)
                if usage is not None and event.get("usage"):
                    usage.update(_usage_from(event["usage"]))
                # The usage chunk arrives with an empty `choices` list.
                choices = event.get("choices") or []
                if not choices:
                    continue
                token = choices[0].get("delta", {}).get("content")
                if token:
                    yield token

    def _open_stream(self, payload: dict):
        """POST a streaming completion, asking for token usage when the
        endpoint will give it.

        `stream_options.include_usage` makes the endpoint finish the stream
        with what it actually counted, cache hits included. That is the only
        honest source of a token count for a hosted model, and it is what
        turns the kernel's bytes/4 estimate into a number a budget can be set
        from. Not every OpenAI-compatible server knows the field, and the
        strict ones answer 400 — so the first refusal is retried once without
        it and remembered, and the reply still streams.
        """
        headers = {"Content-Type": "application/json"}
        if self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"
        url = f"{self._base_url.rstrip('/')}/chat/completions"

        def post(body: dict):
            req = urllib.request.Request(
                url, data=json.dumps(body).encode("utf-8"), headers=headers)
            # A timeout on every socket read. Without one a provider that
            # accepts the connection and then stalls holds this worker thread,
            # and the session behind it, forever — the reply never fails, so
            # nothing ever reports it.
            return urllib.request.urlopen(req, timeout=self._timeout)

        if not self._usage_supported:
            return post(payload)
        try:
            return post({**payload, "stream_options": {"include_usage": True}})
        except urllib.error.HTTPError as exc:
            if exc.code != 400:
                raise
            _logger.info(
                "%s refused stream_options; token usage will not be reported "
                "for this endpoint", self._base_url)
            self._usage_supported = False
            return post(payload)


def _local_usage(llm, produced: int) -> dict:
    """What an in-process generation cost, read off the context.

    After a generation the context holds prompt + completion, so the prompt
    is the difference. Streaming yields one chunk per token, which is what
    `produced` counted. Anything that does not add up is left out rather than
    reported: this is bookkeeping, and it must not be able to fail a reply.
    """
    try:
        total = int(llm.n_tokens)
    except Exception:  # noqa: BLE001
        return {}
    if total <= 0 or produced < 0 or produced > total:
        return {}
    return {"input_tokens": total - produced, "output_tokens": produced}


def _float_env(name: str, default: float) -> float:
    try:
        value = float(os.getenv(name, "") or default)
    except ValueError:
        return default
    return value if value > 0 else default


def _extra_body_from_env() -> dict:
    raw = os.getenv("AURA_INFERENCE_EXTRA", "").strip()
    if not raw:
        return {}
    try:
        parsed = json.loads(raw)
    except ValueError:
        _logger.warning("AURA_INFERENCE_EXTRA is not valid JSON; ignoring it")
        return {}
    if not isinstance(parsed, dict):
        _logger.warning("AURA_INFERENCE_EXTRA must be a JSON object; ignoring it")
        return {}
    return parsed


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
