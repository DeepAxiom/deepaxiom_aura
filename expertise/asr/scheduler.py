"""
asr.scheduler — a priority gate in front of the one shared model.

Every session decodes on the same model object. Without a gate, N sessions
mean N worker threads calling `model.transcribe` at once: no ordering, no
fairness, and a final transcript (which a person is waiting on, and which an
LLM is blocked behind) queues behind other sessions' partials (which nobody
will miss).

The gate is deliberately small:

  * one decode at a time;
  * finals before partials, FIFO within a priority;
  * a partial that is superseded (a newer request, or the final, for the same
    utterance key) or cancelled while it waits is SKIPPED, never run;
  * at most `max_waiting_partials` partials wait — beyond that a new one is
    shed at the door, because each waiter holds a thread of asyncio's default
    executor and a stale partial is worth nothing.

Finals are never skipped, not even when their stop event is set: the caller
decides what a cancelled final means (today: the decode function returns ""
and the handler still emits), and skipping here would change that.

It is a ticket lock, not a queue with its own worker: `run` blocks the calling
thread until its turn and then runs the function on that same thread. That
keeps exceptions, return values and thread-affinity trivial, and it means
there is no worker to shut down — when the last caller returns, nothing is
left.

Limits, stated plainly: priority applies only to decodes that have not
started. A running partial is not pre-empted when a final arrives; the final
waits for it (bounded by one partial decode). No aging either: a steady stream
of finals starves partials, which is the intended trade.

No model, no numpy, no asyncio in here — pure `threading`, unit tested with
plain functions.
"""
from __future__ import annotations

import itertools
import threading
from typing import Any, Callable, Hashable

FINAL = 0
PARTIAL = 1


class DecodeSkipped(Exception):
    """The decode did not run (superseded, cancelled or shed), or was
    abandoned by its own function on a stop event. Not an error: there is
    simply no result to show."""


class _Ticket:
    __slots__ = ("priority", "order", "key", "stop", "skip")

    def __init__(self, priority: int, order: int, key: Hashable,
                 stop: threading.Event | None) -> None:
        self.priority = priority
        self.order = order
        self.key = key
        self.stop = stop
        self.skip: str | None = None

    def dead(self) -> bool:
        """A waiter that will leave without running, whether or not its
        thread has woken up to notice yet."""
        if self.skip is not None:
            return True
        return (self.priority == PARTIAL and self.stop is not None
                and self.stop.is_set())


class DecodeGate:
    def __init__(self, max_waiting_partials: int = 4, poll_s: float = 0.05) -> None:
        self._cond = threading.Condition()
        self._waiting: list[_Ticket] = []
        self._busy = False
        self._order = itertools.count()
        self._max_waiting_partials = max_waiting_partials
        # A stop event cannot notify our condition, so a waiting partial that
        # carries one re-checks it this often. Finals and partials without a
        # stop event sleep until notified.
        self._poll_s = poll_s
        self.queued = 0   # tickets that ever entered the line
        self.ran = 0
        self.skipped = 0

    # ── introspection (tests, logs) ───────────────────────────────────────

    def waiting(self) -> int:
        with self._cond:
            return sum(1 for t in self._waiting if not t.dead())

    def busy(self) -> bool:
        with self._cond:
            return self._busy

    # ── the gate ──────────────────────────────────────────────────────────

    def _head(self) -> _Ticket | None:
        live = [t for t in self._waiting if not t.dead()]
        if not live:
            return None
        return min(live, key=lambda t: (t.priority, t.order))

    def _skip(self, reason: str) -> DecodeSkipped:
        self.skipped += 1
        return DecodeSkipped(reason)

    def run(self, fn: Callable[..., Any], args: tuple = (),
            priority: int = FINAL, key: Hashable = None,
            stop: threading.Event | None = None) -> Any:
        """Wait for the model, then return `fn(*args)`.

        Raises DecodeSkipped if a PARTIAL never got to start. Whatever `fn`
        raises propagates, after the gate has been released.
        """
        with self._cond:
            ticket = _Ticket(priority, next(self._order), key, stop)
            if priority == PARTIAL and ticket.dead():
                raise self._skip("cancelled before it was queued")
            if key is not None:
                # Anything this utterance still has queued describes older
                # audio than what is being asked for now.
                for other in self._waiting:
                    if other.priority == PARTIAL and other.key == key and other.skip is None:
                        other.skip = ("superseded by the final" if priority == FINAL
                                      else "superseded by a newer partial")
            if priority == PARTIAL:
                queued = sum(1 for t in self._waiting
                             if t.priority == PARTIAL and not t.dead())
                if queued >= self._max_waiting_partials:
                    self._cond.notify_all()
                    raise self._skip("shed: too many partials already waiting")
            self._waiting.append(ticket)
            self.queued += 1
            self._cond.notify_all()  # wake the superseded so they can leave
            acquired = False
            try:
                while True:
                    if ticket.skip is not None:
                        raise self._skip(ticket.skip)
                    if ticket.dead():
                        raise self._skip("cancelled while waiting")
                    if not self._busy and self._head() is ticket:
                        break
                    poll = (self._poll_s
                            if priority == PARTIAL and stop is not None else None)
                    self._cond.wait(poll)
                self._busy = True
                acquired = True
            finally:
                # Runs on every way out of the wait — turn taken, skipped, or
                # an exception injected into this thread — so a ticket can
                # never be left at the head of the line with nobody behind it.
                self._waiting.remove(ticket)
                if not acquired:
                    self._cond.notify_all()
        try:
            self.ran += 1
            return fn(*args)
        finally:
            with self._cond:
                self._busy = False
                self._cond.notify_all()
