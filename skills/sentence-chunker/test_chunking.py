"""Tests for the clause-splitting rules.

    cd skills/sentence-chunker && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import time
import unittest

import main

CFG = {
    "first_clause_chars": 10,
    "min_chars": 30,
    "max_chars": 240,
    "max_wait_ms": 400,
    "boundary_chars": ".!?;:,…",
}


def buffer(text: str, clauses_sent: int = 0, idle_ms: float = 0) -> main.Buffer:
    buf = main.Buffer()
    buf.text = text
    buf.clauses_sent = clauses_sent
    buf.last_token = time.monotonic() - idle_ms / 1000
    return buf


def flush_all(tokens, cfg=CFG):
    """Feed tokens one at a time and collect what would be emitted."""
    buf = main.Buffer()
    out = []
    for token in tokens:
        buf.text += token
        buf.last_token = time.monotonic()
        while (cut := main._ready(buf, cfg)) > 0:
            clause, buf.text = buf.text[:cut].strip(), buf.text[cut:]
            buf.clauses_sent += 1
            if clause:
                out.append(clause)
    if buf.text.strip():
        out.append(buf.text.strip())
    return out


class FirstClause(unittest.TestCase):
    # Time-to-first-sound is what a conversation is judged on, so the opening
    # clause leaves as soon as it is a plausible phrase.
    def test_first_clause_leaves_at_a_comma(self):
        buf = buffer("Sure thing, let me check that for you")
        self.assertEqual(buf.text[: main._ready(buf, CFG)], "Sure thing,")

    def test_first_clause_waits_until_it_is_worth_saying(self):
        # "Sure," is a boundary but too short to be worth a synthesis round.
        self.assertEqual(main._ready(buffer("Sure,"), CFG), 0)

    def test_later_clauses_wait_for_a_longer_span(self):
        """A short clause mid-reply would sound clipped, and the listener is
        already hearing something so there is no rush."""
        buf = buffer("and then, ", clauses_sent=1)
        self.assertEqual(main._ready(buf, CFG), 0)


class Boundaries(unittest.TestCase):
    def test_later_clauses_take_the_last_boundary_not_the_first(self):
        """Mid-reply a flush should carry everything that is ready. Taking the
        first boundary instead would trickle the answer out in fragments and
        cost a synthesis round for each."""
        buf = buffer("The kitchen light is on now, the hallway one is off, "
                     "and the rest are unchanged", clauses_sent=1)
        cut = main._ready(buf, CFG)
        self.assertEqual(buf.text[:cut],
                         "The kitchen light is on now, the hallway one is off,")

    def test_does_not_split_inside_an_abbreviation(self):
        buf = buffer("Ask Dr. Chandra about the discovery please", clauses_sent=1)
        cut = main._ready(buf, CFG)
        self.assertNotEqual(buf.text[:cut].strip(), "Ask Dr.")

    def test_does_not_split_inside_a_decimal(self):
        buf = buffer("It takes about 3.5 seconds to finish the job", clauses_sent=1)
        cut = main._ready(buf, CFG)
        self.assertFalse(buf.text[:cut].strip().endswith("3."))

    def test_unpunctuated_prose_still_gets_flushed(self):
        """Otherwise a producer that never punctuates is never spoken."""
        long_text = "word " * 60  # 300 chars, no boundary anywhere
        buf = buffer(long_text, clauses_sent=1)
        self.assertGreater(main._ready(buf, CFG), 0)

    def test_a_silent_producer_flushes_what_it_has(self):
        buf = buffer("thinking about it", clauses_sent=1, idle_ms=500)
        self.assertEqual(main._ready(buf, CFG), len(buf.text))

    def test_a_producer_mid_token_is_not_flushed_early(self):
        buf = buffer("thinking about it", clauses_sent=1, idle_ms=50)
        self.assertEqual(main._ready(buf, CFG), 0)


class Streaming(unittest.TestCase):
    def test_a_token_stream_becomes_clauses_not_tokens(self):
        """The whole point: one synthesis per clause, not one per token."""
        reply = ("Sure thing, I can help with that. The kitchen light is now on, "
                 "and tomorrow will be cold and clear.")
        tokens = [c for c in reply]  # worst case: one character at a time

        clauses = flush_all(tokens)

        self.assertLess(len(clauses), 10, f"far too many pieces: {clauses}")
        self.assertGreater(len(clauses), 1, "a streamed reply should not arrive as one blob")
        # Nothing is lost or duplicated in the process.
        self.assertEqual("".join(clauses).replace(" ", ""), reply.replace(" ", ""))

    def test_the_first_clause_arrives_before_the_rest(self):
        reply = "Of course, here is the answer you asked for in full detail."
        tokens = [c for c in reply]
        clauses = flush_all(tokens)
        self.assertEqual(clauses[0], "Of course,")


if __name__ == "__main__":
    unittest.main()
