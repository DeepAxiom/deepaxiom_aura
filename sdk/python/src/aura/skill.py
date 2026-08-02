"""
Skill — the atomic unit of function in the runtime (SDK v5, contract C1/C3).

A Skill is something the system *knows how to do*. Five types:
  sensorial (perceives) · cognitive (reasons) · motor (acts) ·
  memory (remembers) · logical (transforms/validates)

Design contract:
  - The skill is the CLIENT: it connects out to the kernel (NAT-friendly IoC).
  - First frame: a C3 `register` envelope carrying the C1 manifest.
  - Data envelopes are dispatched to the ingress handler for their port.
  - At-least-once delivery: the SDK deduplicates by idempotency key.
  - On disconnect: exponential backoff, reconnect indefinitely.

Usage::

    from aura import Skill, Context

    skill = Skill()  # reads skill.yaml from CWD

    @skill.on("text_in")
    async def handle(ctx: Context) -> None:
        await ctx.emit("text_out", {"text": ctx.payload["text"]})
        await ctx.done("text_out")

    skill.run()
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import threading
import time
from collections import OrderedDict
from collections.abc import Awaitable, Callable
from typing import Any

import websockets

from .envelope import (
    KIND_CANCEL,
    KIND_CONFIG_UPDATE,
    KIND_DATA,
    KIND_DONE,
    KIND_ERROR,
    KIND_REGISTER,
    KIND_STATUS,
    PROTOCOL_MAJOR,
    Envelope,
    new_id,
)
from .manifest import load_manifest, validate_manifest

_logger = logging.getLogger("aura.skill")
_BACKOFF = [1, 2, 5, 10, 30, 60]

# race condition medio rara: un cancel puede llegar ANTES que el envelope
# que cancela. El kernel lo reenvia apenas el cliente lo pide, y el data
# envelope capaz sigue en vuelo todavia. Por eso el cancel mark tiene que
# sobrevivir mas que el handler al que se refiere — necesita bound + expiry,
# no alcanza con limpiarlo cuando el handler termina.
_CANCEL_TTL_SECONDS = 300
_MAX_CANCEL_MARKS = 4096
_MAX_DEDUP_KEYS = 16384


class Context:
    """Handler context: the incoming envelope + causally-linked emission."""

    def __init__(self, skill: "Skill", incoming: Envelope) -> None:
        self._skill = skill
        self._incoming = incoming

    @property
    def payload(self) -> Any:
        return self._incoming.payload

    @property
    def id(self) -> str:
        """Id del envelope entrante — para encadenar causalidad (C3)."""
        return self._incoming.id

    @property
    def cause_id(self) -> str:
        return self._incoming.cause_id

    @property
    def cancelled(self) -> bool:
        """True once the kernel has forwarded a "cancel" (C3) naming this
        handler's cause chain — see spec/c3-channel.md. Best-effort:
        check it between chunks of a long-running generation (e.g. a
        streaming chat completion) and stop early; nothing enforces this,
        it is on each handler to look.

        Whatever you emit after a cancel is suppressed by the kernel anyway;
        checking this just stops you burning time producing it."""
        return self._skill.is_cancelled(self.session, self._incoming.cause_id)

    @property
    def cancel_event(self) -> threading.Event:
        """The same signal as `cancelled`, readable from a worker thread.

        `cancelled` is a property on the async side, which a blocking call
        moved off the event loop with `asyncio.to_thread` cannot poll — model
        inference and speech synthesis both run that way. Pass this Event into
        the worker and check it between chunks::

            def synth(text, stop):
                for chunk in engine.stream(text):
                    if stop.is_set():
                        return
                    yield chunk

            await asyncio.to_thread(synth, text, ctx.cancel_event)
        """
        return self._skill._cancel_event_for(self.session, self._incoming.cause_id)

    @property
    def session(self) -> str:
        return self._incoming.session

    @property
    def node(self) -> str:
        """This skill's node ref inside the running graph."""
        return self._incoming.node

    async def emit(self, port: str, payload: Any, *, kind: str = KIND_DATA) -> None:
        """Emit on an egress port, causally linked to the incoming envelope."""
        schema = self._skill.egress_schema(port)
        seq = self._skill._next_seq(self.session, port)
        env = Envelope(
            kind=kind,
            cause_id=self._incoming.id,
            session=self.session,
            node=self.node,
            port=port,
            seq=seq,
            idem=f"{self._incoming.idem}:{self.node}:{port}:{seq}",
            schema=schema,
            payload=payload,
        )
        await self._skill._send(env)

    async def done(self, port: str) -> None:
        await self.emit(port, None, kind=KIND_DONE)

    async def status(self, port: str, state: str, detail: str = "") -> None:
        await self.emit(port, {"state": state, "detail": detail}, kind=KIND_STATUS)

    async def error(self, port: str, detail: str) -> None:
        await self.emit(port, {"state": "error", "detail": detail}, kind=KIND_ERROR)


