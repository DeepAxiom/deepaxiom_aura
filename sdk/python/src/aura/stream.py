"""DeltaCoalescer — fewer, larger envelopes for a token stream, with the same
meaning.

A model emits a token every few milliseconds, and the natural thing to do is
emit an envelope for each. Every one of those is a ULID, two JSON encodings,
two appends to the causal event log, a routing decision and a frame on at
least two sockets — for four characters of text. On one conversation that
is invisible. On a node with a few hundred live replies it *is* the load:
the kernel spends its time moving envelope headers, not content.

`std/text@1` makes the fix free. A partial's `text` is a delta that
consumers append, so "a", "b", "c" and "abc" are the same stream by
definition — no consumer can tell them apart except by how many frames
arrived. Coalescing changes the cost and nothing else: the causal chain is
intact (every envelope still names the delivery that caused it), ordering is
intact, and the final marker is still its own envelope.

What it must not do is add latency a person can feel, so three rules:

  - **The first delta goes out immediately.** Time-to-first-token is the
    number a listener perceives, and it is never traded.
  - **Nothing waits longer than `max_delay`.** A timer flushes a buffered
    delta even if the model stalls right after producing it, so a pause in
    generation cannot strand text.
  - **A large buffer flushes at once** (`max_chars`), so a fast model
    produces steady frames instead of one frame per timer tick.

At the default 25 ms a slow local model (under 40 tokens/s) still gets one
envelope per token — every delta arrives after the window has already
elapsed — and a fast one drops from hundreds of envelopes a second to forty.

Only for schemas whose partials concatenate. `std/transcript@1` partials
*replace* each other, and coalescing those would be a different mistake.
"""
from __future__ import annotations

import asyncio
import time
from collections.abc import Awaitable, Callable


class DeltaCoalescer:
    """Buffer text deltas and hand them to `emit` in bounded batches.

    Usage::

        out = DeltaCoalescer(
            lambda text: ctx.emit("text_out", {"text": text, "final": False}),
            max_delay=0.025)
        for token in tokens:
            await out.add(token)
        await out.close()          # flush whatever is left
        await ctx.emit("text_out", {"text": "", "final": True})

    `max_delay <= 0` disables coalescing: every `add` emits at once, which is
    exactly the behaviour before this class existed.
    """

    def __init__(
        self, emit: Callable[[str], Awaitable[None]], *,
        max_delay: float = 0.025, max_chars: int = 256,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._emit = emit
        self._max_delay = max_delay
        self._max_chars = max(1, max_chars)
        self._clock = clock
        self._parts: list[str] = []
        self._size = 0
        self._last_flush: float | None = None
        self._timer: asyncio.Task | None = None
        self._timer_emitting = False
        # Serialises emits so a timer flush and an inline flush cannot
        # interleave and deliver two deltas out of order.
        self._lock = asyncio.Lock()
        self.emitted = 0  # envelopes handed to `emit`, for tests and metrics

    async def add(self, text: str) -> None:
        if not text:
            return
        if self._max_delay <= 0:
            self.emitted += 1
            await self._emit(text)
            return

        self._parts.append(text)
        self._size += len(text)
        now = self._clock()
        due = self._last_flush is None or now - self._last_flush >= self._max_delay
        if due or self._size >= self._max_chars:
            await self._flush()
            return
        if self._timer is None:
            remaining = self._max_delay - (now - (self._last_flush or now))
            self._timer = asyncio.get_running_loop().create_task(self._flush_later(remaining))

    async def _flush_later(self, delay: float) -> None:
        await asyncio.sleep(max(delay, 0))
        # From here on this task may be holding text it took out of the
        # buffer, so it must be awaited rather than cancelled.
        self._timer_emitting = True
        self._timer = None
        try:
            await self._flush()
        finally:
            self._timer_emitting = False

    async def _flush(self) -> None:
        async with self._lock:
            if not self._parts:
                return
            text = "".join(self._parts)
            self._parts.clear()
            self._size = 0
            self._last_flush = self._clock()
            self.emitted += 1
            await self._emit(text)

    async def _settle_timer(self) -> None:
        timer, self._timer = self._timer, None
        if timer is None or timer is asyncio.current_task():
            return
        if not self._timer_emitting:
            # Not started, or still waiting out its delay: it owns no text
            # yet, so it can simply go.
            timer.cancel()
        # Past the sleep and inside an emit, cancelling would drop the text it
        # took out of the buffer — so in that case it is left to finish.
        try:
            await timer
        except asyncio.CancelledError:
            pass

    async def close(self) -> None:
        """Emit whatever is buffered. Call before the final marker."""
        await self._settle_timer()
        await self._flush()

    async def discard(self) -> None:
        """Drop whatever is buffered without emitting it — for a reply that
        was cancelled, where the kernel would suppress the text anyway."""
        await self._settle_timer()
        self._parts.clear()
        self._size = 0
