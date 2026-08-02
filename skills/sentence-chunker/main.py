"""
sentence-chunker — logical.text.sentence_chunk

Sits between a producer that streams token by token and a consumer that works
in whole utterances. Without it, wiring an LLM straight into a speech
synthesiser fires one full synthesis per token: `llm-chat` emits an envelope
per token, and TTS speaks anything non-empty it is handed.

It is a skill rather than logic inside TTS because the shape is general — any
per-token producer feeding any per-utterance consumer wants it, whether that
consumer speaks, translates, moderates or logs. It also keeps TTS a pure
driver, and makes the flush policy hot-tunable config instead of constants
buried in a synthesiser.

The first clause is flushed aggressively and later ones are not, on purpose.
Time-to-first-sound is what a conversation is judged on, so the opening clause
goes out as soon as it is a plausible phrase; after that the listener is
already hearing something, and longer spans give the synthesiser the context
it needs for natural prosody.

One limit worth knowing: flushes are driven by arriving tokens, so
`max_wait_ms` is evaluated when the *next* token lands. A producer that stops
dead without sending `final` leaves its last few words unspoken. Every
first-party producer terminates its stream properly; a custom one must too.
"""
import logging
import re
import time

from aura import Context, Skill, run_all

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("sentence-chunker")

skill = Skill()

# Splitting after these would cut a title or a decimal in half: "Dr. Chandra",
# "3.5 seconds". Cheap to check, and the alternative is an NLP dependency for
# a problem that is mostly this list.
_NO_SPLIT_BEFORE = re.compile(r"(?:\b(?:[A-Z][a-z]{0,3}|vs|etc|approx)\.|\d\.)$")


class Buffer:
    """One producer's in-progress text, plus when it last said anything."""

    def __init__(self) -> None:
        self.text = ""
        self.last_token = time.monotonic()
        self.clauses_sent = 0


_buffers: dict[tuple[str, str], Buffer] = {}


def _sweep() -> None:
    """The SDK has no session-end hook, so a buffer whose producer vanished
    mid-sentence has to age out or it is kept forever."""
    cutoff = time.monotonic() - 300
    for key, buf in list(_buffers.items()):
        if buf.last_token < cutoff:
            _buffers.pop(key, None)


def _split_point(text: str, boundaries: str, min_chars: int, earliest: bool) -> int:
    """Index just past a usable boundary, or -1.

    Direction is the whole trick. `earliest` takes the first opportunity —
    lowest latency, for the opening clause nobody has heard yet. Otherwise it
    takes the last, so a flush carries everything that is ready: fewer
    synthesis calls and a longer span for the synthesiser to get prosody from.
    Scanning backwards for the *first* clause would wait for the end of the
    sentence and throw away the only latency that is being optimised.
    """
    positions = (range(min_chars - 1, len(text)) if earliest
                 else range(len(text) - 1, min_chars - 2, -1))
    for i in positions:
        if text[i] in boundaries and not _NO_SPLIT_BEFORE.search(text[: i + 1]):
            return i + 1
    return -1


def _ready(buf: Buffer, cfg: dict) -> int:
    """How much of the buffer to flush now; 0 for nothing yet."""
    text = buf.text
    if not text.strip():
        return 0

    first = buf.clauses_sent == 0
    min_chars = cfg["first_clause_chars"] if first else cfg["min_chars"]

    cut = _split_point(text, cfg["boundary_chars"], min_chars, earliest=first)
    if cut > 0:
        return cut

    # No boundary in sight. Two escape hatches, so a producer that writes
    # long unpunctuated prose still gets spoken.
    if len(text) >= cfg["max_chars"]:
        space = text.rfind(" ", min_chars)
        return space + 1 if space > 0 else len(text)
    # The producer has gone quiet. Say what is there, however short — the
    # length floors exist to stop chopping a live stream into fragments, and
    # there is no live stream left to protect. Holding it back would just
    # leave those words unspoken until the reply ends.
    if (time.monotonic() - buf.last_token) * 1000 >= cfg["max_wait_ms"]:
        return len(text)
    return 0


@skill.on("text_in")
async def handle(ctx: Context) -> None:
    payload = ctx.payload or {}
    text = payload.get("text") or ""
    final = bool(payload.get("final"))

    key = (ctx.session, ctx.node)
    # Mutate before the first await: the SDK runs one task per envelope and
    # they reach their first suspension in arrival order, which is what keeps
    # tokens in the order they were written.
    buf = _buffers.get(key)
    if buf is None:
        buf = Buffer()
        _buffers[key] = buf
    buf.text += text
    buf.last_token = time.monotonic()

    if final:
        # End of the reply: flush whatever is left, however short, and pass
        # `final` on so the consumer knows the stream closed.
        remaining = buf.text.strip()
        _buffers.pop(key, None)
        _sweep()
        if remaining:
            await ctx.emit("text_out", {"text": remaining, "final": True})
        else:
            await ctx.emit("text_out", {"text": "", "final": True})
        return

    cfg = skill.config
    while (cut := _ready(buf, cfg)) > 0:
        clause, buf.text = buf.text[:cut].strip(), buf.text[cut:]
        buf.clauses_sent += 1
        if clause:
            await ctx.emit("text_out", {"text": clause, "final": False})
        if ctx.cancelled:
            _buffers.pop(key, None)
            return


if __name__ == "__main__":
    run_all([skill])