class Skill:
    """One skill: a C1 manifest plus handlers for its ingress ports.

    The manifest normally comes from a `skill.yaml` next to the code. Pass
    `manifest=` instead to build one at runtime — that is how a single process
    turns a declarative spec into several skills (see `skills/connector/`)
    without writing throwaway YAML files to disk first.
    """

    def __init__(self, manifest_path: str = "skill.yaml", *,
                 manifest: dict[str, Any] | None = None) -> None:
        if manifest is not None:
            validate_manifest(manifest)
            self.manifest: dict[str, Any] = manifest
        else:
            self.manifest = load_manifest(manifest_path)
        self._handlers: dict[str, Callable[[Context], Awaitable[None]]] = {}
        self._ws_url: str = os.getenv("AURA_WS_URL", "ws://localhost:9080/ws/skill")
        self._ws: Any = None
        self._send_lock = asyncio.Lock()
        # ojo con esto: el key de dedup es (session, idem), no solo idem —
        # idem nada mas es unico DENTRO de una sesion, so a process-wide key
        # by itself would let two sessions silently step on each other's
        # envelopes. Bug bien sutil si no lo tenes en cuenta.
        self._seen: OrderedDict[tuple[str, str], None] = OrderedDict()
        # cancel marks: (session, cause_id) -> cuando se marcaron. Bounded y
        # con expiry, NO se limpian cuando el handler termina — a veces un
        # cancel llega antes que el propio data envelope que cancela, y aun
        # asi tiene que contar.
        self._cancelled: OrderedDict[tuple[str, str], float] = OrderedDict()
        self._cancel_events: dict[tuple[str, str], threading.Event] = {}
        # C3 pide seq monotonico por (session, node, port). Vive en el skill
        # y no en cada invocacion del handler a proposito: un contador por
        # invocacion arrancaria en 1 cada vez, y ahi se pierde la gap
        # detection que seq existe para dar — que es como un cliente se da
        # cuenta que un canal realtime le tiro un frame al piso.
        self._seq: dict[tuple[str, str], int] = {}
        self._tasks: set[asyncio.Task] = set()
        # Runtime config (C1 `config`, additive). Arranca en los defaults
        # declarados para que el skill pueda leer skill.config aun antes de
        # que llegue el ack de registro del kernel (o si nunca llega, e.g.
        # hablando con un kernel viejo). El ack del kernel lo pisa con los
        # valores efectivos reales (default < --config file < override en
        # vivo); un config_update posterior (C3, additive) lo vuelve a pisar
        # si algo cambia mientras esta conectado. Los handlers solo leen
        # skill.config[key] y listo — siempre al dia, no hay que pensarlo.
        self.config: dict[str, Any] = {
            p["key"]: p.get("default") for p in self.manifest.get("config", [])
        }

    def on(self, port: str) -> Callable:
        """Register the async handler for a declared ingress port."""
        declared = {p["name"] for p in self.manifest["ports"].get("ingress", [])}
        if port not in declared:
            raise ValueError(
                f"port {port!r} is not a declared ingress port {sorted(declared)}"
            )

        def decorator(fn: Callable[[Context], Awaitable[None]]) -> Callable:
            self._handlers[port] = fn
            return fn

        return decorator

    on_ingress = on

    def egress_schema(self, port: str) -> str:
        for p in self.manifest["ports"].get("egress", []):
            if p["name"] == port:
                return p["schema"]
        raise ValueError(f"port {port!r} is not a declared egress port")

    def run(self) -> None:
        """Connect, register, serve. Blocks; reconnects forever."""
        asyncio.run(self._serve())

    async def _send(self, env: Envelope) -> None:
        async with self._send_lock:
            if self._ws is None:
                raise ConnectionError("not connected to kernel")
            await self._ws.send(json.dumps(env.to_wire()))

    def _next_seq(self, session: str, port: str) -> int:
        key = (session, port)
        self._seq[key] = self._seq.get(key, 0) + 1
        return self._seq[key]

    def _duplicate(self, session: str, idem: str) -> bool:
        if not idem:
            return False
        key = (session, idem)
        if key in self._seen:
            return True
        self._seen[key] = None
        if len(self._seen) > _MAX_DEDUP_KEYS:
            self._seen.popitem(last=False)
        return False

    def _apply_config(self, values: dict[str, Any] | None) -> None:
        if not values:
            return
        changed = {k: v for k, v in values.items() if self.config.get(k) != v}
        self.config.update(values)
        if changed:
            _logger.info("config updated: %s", changed)

    def _expire_cancel_marks(self) -> None:
        cutoff = time.monotonic() - _CANCEL_TTL_SECONDS
        while self._cancelled:
            key, marked_at = next(iter(self._cancelled.items()))
            if marked_at >= cutoff and len(self._cancelled) <= _MAX_CANCEL_MARKS:
                break
            self._cancelled.pop(key, None)
            self._cancel_events.pop(key, None)

    def is_cancelled(self, session: str, cause_id: str) -> bool:
        return bool(cause_id) and (session, cause_id) in self._cancelled

    def _mark_cancelled(self, session: str, cause_id: str) -> None:
        if not cause_id:
            return
        key = (session, cause_id)
        self._cancelled[key] = time.monotonic()
        self._cancelled.move_to_end(key)
        self._expire_cancel_marks()
        # Wake any worker thread already blocked on this chain.
        event = self._cancel_events.get(key)
        if event is not None:
            event.set()

    def _cancel_event_for(self, session: str, cause_id: str) -> threading.Event:
        key = (session, cause_id)
        event = self._cancel_events.get(key)
        if event is None:
            event = threading.Event()
            self._cancel_events[key] = event
        # A cancel may already have landed before the handler asked for this.
        if key in self._cancelled:
            event.set()
        return event

    def _release_cancel_event(self, session: str, cause_id: str) -> None:
        """Drop the Event once its handler is done. The cancel *mark* stays —
        it expires on its own — so a late-arriving duplicate is still seen as
        cancelled."""
        self._cancel_events.pop((session, cause_id), None)

    async def _serve(self) -> None:
        skill_id = self.manifest["id"]
        attempt = 0
        _logger.info("skill %s starting (kernel: %s)", skill_id, self._ws_url)
        while True:
            try:
                async with websockets.connect(
                    self._ws_url, ping_interval=20, ping_timeout=10, open_timeout=10
                ) as ws:
                    self._ws = ws
                    attempt = 0
                    reg = Envelope(kind=KIND_REGISTER, payload=self.manifest)
                    await ws.send(json.dumps(reg.to_wire()))
                    ack = Envelope.from_wire(json.loads(await ws.recv()))
                    if ack.kind == KIND_ERROR:
                        raise RuntimeError(f"kernel rejected manifest: {ack.payload}")
                    _logger.info("registered: %s", ack.payload)
                    self._apply_config((ack.payload or {}).get("config"))
                    async for raw in ws:
                        task = asyncio.create_task(self._dispatch(json.loads(raw)))
                        self._tasks.add(task)
                        task.add_done_callback(self._tasks.discard)
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001
                self._ws = None
                delay = _BACKOFF[min(attempt, len(_BACKOFF) - 1)]
                attempt += 1
                _logger.warning("disconnected (%s); retry in %ss", exc, delay)
                await asyncio.sleep(delay)

    async def _dispatch(self, wire: dict) -> None:
        env = Envelope.from_wire(wire)
        if env.kind == KIND_CANCEL:
            self._mark_cancelled(env.session, env.cause_id)
            return
        if env.kind == KIND_CONFIG_UPDATE:
            self._apply_config(env.payload)
            return
        if env.kind not in (KIND_DATA, KIND_DONE):
            return
        if self._duplicate(env.session, env.idem):
            return
        handler = self._handlers.get(env.port)
        if handler is None:
            _logger.debug("no handler for port %s", env.port)
            return
        ctx = Context(self, env)
        try:
            await handler(ctx)
        except Exception as exc:  # noqa: BLE001
            _logger.exception("handler for %s failed", env.port)
            try:
                egress = self.manifest["ports"].get("egress", [])
                if egress:
                    await ctx.error(egress[0]["name"], f"{type(exc).__name__}: {exc}")
            except Exception:  # noqa: BLE001
                pass
        finally:
            self._release_cancel_event(env.session, env.cause_id)


def run_all(skills: list[Skill]) -> None:
    """Run several skills from one process, each on its own kernel connection.

    `Skill.run()` serves exactly one skill and blocks. A process that hosts a
    whole declarative spec — one skill per operation — needs them all served
    concurrently, and needs the process to stay up as long as any of them can
    still reconnect. Each skill keeps its own independent reconnect loop, so a
    kernel restart brings all of them back.

    Blocks until interrupted::

        run_all([build(op) for op in spec["operations"]])
    """
    if not skills:
        raise ValueError("run_all needs at least one skill")

    async def serve() -> None:
        await asyncio.gather(*(s._serve() for s in skills))

    try:
        asyncio.run(serve())
    except KeyboardInterrupt:  # pragma: no cover - operator interrupt
        _logger.info("stopped")
