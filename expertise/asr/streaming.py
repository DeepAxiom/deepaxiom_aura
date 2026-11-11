"""
asr.streaming — incremental decoding with a stable prefix (LocalAgreement-n).

Whisper is not a streaming model: it decodes a buffer, and the only way to
get a hypothesis while someone is still talking is to decode what has arrived
so far. The `redecode` policy does exactly that and nothing else, so every
partial pays for the whole buffer again and any word may change until the
final.

This module is the other policy, from the whisper-streaming literature
(Macháček et al., "Turning Whisper into Real-Time Transcription System",
2023). The idea:

  1. decode only the audio that is not committed yet;
  2. compare with the previous hypothesis for that same stretch; the longest
     word prefix both agree on is COMMITTED and never changes again;
  3. cut the audio buffer at a committed word boundary and hand the committed
     text before the cut to the next decode as its prompt, so the next round
     only pays for the uncommitted tail (plus a little context, below).

The cut is NOT made right behind the last committed word. Measured on a real
model (faster-whisper `base`): a window that begins exactly where the last
committed word ended decodes erratically — the 1.3 s tail holding the single
word "list." came back empty with a prompt and as "linked in the link in the
video description" without one, while the same word decoded correctly from a
window starting 0.4 s earlier. Whisper needs some lead-in. So the window keeps
`context_s` seconds of already-committed audio in front of the uncommitted
part, still starting on a word boundary; what the model re-decodes in that
stretch is discarded by time. The prompt covers only the text BEFORE the
window, never words whose audio the model is about to hear again.

What a consumer sees is `committed + volatile`: a prefix that only grows, and
a tail that may still be rewritten.

`StreamingDecoder` is the policy and nothing else. It never touches a model,
numpy, bytes or a clock: the caller passes the audio of the current window as
an opaque object plus its length in seconds, and a `decode(audio, prompt)`
callable that returns `Word`s timed relative to the start of that audio. That
is what makes it testable with a scripted decoder — and, bluntly, a scripted
decoder is most of what it HAS been tested with; see the note in engine.py.

Known limits, so nobody has to rediscover them:

  * It does not make whisper cheaper on a CPU. Whisper's encoder runs over a
    fixed 30 s window however little audio it is given, so a shorter buffer
    only saves decoder steps, and the word timestamps this policy needs cost
    about as much. In the one real-model run so far (figures in engine.py)
    partials cost the same under both policies. What the policy delivers is
    the stable prefix.
  * Commitment is irreversible. If two consecutive hypotheses agree on a wrong
    word, it stays wrong; `redecode` would have had the whole utterance of
    context — and the wider final beam — to fix it at the final. The same run
    showed exactly that on one of its two clips.
  * Word boundaries come from whisper's word timestamps, which are
    cross-attention estimates, good to roughly a tenth of a second. They
    decide where the window is cut and which re-decoded words count as
    already committed; a bad timestamp can cost or duplicate a word there.
  * The final is only as good as one decode of a short tail. The context
    above makes that decode far better behaved; it does not make it equal to
    decoding the whole utterance.
  * A word spoken twice in a row across a commit boundary ("no | no") can be
    taken for an echo of the committed text and dropped; see `_drop_echo`.
  * Comparison ignores case and punctuation, so the committed text carries the
    punctuation of whichever hypothesis happened to commit it.
"""
from __future__ import annotations

import itertools
import unicodedata
from collections import deque
from dataclasses import dataclass
from typing import Any, Callable, Iterable, Sequence


@dataclass(frozen=True)
class Word:
    """One decoded word. `start`/`end` are seconds; as returned by a decode
    callable they are relative to the start of the audio it was given.

    `space` says whether a space precedes the word when rendering. Whisper
    marks that itself (a leading space on the token), and keeping it is what
    lets languages written without spaces come out without spaces.
    """
    text: str
    start: float
    end: float
    space: bool = True


@dataclass(frozen=True)
class Hypothesis:
    """The state after one decode.

    `text[:stable_chars]` is committed: it will be a prefix of every later
    hypothesis of this utterance and of the final assembled from it.
    """
    text: str
    stable_chars: int
    committed: str
    volatile: str
    newly_committed: int = 0


