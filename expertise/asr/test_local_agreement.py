"""Tests for the LocalAgreement policy (streaming.py), without a model.

Everything here runs against a SCRIPTED decoder: either a fixed list of
hypotheses, or an "oracle" that knows the true words and their times and
answers the way a recogniser does — right about what it has fully heard,
wrong about the word it has heard half of. That is the whole point of keeping
the policy model-free, and also its limit: these tests prove the policy does
what it says given hypotheses, not that whisper produces hypotheses it works
well with.

"Audio" in these tests is whatever is convenient — the policy never looks
inside it. Most tests pass a (start, end) tuple in seconds.

    cd expertise/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import unittest

from streaming import (Hypothesis, StreamingDecoder, Word, normalize, render,
                       strip_repeats)


def Bare(*args, **kwargs):
    """A decoder that keeps NO committed audio as context: the window starts
    right behind the last committed word. Not how the skill runs it (see
    `Context` below, and streaming.py for why), but it is what lets a script
    of window-relative word times be read without doing arithmetic."""
    kwargs.setdefault("context_s", 0.0)
    return StreamingDecoder(*args, **kwargs)


def words(spec, step=0.5, start=0.0):
    """"a b c" -> evenly spaced Words, `step` seconds each, from `start`."""
    out = []
    for i, text in enumerate(spec.split()):
        out.append(Word(text, start + i * step, start + (i + 1) * step))
    return out


class Script:
    """A decoder that returns the next scripted hypothesis on each call and
    records what it was asked."""

    def __init__(self, *hypotheses):
        self.hypotheses = list(hypotheses)
        self.calls = []

    def __call__(self, audio, prompt):
        self.calls.append((audio, prompt))
        return self.hypotheses.pop(0)


TRUTH = [
    ("Please", 0.0, 0.4), ("turn", 0.5, 0.8), ("on", 0.8, 1.0),
    ("the", 1.0, 1.2), ("kitchen", 1.2, 1.8), ("light", 1.8, 2.3),
    ("and", 2.6, 2.8), ("set", 2.8, 3.1), ("the", 3.1, 3.3),
    ("thermostat", 3.3, 4.2), ("to", 4.2, 4.4), ("twenty", 4.4, 4.9),
    ("one", 4.9, 5.2), ("degrees.", 5.2, 5.9),
]


class Oracle:
    """Answers for a window (start, end) of TRUTH: every word fully inside it,
    correctly, plus a mangled guess for a word the window cuts through."""

    def __init__(self, truth=TRUTH):
        self.truth = truth
        self.calls = []

    def __call__(self, audio, prompt):
        a, b = audio
        self.calls.append((a, b, prompt))
        out = []
        for text, s, e in self.truth:
            if s < a - 0.05:
                continue
            if e <= b:
                out.append(Word(text, s - a, e - a))
            elif s < b:
                out.append(Word("~" + text[:2], s - a, b - a))
        return out


def stream(dec, decode, total, step=0.9, first=1.0):
    """Drive `dec` the way the skill does: a partial every `step` seconds of
    new audio, then the final. Returns (hypotheses, final text)."""
    hyps = []
    t = first
    while t < total:
        hyps.append(dec.update((dec.window_start, t), t - dec.window_start, decode))
        t += step
    final = dec.finish((dec.window_start, total), total - dec.window_start, decode)
    return hyps, final


class Agreement(unittest.TestCase):
    def test_one_hypothesis_commits_nothing(self):
        """There is nothing to agree with yet: it is all volatile."""
        dec = Bare(Script(words("turn on the")))
        hyp = dec.update("audio", 1.5)

        self.assertEqual(hyp.text, "turn on the")
        self.assertEqual(hyp.stable_chars, 0)
        self.assertEqual(hyp.committed, "")
        self.assertEqual(dec.window_start, 0.0)

    def test_the_common_prefix_of_two_hypotheses_is_committed(self):
        dec = Bare(Script(
            words("turn on the kitten"),
            words("turn on the kitchen light")))
        dec.update("a", 2.0)
        hyp = dec.update("a", 2.5)

        self.assertEqual(hyp.committed, "turn on the")
        self.assertEqual(hyp.volatile, "kitchen light")
        self.assertEqual(hyp.text, "turn on the kitchen light")
        self.assertEqual(hyp.text[:hyp.stable_chars], "turn on the")
        self.assertEqual(hyp.newly_committed, 3)

    def test_a_disagreement_on_the_first_word_commits_nothing(self):
        dec = Bare(Script(words("tern de"), words("turn on the")))
        dec.update("a", 1.0)
        hyp = dec.update("a", 1.5)

        self.assertEqual(hyp.stable_chars, 0)
        self.assertEqual(hyp.text, "turn on the")

    def test_case_and_punctuation_do_not_block_agreement(self):
        """whisper re-punctuates constantly; the word did not change."""
        dec = Bare(Script(
            words("hello world how"),
            words("Hello, world! How are")))
        dec.update("a", 1.5)
        hyp = dec.update("a", 2.0)

        # Committed in the form of the hypothesis that confirmed it.
        self.assertEqual(hyp.committed, "Hello, world! How")

    def test_accents_are_part_of_the_word(self):
        self.assertNotEqual(normalize("más"), normalize("mas"))
        self.assertEqual(normalize("¿Qué?"), normalize("qué"))
        dec = Bare(Script(words("quiero mas"), words("quiero más")))
        dec.update("a", 1.0)
        self.assertEqual(dec.update("a", 1.0).committed, "quiero")

    def test_three_way_agreement_needs_three_hypotheses(self):
        dec = Bare(
            Script(words("a b c"), words("a b x"), words("a b y z")), agreement=3)
        self.assertEqual(dec.update("a", 1.5).stable_chars, 0)
        self.assertEqual(dec.update("a", 1.5).stable_chars, 0)
        self.assertEqual(dec.update("a", 2.0).committed, "a b")

    def test_agreement_below_two_is_refused(self):
        with self.assertRaises(ValueError):
            Bare(agreement=1)

    def test_committed_text_never_changes_again(self):
        """Later hypotheses disagreeing with a committed word cannot touch
        it: they are not even compared against it any more."""
        dec = Bare(Script(
            words("turn on the"),
            words("turn on the kitchen"),
            # The window now starts after "the"; whatever comes back is new.
            [Word("chicken", 0.0, 0.5), Word("light", 0.5, 1.0)]))
        dec.update("a", 1.5)
        dec.update("a", 2.0)
        hyp = dec.update("a", 1.0)

        self.assertTrue(hyp.text.startswith("turn on the"))
        self.assertEqual(hyp.committed, "turn on the")
        self.assertEqual(hyp.volatile, "chicken light")


class WindowAndPrompt(unittest.TestCase):
    def test_the_window_moves_to_the_end_of_the_last_committed_word(self):
        dec = Bare(Script(words("a b c"), words("a b d")))
        dec.update("a", 1.5)
        dec.update("a", 1.5)

        self.assertEqual(dec.committed_text, "a b")
        self.assertAlmostEqual(dec.window_start, 1.0)

    def test_word_times_from_a_moved_window_are_utterance_times(self):
        dec = Bare(Script(
            words("a b c"), words("a b c"),
            [Word("d", 0.2, 0.6)], [Word("d", 0.2, 0.6), Word("e", 0.6, 1.0)]))
        dec.update("a", 1.5)
        dec.update("a", 1.5)          # commits a b c, window at 1.5
        dec.update("a", 0.7)
        dec.update("a", 1.1)          # commits d, timed 1.7..2.1 in the utterance

        self.assertEqual(dec.committed_text, "a b c d")
        self.assertAlmostEqual(dec.committed[-1].start, 1.7)
        self.assertAlmostEqual(dec.window_start, 2.1)


    def test_the_committed_text_is_passed_as_the_prompt(self):
        script = Script(words("turn on"), words("turn on the"), [], [])
        dec = Bare(script)
        dec.update("a", 1.0)
        dec.update("a", 1.5)
        dec.update("a", 0.5)

        self.assertEqual([p for _, p in script.calls], ["", "", "turn on"])

    def test_the_prompt_is_a_bounded_tail_cut_on_a_word_boundary(self):
        long = " ".join(f"word{i}" for i in range(100))
        dec = Bare(Script(words(long), words(long)), prompt_chars=40,
                               max_uncommitted_s=1000)
        dec.update("a", 50.0)
        dec.update("a", 50.0)

        prompt = dec.prompt()
        self.assertLessEqual(len(prompt), 40)
        self.assertTrue(prompt.endswith("word99"))
        self.assertTrue(prompt.startswith("word"))  # no half word at the cut

    def test_nothing_committed_means_no_prompt(self):
        self.assertEqual(Bare().prompt(), "")


class Context(unittest.TestCase):
    """The window keeps some committed audio in front of the uncommitted
    part, because a real model decodes a window that starts flush against the
    next word badly (measured; see streaming.py)."""

    def _committed_abcd(self, context_s, *more):
        # a 0-0.5, b 0.5-1, c 1-1.5, d 1.5-2, then a disagreement.
        dec = StreamingDecoder(
            Script(words("a b c d x"), words("a b c d y"), *more), context_s=context_s)
        dec.update("w", 2.5)
        dec.update("w", 2.5)
        self.assertEqual(dec.committed_text, "a b c d")
        return dec

    def test_the_window_starts_on_a_word_boundary_at_least_context_back(self):
        self.assertAlmostEqual(self._committed_abcd(0.0).window_start, 2.0)
        self.assertAlmostEqual(self._committed_abcd(0.5).window_start, 1.5)
        self.assertAlmostEqual(self._committed_abcd(0.7).window_start, 1.0)   # not 1.3
        self.assertAlmostEqual(self._committed_abcd(1.0).window_start, 1.0)
        self.assertAlmostEqual(self._committed_abcd(1.9).window_start, 0.0)

    def test_the_default_keeps_context(self):
        dec = StreamingDecoder(Script(words("a b c d x"), words("a b c d y")))
        dec.update("w", 2.5)
        dec.update("w", 2.5)
        self.assertLess(dec.window_start, 2.0 - 0.5)

    def test_the_window_never_moves_backwards(self):
        dec = self._committed_abcd(
            0.5, [Word("d", 0.0, 0.5), Word("e", 0.5, 0.6)],
            [Word("d", 0.0, 0.5), Word("e", 0.5, 0.6)])
        self.assertAlmostEqual(dec.window_start, 1.5)
        dec.update("w", 0.7)
        dec.update("w", 0.7)             # commits a 0.1 s word at 2.0-2.1
        self.assertEqual(dec.committed_text, "a b c d e")
        self.assertGreaterEqual(dec.window_start, 1.5)

    def test_committed_words_heard_again_in_the_context_are_not_duplicated(self):
        dec = self._committed_abcd(
            1.0, [Word("c", 0.0, 0.5), Word("d", 0.5, 1.0), Word("e", 1.0, 1.5)])
        hyp = dec.update("w", 1.6)
        self.assertEqual(hyp.text, "a b c d e")
        self.assertEqual(hyp.volatile, "e")

    def test_a_different_reading_of_the_context_cannot_rewrite_committed_text(self):
        """The model hears the committed second again and words it
        differently this time. Committed is committed: what lies before the
        commit point is dropped by time, whatever it says."""
        dec = self._committed_abcd(
            1.0, [Word("sea", 0.0, 0.5), Word("dee", 0.5, 1.0), Word("e", 1.0, 1.5)])
        hyp = dec.update("w", 1.6)
        self.assertEqual(hyp.text, "a b c d e")

    def test_a_word_straddling_the_commit_point_is_judged_by_its_middle(self):
        dec = self._committed_abcd(
            1.0, [Word("cd", 0.0, 1.05), Word("e", 0.95, 1.5)])
        self.assertEqual(dec.update("w", 1.6).volatile, "e")

    def test_the_prompt_stops_where_the_window_begins(self):
        """Words the model is about to hear again are not in the prompt: a
        prompt that already contains them invites it to skip them."""
        script = Script(words("a b c d x"), words("a b c d y"), [])
        dec = StreamingDecoder(script, context_s=1.0)
        dec.update("w", 2.5)
        dec.update("w", 2.5)
        dec.update("w", 1.6)
        self.assertEqual([p for _, p in script.calls], ["", "", "a b"])

    def test_no_prompt_while_the_window_still_starts_at_zero(self):
        script = Script(words("a b x"), words("a b y"), [])
        dec = StreamingDecoder(script, context_s=5.0)
        dec.update("w", 1.5)
        dec.update("w", 1.5)
        dec.update("w", 1.5)
        self.assertEqual(dec.window_start, 0.0)
        self.assertEqual(script.calls[-1][1], "")

    def test_the_cap_counts_uncommitted_audio_not_the_context(self):
        """Ten seconds of context plus two uncommitted is not a stuck
        window."""
        nine = " ".join(f"w{i}" for i in range(18))        # 9 s of words
        dec = StreamingDecoder(
            Script(words("w0 w1"), words("w0 w1"), words(nine), words(nine),
                   words(nine + " w18 w19")),
            context_s=10.0, max_uncommitted_s=10.0)
        dec.update("w", 1.0)
        dec.update("w", 1.0)
        dec.update("w", 9.0)
        dec.update("w", 9.0)
        self.assertEqual(len(dec.committed), 18)
        self.assertEqual(dec.window_start, 0.0)            # all of it is context
        hyp = dec.update("w", 11.5)                        # an 11.5 s window ...

        self.assertEqual(dec.forced_commits, 0)            # ... 2.5 s uncommitted
        self.assertEqual(hyp.volatile, "w18 w19")


class EndToEnd(unittest.TestCase):
    """The oracle against the decoder as the skill configures it (default
    context). The oracle answers for whatever window it is given, so it also
    returns the committed words inside the context — every round exercises
    the de-duplication."""

    def test_final_is_the_truth_and_decodes_only_the_tail(self):
        oracle = Oracle()
        dec = StreamingDecoder()
        hyps, final = stream(dec, oracle, total=6.0)

        self.assertEqual(
            final, "Please turn on the kitchen light and set the thermostat "
                   "to twenty one degrees.")
        # The latency win, in the only currency a scripted decoder has: the
        # final decoded a short tail (plus context), not six seconds.
        a, b, _ = oracle.calls[-1]
        self.assertLess(b - a, 3.0)
        self.assertGreater(a, 3.0)

    def test_the_same_holds_with_no_context_and_with_a_lot(self):
        for context in (0.0, 0.5, 2.0, 30.0):
            with self.subTest(context=context):
                hyps, final = stream(StreamingDecoder(context_s=context), Oracle(), total=6.0)
                self.assertEqual(
                    final, "Please turn on the kitchen light and set the "
                           "thermostat to twenty one degrees.")
                for h in hyps:
                    self.assertTrue(final.startswith(h.text[:h.stable_chars]))

    def test_the_window_stops_growing_once_words_commit(self):
        oracle = Oracle()
        dec = StreamingDecoder()
        stream(dec, oracle, total=6.0)

        spans = [b - a for a, b, _ in oracle.calls]
        self.assertLess(max(spans), 4.0)   # redecode would reach 6.0
        self.assertEqual(oracle.calls[0][0], 0.0)
        self.assertGreater(oracle.calls[-1][0], oracle.calls[1][0])

    def test_stable_prefix_holds_for_every_later_hypothesis_and_the_final(self):
        """The promise made on the wire by `stable_chars`."""
        dec = StreamingDecoder()
        hyps, final = stream(dec, Oracle(), total=6.0)

        self.assertTrue(any(h.stable_chars for h in hyps))
        for i, h in enumerate(hyps):
            stable = h.text[:h.stable_chars]
            self.assertEqual(stable, h.committed)
            for later in hyps[i:]:
                self.assertTrue(later.text.startswith(stable), (stable, later.text))
            self.assertTrue(final.startswith(stable), (stable, final))
        # ... and it only ever grows.
        sizes = [h.stable_chars for h in hyps]
        self.assertEqual(sizes, sorted(sizes))

    def test_the_half_heard_word_is_never_committed(self):
        dec = Bare()
        hyps, final = stream(dec, Oracle(), total=6.0)

        self.assertTrue(any("~" in h.volatile for h in hyps))
        for h in hyps:
            self.assertNotIn("~", h.committed)
        self.assertNotIn("~", final)

    def test_prompts_follow_the_committed_text(self):
        oracle = Oracle()
        dec = Bare()
        stream(dec, oracle, total=6.0)

        prompts = [p for _, _, p in oracle.calls]
        self.assertEqual(prompts[0], "")
        self.assertTrue(prompts[-1].startswith("Please turn on"))


class Finish(unittest.TestCase):
    def test_finish_accepts_the_tail_without_agreement(self):
        script = Script(words("a b"), words("a b c"), [Word("c", 0.0, 0.5), Word("d", 0.5, 0.9)])
        dec = Bare(script)
        dec.update("a", 1.0)
        dec.update("a", 1.5)
        self.assertEqual(dec.finish("tail", 1.0), "a b c d")
        self.assertEqual(len(script.calls), 3)

    def test_finish_with_no_audio_left_does_not_decode(self):
        script = Script(words("a b"), words("a b"))
        dec = Bare(script)
        dec.update("a", 1.0)
        dec.update("a", 1.0)
        self.assertEqual(dec.finish("", 0.0), "a b")
        self.assertEqual(len(script.calls), 2)

    def test_finish_without_any_partial_is_one_plain_decode(self):
        """A short utterance, or partials disabled: the final is one decode
        of everything, which is what redecode does."""
        script = Script(words("turn on the light"))
        dec = Bare(script)
        self.assertEqual(dec.finish("all", 2.0), "turn on the light")
        self.assertEqual(script.calls, [("all", "")])

    def test_finish_can_use_a_different_decoder(self):
        """The skill decodes the final with a wider beam than partials."""
        partial = Script(words("a b"), words("a b"))
        final = Script([Word("c", 0.0, 0.4)])
        dec = Bare(partial)
        dec.update("a", 1.0)
        dec.update("a", 1.0)
        self.assertEqual(dec.finish("tail", 0.5, final), "a b c")
        self.assertEqual(len(final.calls), 1)

    def test_an_empty_final_decode_keeps_what_was_committed(self):
        dec = Bare(Script(words("a b"), words("a b x"), []))
        dec.update("a", 1.0)
        dec.update("a", 1.5)
        self.assertEqual(dec.finish("tail", 0.5), "a b")


class Echo(unittest.TestCase):
    def _committed(self, *more):
        dec = Bare(Script(words("turn on the"), words("turn on the"), *more))
        dec.update("a", 1.5)
        dec.update("a", 1.5)
        self.assertEqual(dec.committed_text, "turn on the")
        return dec

    def test_a_repeated_prompt_is_not_transcribed_twice(self):
        """Given its own words as a prompt, the model sometimes says them
        again before it gets to the audio."""
        dec = self._committed(
            [Word("on", 0.0, 0.1), Word("the", 0.1, 0.2),
             Word("kitchen", 0.2, 0.8), Word("light", 0.8, 1.2)])
        hyp = dec.update("a", 1.3)
        self.assertEqual(hyp.text, "turn on the kitchen light")

    def test_the_longest_echo_wins(self):
        dec = self._committed(
            [Word("Turn", 0.0, 0.1), Word("on", 0.1, 0.2), Word("the", 0.2, 0.3),
             Word("kitchen", 0.3, 0.8)])
        self.assertEqual(dec.update("a", 1.0).text, "turn on the kitchen")


    def test_a_far_later_repetition_is_not_an_echo(self):
        """Same word again after more than a second of something else is the
        speaker repeating it, not the model."""
        dec = self._committed([Word("the", 1.4, 1.6), Word("end", 1.6, 2.0)])
        self.assertEqual(dec.update("a", 2.2).text, "turn on the the end")


class Hallucination(unittest.TestCase):
    def test_a_looping_phrase_is_cut_to_one_copy(self):
        loop = words("so thank you " + "thank you " * 9)
        self.assertEqual(render(strip_repeats(loop)), "so thank you")

    def test_a_looping_word_is_cut_to_one_copy(self):
        self.assertEqual(render(strip_repeats(words("yes " + "no " * 8 + "maybe"))), "yes no")

    def test_ordinary_repetition_survives(self):
        said = words("no no no I said no")
        self.assertEqual(strip_repeats(said), said)
        self.assertEqual(strip_repeats([]), [])

    def test_a_loop_differing_only_in_punctuation_is_still_a_loop(self):
        loop = words("Okay. okay, Okay okay! okay")
        self.assertEqual(render(strip_repeats(loop)), "Okay.")

    def test_a_loop_cannot_be_committed_through_agreement(self):
        """Two hypotheses that both loop DO agree with each other. The loop
        has to be cut before the comparison, or it would be committed."""
        looping = "hello " + "thank you " * 10
        dec = Bare(Script(words(looping, step=0.1), words(looping, step=0.1)))
        dec.update("a", 3.0)
        hyp = dec.update("a", 3.0)
        self.assertEqual(hyp.text, "hello thank you")

    def test_words_timed_after_the_audio_ends_are_dropped(self):
        """The model finishing the sentence on its own: there is no sound
        under those words."""
        dec = Bare(Script(
            [Word("turn", 0.0, 0.4), Word("on", 0.4, 0.8),
             Word("the", 1.3, 1.5), Word("radio", 1.5, 2.0)]))
        self.assertEqual(dec.update("a", 1.0).text, "turn on")

    def test_a_run_of_words_with_no_duration_is_cut(self):
        """Verbatim from a real decode of a 1.3 s tail holding one word: the
        aligner found no audio for the invented ones and stamped them all at
        the same instant."""
        dec = Bare(Script(
            [Word("linked", 0.0, 0.24), Word("in", 0.24, 0.6), Word("the", 0.6, 0.6),
             Word("link", 0.6, 0.6), Word("in", 0.6, 0.6), Word("the", 0.6, 0.6),
             Word("video", 0.6, 0.6), Word("description.", 0.6, 0.6)]))
        self.assertEqual(dec.update("a", 1.32).text, "linked in")

    def test_one_word_with_no_duration_is_left_alone(self):
        """Also from a real decode: a clipped "the" at the very start of a
        window, stamped 0.0-0.0, in front of words that were really said."""
        dec = Bare(Script(
            [Word("the", 0.0, 0.0), Word("shopping", 0.0, 0.38), Word("list.", 0.38, 0.8)]))
        self.assertEqual(dec.update("a", 1.7).text, "the shopping list.")

    def test_punctuation_on_its_own_rides_on_the_previous_word(self):
        dec = Bare(Script(
            [Word("...", 0.0, 0.1), Word("well", 0.1, 0.5), Word("-", 0.5, 0.6),
             Word("yes", 0.6, 0.9), Word("  ", 0.9, 1.0)]))
        hyp = dec.update("a", 1.0)
        self.assertEqual(hyp.text, "well - yes")
        self.assertEqual(len(dec.volatile), 2)


class EmptyAndFailing(unittest.TestCase):
    def test_an_empty_decode_is_an_empty_hypothesis_not_an_error(self):
        dec = Bare(Script([], []))
        hyp = dec.update("a", 1.0)
        self.assertEqual(hyp, Hypothesis("", 0, "", "", 0))
        self.assertEqual(dec.update("a", 2.0).text, "")

    def test_an_empty_decode_between_two_equal_ones_resets_agreement(self):
        """Agreement is between CONSECUTIVE hypotheses. Silence in between
        means the first one was not confirmed."""
        dec = Bare(Script(words("a b"), [], words("a b"), words("a b")))
        dec.update("a", 1.0)
        self.assertEqual(dec.update("a", 1.0).text, "")
        self.assertEqual(dec.update("a", 1.0).stable_chars, 0)
        self.assertEqual(dec.update("a", 1.0).committed, "a b")

    def test_an_empty_decode_keeps_committed_text_visible(self):
        dec = Bare(Script(words("a b"), words("a b c"), []))
        dec.update("a", 1.0)
        dec.update("a", 1.5)
        hyp = dec.update("a", 0.5)
        self.assertEqual((hyp.text, hyp.stable_chars), ("a b", 3))

    def test_a_failing_decode_leaves_the_state_untouched(self):
        def boom(audio, prompt):
            raise RuntimeError("model fell over")

        dec = Bare(Script(words("a b c"), words("a b d"), words("a b d e", start=0.0)))
        dec.update("a", 1.5)
        before = (dec.window_start, dec.committed_text, dec.volatile, dec.decodes)
        with self.assertRaises(RuntimeError):
            dec.update("a", 1.5, boom)
        self.assertEqual(
            (dec.window_start, dec.committed_text, dec.volatile, dec.decodes), before)
        # ... and the round can simply be tried again.
        self.assertEqual(dec.update("a", 1.5).committed, "a b")

    def test_no_decoder_at_all_is_a_clear_error(self):
        with self.assertRaises(ValueError):
            Bare().update("a", 1.0)


class Cap(unittest.TestCase):
    def test_when_nothing_agrees_the_older_half_is_committed_anyway(self):
        """A model that words the same audio differently each round would
        otherwise make the window — and the cost — grow without bound."""
        n = [0]

        def fickle(audio, prompt):
            n[0] += 1
            a, b = audio
            # One word per second, never the same word twice in a row.
            return [Word(f"w{int(a) + i}v{n[0]}", i, i + 1.0) for i in range(int(b - a))]

        dec = Bare(max_uncommitted_s=10.0)
        spans = []
        for t in range(1, 40):
            spans.append(t - dec.window_start)
            dec.update((dec.window_start, float(t)), t - dec.window_start, fickle)

        self.assertLessEqual(max(spans), 11.0)
        self.assertGreater(dec.forced_commits, 0)
        self.assertGreater(len(dec.committed), 20)
        # Forced words are committed in order, each second exactly once.
        seconds = [int(w.text[1:].split("v")[0]) for w in dec.committed]
        self.assertEqual(seconds, list(range(len(seconds))))

    def test_a_long_silence_moves_the_window_without_committing(self):
        dec = Bare(max_uncommitted_s=10.0)
        for t in range(1, 30):
            dec.update("silence", t - dec.window_start, lambda a, p: [])

        self.assertEqual(dec.committed_text, "")
        self.assertGreater(dec.window_start, 15.0)
        self.assertLessEqual(29 - dec.window_start, 10.0)

    def test_a_word_straddling_the_cut_is_not_split(self):
        dec = Bare(
            Script([Word("first", 1.0, 2.0), Word("looooong", 4.0, 8.0), Word("x1", 9.0, 9.5)]),
            max_uncommitted_s=10.0)
        hyp = dec.update("a", 11.0)          # cut at 6.0, inside "looooong"

        self.assertEqual(hyp.committed, "first")
        self.assertAlmostEqual(dec.window_start, 2.0)
        self.assertEqual(hyp.volatile, "looooong x1")

    def test_one_endless_word_cannot_pin_the_window(self):
        """A single 'word' covering the whole window is not a word. The
        window moves regardless."""
        dec = Bare(Script([Word("mmmm", 0.0, 12.0)]), max_uncommitted_s=10.0)
        dec.update("a", 12.0)
        self.assertAlmostEqual(dec.window_start, 7.0)

    def test_the_cap_also_applies_after_a_small_commit(self):
        """Agreeing on one early word must not excuse a window that is still
        far too long behind it."""
        early = [Word(f"x{i}", 1.0 + i, 2.0 + i) for i in range(3)]
        later = [Word(f"y{i}", 1.0 + i, 2.0 + i) for i in range(13)]
        dec = Bare(Script([Word("hi", 0.0, 0.5)] + early,
                                      [Word("hi", 0.0, 0.5)] + later),
                               max_uncommitted_s=10.0)
        dec.update("a", 4.0)
        self.assertEqual(dec.forced_commits, 0)
        hyp = dec.update("a", 14.0)          # agrees on "hi" only: 13.5s left
        self.assertLessEqual(14.0 - dec.window_start, 10.0)
        self.assertEqual(dec.forced_commits, 1)
        self.assertTrue(hyp.committed.startswith("hi y0 y1"))


class LateAudio(unittest.TestCase):
    """A chunk that arrives out of order moves every sample after it."""

    def test_an_insert_inside_the_window_restarts_agreement(self):
        dec = Bare(Script(words("a b c"), words("a b c"), words("a b c")))
        dec.update("a", 1.5)
        dec.notify_insert(at_s=0.7, seconds=0.1)
        self.assertEqual(dec.update("a", 1.6).stable_chars, 0)
        self.assertEqual(dec.update("a", 1.6).committed, "a b c")
        self.assertEqual(dec.late_audio_s, 0.0)

    def test_an_insert_before_the_window_shifts_it_and_is_counted(self):
        dec = Bare(Script(words("a b c"), words("a b d")))
        dec.update("a", 1.5)
        dec.update("a", 1.5)                 # window at 1.0
        dec.notify_insert(at_s=0.3, seconds=0.1)

        self.assertAlmostEqual(dec.window_start, 1.1)   # same samples as before
        self.assertAlmostEqual(dec.late_audio_s, 0.1)
        self.assertEqual(dec.committed_text, "a b")

    def test_an_insert_inside_the_context_is_also_lost_audio(self):
        """The context is committed audio that happens to still be in the
        window. Late samples landing there are no more transcribable than
        ones before it — only the window itself does not need to move."""
        dec = StreamingDecoder(Script(words("a b c d x"), words("a b c d y")),
                               context_s=1.0)
        dec.update("w", 2.5)
        dec.update("w", 2.5)                 # committed to 2.0, window at 1.0
        dec.notify_insert(at_s=1.2, seconds=0.2)

        self.assertAlmostEqual(dec.late_audio_s, 0.2)
        self.assertAlmostEqual(dec.window_start, 1.0)
        self.assertAlmostEqual(dec.committed[-1].end, 2.2)   # "d" moved with its samples
        self.assertAlmostEqual(dec.committed[1].end, 1.0)    # "b" did not

    def test_an_utterance_with_a_hole_still_streams(self):
        """A dropped chunk that never arrives splices two moments together.
        The words around the splice may be wrong; the policy must not stall
        or lose what it already committed."""
        holed = [w for w in TRUTH if not 2.4 < w[1] < 3.2]   # "and set the" lost
        shifted = [(t, s if s < 2.4 else s - 0.7, e if s < 2.4 else e - 0.7)
                   for t, s, e in holed]
        dec = Bare()
        hyps, final = stream(dec, Oracle(shifted), total=5.3)
        self.assertEqual(
            final, "Please turn on the kitchen light thermostat to twenty one degrees.")
        self.assertGreater(hyps[-1].stable_chars, 0)


class Rendering(unittest.TestCase):
    def test_words_without_a_leading_space_are_joined_without_one(self):
        """Languages written without spaces: whisper marks the boundary, the
        renderer must not invent one."""
        zh = [Word("你好", 0.0, 0.5, space=False), Word("世界", 0.5, 1.0, space=False)]
        self.assertEqual(render(zh), "你好世界")
        self.assertEqual(render(words("a b")), "a b")
        self.assertEqual(render([]), "")

    def test_stable_chars_indexes_the_rendered_text(self):
        zh = [Word("你好", 0.0, 0.5, space=False), Word("世界", 0.5, 1.0, space=False)]
        dec = Bare(Script(zh[:1] + [Word("是", 0.5, 0.6, space=False)], zh))
        dec.update("a", 0.7)
        hyp = dec.update("a", 1.0)
        self.assertEqual((hyp.text, hyp.stable_chars), ("你好世界", 2))


if __name__ == "__main__":
    unittest.main()