Decode = Callable[[Any, str], Iterable[Word]]

# Seconds of committed audio kept in front of the uncommitted part. See the
# module docstring for why this is not zero.
DEFAULT_CONTEXT_S = 1.0


def normalize(text: str) -> str:
    """The form two hypotheses are compared in: letters and digits only,
    case-folded. "Hello," and "hello" are the same word said twice; whisper
    changes its mind about capitals and commas far more often than about the
    word. Accents are kept — "mas" and "más" are different words."""
    folded = unicodedata.normalize("NFKC", text).casefold()
    return "".join(ch for ch in folded if ch.isalnum())


def render(words: Sequence[Word]) -> str:
    parts: list[str] = []
    for w in words:
        if parts and w.space:
            parts.append(" ")
        parts.append(w.text)
    return "".join(parts).strip()


def strip_repeats(words: list[Word], max_repeats: int = 3, max_ngram: int = 6) -> list[Word]:
    """Cut a hypothesis where it starts looping.

    Whisper's best-known failure is repeating a phrase until the window runs
    out ("thank you thank you thank you ..."). A phrase of up to `max_ngram`
    words said more than `max_repeats` times back to back is treated as that:
    one copy is kept and everything after it is dropped, because whatever
    follows a loop was decoded from the loop, not from the audio.

    The cost: someone who really does say "no" four times in a row is
    transcribed as saying it once.
    """
    tokens = [normalize(w.text) for w in words]
    for i in range(len(tokens)):
        for n in range(1, max_ngram + 1):
            gram = tokens[i:i + n]
            if len(gram) < n:
                break
            count = 1
            while tokens[i + count * n:i + (count + 1) * n] == gram:
                count += 1
            if count > max_repeats:
                return words[:i + n]
    return words


def _cut_untimed_run(words: list[Word], run: int = 3) -> list[Word]:
    """Cut a hypothesis at the first `run` consecutive words with no duration.

    Seen on a real model: a tail decoded as "linked in the link in the video
    description", every word after the second stamped 0.60-0.60. The aligner
    found no audio for them because there was none. One zero-length word
    happens to real speech (a clipped "the"); three in a row do not.
    """
    streak = 0
    for i, w in enumerate(words):
        streak = streak + 1 if w.end - w.start <= 1e-6 else 0
        if streak >= run:
            return words[:i - run + 1]
    return words


class StreamingDecoder:
    """LocalAgreement-n over one utterance.

    Time is seconds from the start of the utterance. `window_start` is where
    the audio still being decoded begins — up to `context_s` of committed
    audio, then everything uncommitted; the caller passes the audio from there
    to the end on every call.

        dec = StreamingDecoder(decode)
        hyp = dec.update(audio_from(dec.window_start), seconds)   # per partial
        text = dec.finish(audio_from(dec.window_start), seconds)  # once

    Not thread-safe: one call at a time (the skill runs every call under the
    decode gate). If `decode` raises, the state is untouched and the call can
    simply be repeated later.
    """

    def __init__(self, decode: Decode | None = None, *, agreement: int = 2,
                 max_uncommitted_s: float = 10.0, prompt_chars: int = 200,
                 context_s: float = DEFAULT_CONTEXT_S, max_repeats: int = 3) -> None:
        if agreement < 2:
            raise ValueError("agreement needs at least 2 hypotheses to compare")
        self._decode = decode
        self._agreement = agreement
        self._max_uncommitted_s = max_uncommitted_s
        self._prompt_chars = prompt_chars
        self._context_s = max(context_s, 0.0)
        self._max_repeats = max_repeats

        self.committed: list[Word] = []
        self.window_start = 0.0
        self._committed_until = 0.0
        # The last (agreement - 1) hypotheses for the uncommitted stretch,
        # each already stripped of whatever has been committed since.
        self._history: deque[list[Word]] = deque(maxlen=agreement - 1)

        # Counters for logs and tests.
        self.decodes = 0
        self.forced_commits = 0
        self.late_audio_s = 0.0

    # ── read-only views ───────────────────────────────────────────────────

    @property
    def committed_text(self) -> str:
        return render(self.committed)

    @property
    def volatile(self) -> list[Word]:
        return list(self._history[-1]) if self._history else []

    def prompt(self) -> str:
        """The tail of the committed text that lies BEFORE the window, cut on
        a word boundary. Given to the next decode so the model continues the
        sentence instead of starting one — it no longer hears the audio those
        words came from. Committed words still inside the window (the context)
        are left out: the model is about to hear them, and a prompt that
        already contains them invites it to skip ahead."""
        tail: list[Word] = []
        size = 0
        for w in reversed(self.committed):
            if w.end > self.window_start + 1e-6:
                continue
            size += len(w.text) + 1
            if size > self._prompt_chars and tail:
                break
            tail.append(w)
        return render(list(reversed(tail)))

    def hypothesis(self, newly_committed: int = 0) -> Hypothesis:
        committed = self.committed_text
        volatile = self.volatile
        return Hypothesis(
            text=render(self.committed + volatile),
            stable_chars=len(committed),
            committed=committed,
            volatile=render(volatile),
            newly_committed=newly_committed,
        )

    # ── the policy ────────────────────────────────────────────────────────

    def update(self, audio: Any, seconds: float, decode: Decode | None = None) -> Hypothesis:
        """One partial round: decode the window, commit what two (n)
        consecutive hypotheses agree on, move the window."""
        words = self._decode_window(audio, seconds, decode)
        window_end = self.window_start + seconds

        agreed = self._agreed_prefix(words)
        if agreed:
            self._commit(words[:agreed])
        rest = words[agreed:]
        for old in self._history:
            del old[:agreed]
        self._history.append(rest)

        if agreed:
            self._trim_to_committed()
        if window_end - self._committed_until > self._max_uncommitted_s:
            self._force_trim(rest, window_end)
        return self.hypothesis(newly_committed=agreed)

    def finish(self, audio: Any, seconds: float, decode: Decode | None = None) -> str:
        """The settled transcript: what is committed, plus ONE decode of the
        audio that is not. Everything that decode returns is accepted — there
        is no later hypothesis to agree with."""
        words = self._decode_window(audio, seconds, decode) if seconds > 0 else []
        if words:
            self._commit(words)
        self._history.clear()
        self.window_start += max(seconds, 0.0)
        return self.committed_text

    def notify_insert(self, at_s: float, seconds: float) -> None:
        """Audio was inserted into the utterance at `at_s` after the fact — a
        chunk that arrived out of order, filling a gap.

        Either way the previous hypotheses describe a stretch that has since
        changed under them, so they are dropped and agreement starts over.

        If it landed before the commit point, it is in a region whose words
        are already settled: it cannot be transcribed incrementally. Times are
        shifted so the window still points at the same samples, and the loss
        is counted in `late_audio_s` so the caller can decide to re-decode
        everything for the final.
        """
        self._history.clear()
        if at_s >= self._committed_until:
            return
        self.late_audio_s += abs(seconds)
        self._committed_until = max(0.0, self._committed_until + seconds)
        self.committed = [
            Word(w.text, w.start + seconds, w.end + seconds, w.space)
            if w.start >= at_s else w for w in self.committed]
        if at_s < self.window_start:
            self.window_start = max(0.0, self.window_start + seconds)

    # ── internals ─────────────────────────────────────────────────────────

    def _decode_window(self, audio: Any, seconds: float, decode: Decode | None) -> list[Word]:
        fn = decode or self._decode
        if fn is None:
            raise ValueError("no decode callable given")
        raw = list(fn(audio, self.prompt()))  # may raise: nothing mutated yet
        self.decodes += 1
        words = self._absolute(raw, seconds)
        words = self._drop_echo(words)
        return strip_repeats(words, self._max_repeats)

    def _absolute(self, raw: list[Word], seconds: float) -> list[Word]:
        """Window-relative words -> utterance time, minus what cannot be
        compared or cannot be real."""
        out: list[Word] = []
        for w in raw:
            text = w.text.strip()
            if not text:
                continue
            if w.start > seconds:
                # Timed after the end of the audio it was decoded from: the
                # model is continuing the sentence on its own. Nothing after
                # this point is anchored to sound either.
                break
            if not normalize(text):
                # Punctuation on its own ("-", "..."). It has no form to
                # compare, so it rides on the word before it — or is dropped
                # if there is none.
                if out:
                    prev = out[-1]
                    joined = prev.text + (" " if w.space else "") + text
                    out[-1] = Word(joined, prev.start, prev.end, prev.space)
                continue
            out.append(Word(text, self.window_start + w.start,
                            self.window_start + max(w.end, w.start), w.space))
        return _cut_untimed_run(out)

    def _drop_echo(self, words: list[Word]) -> list[Word]:
        """Remove from the head of a hypothesis what is already committed.

        Two ways committed words come back: the window still overlaps them
        (the context it keeps on purpose), or the model repeats its prompt
        before transcribing. The first is caught by time — a word whose
        midpoint lies before the commit point is the committed audio heard
        again, whatever the model made of it this time — the second by text:
        if the hypothesis opens with the same 1-5 words the committed text
        ends with, right at the commit point, they are taken for an echo.

        That second test is a heuristic and it is wrong for a genuine
        immediate repetition across the boundary ("no | no"): the second "no"
        is dropped. whisper-streaming makes the same trade.
        """
        if not self.committed:
            return words
        edge = self._committed_until
        words = [w for w in words if (w.start + w.end) / 2 >= edge - 1e-6]
        if not words or words[0].start - edge > 1.0:
            return words
        tail = [normalize(w.text) for w in self.committed[-5:]]
        head = [normalize(w.text) for w in words[:5]]
        for n in range(min(len(tail), len(head)), 0, -1):
            if tail[-n:] == head[:n]:
                return words[n:]
        return words

    def _agreed_prefix(self, words: list[Word]) -> int:
        if len(self._history) < self._agreement - 1:
            return 0
        n = min([len(words)] + [len(h) for h in self._history])
        for i in range(n):
            token = normalize(words[i].text)
            if any(normalize(h[i].text) != token for h in self._history):
                return i
        return n

    def _commit(self, words: list[Word]) -> None:
        self.committed.extend(words)
        self._committed_until = max(self._committed_until, words[-1].end)

    def _trim_to_committed(self) -> None:
        """Move the window to the latest committed word boundary that still
        leaves `context_s` of committed audio in front of the commit point."""
        limit = self._committed_until - self._context_s + 1e-6
        for w in reversed(self.committed):
            if w.end <= limit:
                self.window_start = max(self.window_start, w.end)
                return

    def _force_trim(self, rest: list[Word], window_end: float) -> None:
        """Too little has agreed for longer than the cap — noise, music, or a
        model that words the same audio differently every time. Without this
        the window grows until the utterance ends and every partial costs
        more than the last.

        The older half of the window is given up on: words of the newest
        hypothesis that end inside it are committed WITHOUT agreement, and
        the window moves past them. Blunt, and it can commit a wrong word;
        the alternative is no bound at all.
        """
        cut = window_end - self._max_uncommitted_s / 2
        forced = list(itertools.takewhile(lambda w: w.end <= cut, rest))
        self.forced_commits += 1
        if forced:
            self._commit(forced)
            self._trim_to_committed()
            remaining = rest[len(forced):]
        else:
            # No whole word in the old half. Move to the start of the word
            # that straddles the cut, or to the cut itself when there is none
            # (silence) or when that would not move at all (one "word"
            # spanning the whole stretch is not a word).
            straddling = [w for w in rest if w.start < cut]
            target = straddling[0].start if straddling else cut
            if target <= self.window_start + 1e-6:
                target = cut
            self.window_start = target
            self._committed_until = max(self._committed_until, target)
            remaining = [w for w in rest if w.start >= target - 1e-6]
        self._history.clear()
        self._history.append(remaining)
