"""The inference bench, validated against its own mock engine.

Nothing here touches a real engine, and nothing here measures one. What is
checked is that the harness tells the truth about an engine whose behaviour
is known: one that queues first-come-first-served, and one whose priority
reorders the waiting queue without preempting anything.

Timings are asserted as relations with wide margins — "waited for the running
generation", "not slower than baseline by more than 150 ms" — never as
values, so a slow CI machine stretches the run without changing the outcome.

    cd bench/inference && python -m unittest discover -p 'test_*.py' -v
"""
import contextlib
import io
import json
import os
import socket
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import calibrate_tokens  # noqa: E402
import robot_bench  # noqa: E402
from mock_engine import MockEngine  # noqa: E402
from robot_bench import (Endpoint, SSEParser, UsageSupport, percentile,  # noqa: E402
                         stream_request, usage_of)


class Percentiles(unittest.TestCase):
    def test_an_empty_sample_has_no_percentile(self):
        for p in (50, 90, 99, 100):
            self.assertIsNone(percentile([], p))

    def test_one_sample_is_every_percentile(self):
        for p in (1, 50, 90, 99, 100):
            self.assertEqual(percentile([7.5], p), 7.5)

    def test_nearest_rank_on_ten_samples(self):
        values = list(range(1, 11))
        self.assertEqual(percentile(values, 50), 5)
        self.assertEqual(percentile(values, 90), 9)
        self.assertEqual(percentile(values, 91), 10)
        self.assertEqual(percentile(values, 99), 10, "with n < 100, p99 is the maximum")
        self.assertEqual(percentile(values, 100), 10)
        self.assertEqual(percentile(values, 10), 1)
        self.assertEqual(percentile(values, 1), 1)

    def test_nearest_rank_on_larger_samples(self):
        self.assertEqual(percentile(range(1, 101), 99), 99)
        self.assertEqual(percentile(range(1, 101), 50), 50)
        self.assertEqual(percentile(range(1, 201), 99), 198)
        self.assertEqual(percentile(range(1, 1001), 99.9), 999)

    def test_it_never_invents_a_value_between_two_samples(self):
        self.assertEqual(percentile([10, 20], 50), 10)
        self.assertEqual(percentile([10, 20], 51), 20)
        for p in range(1, 101):
            self.assertIn(percentile([3.0, 1.0, 4.0, 1.5, 9.0], p), (3.0, 1.0, 4.0, 1.5, 9.0))

    def test_order_does_not_matter_and_the_input_is_left_alone(self):
        values = [9, 1, 5, 3, 7]
        self.assertEqual(percentile(values, 50), 5)
        self.assertEqual(values, [9, 1, 5, 3, 7])

    def test_a_percentile_outside_the_range_is_an_error(self):
        for p in (0, -1, 100.1):
            with self.assertRaises(ValueError):
                percentile([1, 2, 3], p)


def _chunk(text):
    return json.dumps({"choices": [{"index": 0, "delta": {"content": text}}]})


USAGE_ONLY = json.dumps({"choices": [], "usage": {
    "prompt_tokens": 900, "completion_tokens": 2, "total_tokens": 902,
    "prompt_tokens_details": {"cached_tokens": 768}}})


class EventStream(unittest.TestCase):
    def _all(self, raw: bytes) -> list[str]:
        parser = SSEParser()
        return parser.feed(raw) + parser.close()

    def test_a_stream_that_ends_with_a_usage_only_chunk_and_done(self):
        raw = (f"data: {_chunk('he')}\n\ndata: {_chunk('llo')}\n\n"
               f"data: {USAGE_ONLY}\n\ndata: [DONE]\n\n").encode()
        events = self._all(raw)
        self.assertEqual(len(events), 4)
        self.assertEqual(events[-1], "[DONE]")
        usage_event = json.loads(events[-2])
        self.assertEqual(usage_event["choices"], [])
        self.assertEqual(usage_of(usage_event["usage"]),
                         {"prompt_tokens": 900, "completion_tokens": 2, "cached_tokens": 768})

    def test_bytes_may_arrive_split_anywhere(self):
        text = "caf\u00e9 \u2192 ok"
        # Not ASCII-escaped: the multi-byte characters have to be on the wire
        # for a read to be able to end in the middle of one.
        chunk = json.dumps({"choices": [{"index": 0, "delta": {"content": text}}]},
                           ensure_ascii=False)
        raw = (f"data: {chunk}\r\n\r\n: keep-alive\r\n\r\n"
               f"data: {USAGE_ONLY}\r\n\r\ndata: [DONE]\r\n\r\n").encode("utf-8")
        self.assertIn(text.encode("utf-8"), raw)
        whole = self._all(raw)
        self.assertEqual(len(whole), 3)
        self.assertEqual(json.loads(whole[0])["choices"][0]["delta"]["content"], text)
        for size in (1, 2, 3, 7):
            parser = SSEParser()
            got = []
            for i in range(0, len(raw), size):
                got += parser.feed(raw[i:i + size])
            got += parser.close()
            self.assertEqual(got, whole, f"fed {size} byte(s) at a time")

    def test_a_cr_and_its_lf_in_different_reads_are_one_line_ending(self):
        # Taken for two line endings, the pair would be a blank line, and a
        # blank line ends the event half way through.
        parser = SSEParser()
        got = parser.feed(b"data: a\r")
        got += parser.feed(b"\ndata: b\r\n\r\n")
        self.assertEqual(got, ["a\nb"])

    def test_all_three_line_endings(self):
        self.assertEqual(self._all(b"data: one\n\ndata: two\n\n"), ["one", "two"])
        self.assertEqual(self._all(b"data: one\r\n\r\ndata: two\r\n\r\n"), ["one", "two"])
        self.assertEqual(self._all(b"data: one\r\rdata: two\r\r"), ["one", "two"])

    def test_comments_and_other_fields_are_not_data(self):
        raw = b": ping\n\nevent: message\nid: 7\nretry: 100\ndata: x\n\n:\n\n"
        self.assertEqual(self._all(raw), ["x"])

    def test_the_space_after_the_colon_is_optional_and_only_one_is_eaten(self):
        self.assertEqual(self._all(b"data:x\n\ndata: y\n\ndata:  z\n\n"), ["x", "y", " z"])

    def test_data_over_several_lines_is_one_event(self):
        self.assertEqual(self._all(b"data: {\ndata: \"a\": 1}\n\n"), ['{\n"a": 1}'])

    def test_an_event_the_server_never_terminated_is_still_delivered(self):
        parser = SSEParser()
        self.assertEqual(parser.feed(b"data: " + _chunk("x").encode() + b"\n\ndata: [DONE]"), [
            _chunk("x")])
        self.assertEqual(parser.close(), ["[DONE]"])
        self.assertEqual(parser.close(), [], "and only once")

    def test_a_byte_order_mark_is_not_part_of_the_first_field(self):
        self.assertEqual(self._all(b"\xef\xbb\xbfdata: x\n\n"), ["x"])

    def test_usage_without_cache_details_says_unknown_not_zero(self):
        self.assertEqual(usage_of({"prompt_tokens": 5, "completion_tokens": 1}),
                         {"prompt_tokens": 5, "completion_tokens": 1, "cached_tokens": None})
        self.assertEqual(
            usage_of({"prompt_tokens": 5, "completion_tokens": 1,
                      "prompt_tokens_details": None})["cached_tokens"], None)
        self.assertEqual(
            usage_of({"prompt_tokens": 5, "completion_tokens": 1,
                      "prompt_tokens_details": {"cached_tokens": 0}})["cached_tokens"], 0)
        self.assertIsNone(usage_of(None))


def _body(max_tokens: int, priority=None, text: str = "hello there") -> dict:
    body = {"model": "mock-model", "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": text}]}
    if priority is not None:
        body["priority"] = priority
    return body


def _wait_for(condition, what: str, timeout: float = 10.0) -> None:
    give_up = time.perf_counter() + timeout
    while not condition():
        if time.perf_counter() > give_up:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(0.005)


def _first_token_at(rec: dict) -> float:
    return rec["t_send_s"] + rec["ttft_ms"] / 1000.0


class MockQueue(unittest.TestCase):
    """The mock's queue, seen through the harness's own request function.
    These are the two behaviours the bench exists to tell apart, pinned down
    one request at a time before they are looked for in percentiles."""

    def _engine(self, **kwargs) -> MockEngine:
        engine = MockEngine(**kwargs).start()
        self.addCleanup(engine.stop)
        return engine

    def _three(self, engine, *, priorities) -> dict:
        """One long generation takes the only slot; a background request and
        then a critical one arrive, in that order, while it runs."""
        endpoint = Endpoint(engine.base_url)
        results: dict = {}

        def go(name, max_tokens, priority):
            results[name] = stream_request(
                endpoint, _body(max_tokens, priority, text=name), klass=name)

        running_p, background_p, critical_p = priorities
        threads = [threading.Thread(target=go, args=("running", 100, running_p))]
        threads[0].start()
        _wait_for(lambda: engine.stats()["running"] == 1, "the long generation to start")
        threads.append(threading.Thread(target=go, args=("background", 3, background_p)))
        threads[1].start()
        _wait_for(lambda: engine.stats()["waiting"] == 1, "the background request to queue")
        threads.append(threading.Thread(target=go, args=("critical", 2, critical_p)))
        threads[2].start()
        for thread in threads:
            thread.join(timeout=20)
        self.assertEqual({r["outcome"] for r in results.values()}, {"completed"}, results)
        return results

    def test_priority_reorders_the_waiting_queue_but_does_not_preempt(self):
        engine = self._engine(slots=1, ttft_ms=10, tokens_per_s=100, queue="priority")
        r = self._three(engine, priorities=(10, 10, 0))

        # Not preempted: the running generation produced every token it was
        # asked for, and the critical request waited out most of its second.
        self.assertEqual(r["running"]["usage"]["completion_tokens"], 100)
        self.assertEqual(r["running"]["chunks"], 100)
        self.assertGreater(r["critical"]["ttft_ms"], 300)
        # Reordered: it arrived after the background request and was served
        # before it.
        self.assertGreater(r["critical"]["t_send_s"], r["background"]["t_send_s"])
        self.assertLess(_first_token_at(r["critical"]), _first_token_at(r["background"]))
        self.assertEqual(engine.stats()["max_running"], 1)

    def test_fcfs_serves_the_waiting_queue_in_arrival_order(self):
        engine = self._engine(slots=1, ttft_ms=10, tokens_per_s=100, queue="fcfs")
        r = self._three(engine, priorities=(None, None, None))

        self.assertGreater(r["critical"]["ttft_ms"], 300)
        self.assertGreater(_first_token_at(r["critical"]), _first_token_at(r["background"]))

    def test_fcfs_refuses_a_non_zero_priority_and_the_record_says_why(self):
        engine = self._engine(slots=1, queue="fcfs")
        support = UsageSupport()
        rec = stream_request(Endpoint(engine.base_url), _body(2, priority=10),
                             klass="background", usage_support=support)
        self.assertEqual(rec["outcome"], "failed")
        self.assertEqual(rec["http_status"], 400)
        self.assertIn("priority", rec["error"])
        self.assertIsNone(rec["ttft_ms"])
        # The retry without stream_options failed the same way, so the 400
        # was not about stream_options and usage is still asked for.
        self.assertTrue(support.supported)

    def test_the_role_chunk_is_not_the_first_token_and_usage_is_read(self):
        engine = self._engine(slots=1, ttft_ms=200, tokens_per_s=200)
        endpoint = Endpoint(engine.base_url)
        first = stream_request(endpoint, _body(5, text="a b c d e f"), klass="x")
        again = stream_request(endpoint, _body(5, text="a b c d e f"), klass="x")

        self.assertEqual(first["outcome"], "completed")
        # The mock sends a role-only chunk as soon as the slot is granted and
        # the first text 200 ms later. Counting the former would report ~0.
        self.assertGreater(first["ttft_ms"], 150)
        self.assertLess(first["ttft_ms"], first["e2e_ms"])
        self.assertEqual(first["first_token_field"], "content")
        self.assertEqual(first["finish_reason"], "length")
        self.assertEqual(first["chunks"], 5)
        self.assertEqual(first["output_tokens"], 5)
        self.assertEqual(first["output_tokens_source"], "engine")
        self.assertEqual(first["usage"],
                         {"prompt_tokens": 7, "completion_tokens": 5, "cached_tokens": 0})
        self.assertEqual(again["usage"]["cached_tokens"], 7, "the same prompt, sent again")
        self.assertIsNone(first["first_token_late"], "no deadline was given")

    def test_an_engine_that_refuses_stream_options_is_retried_once_and_remembered(self):
        engine = self._engine(slots=1, ttft_ms=5, tokens_per_s=500, reject_stream_options=True)
        endpoint = Endpoint(engine.base_url)
        support = UsageSupport()

        first = stream_request(endpoint, _body(4), klass="x", usage_support=support)
        second = stream_request(endpoint, _body(4), klass="x", usage_support=support)

        self.assertEqual(first["outcome"], "completed")
        self.assertTrue(first["retried_without_stream_options"])
        self.assertIsNone(first["error"])
        self.assertIsNone(first["usage"])
        self.assertEqual(first["output_tokens"], 4)
        self.assertEqual(first["output_tokens_source"], "chunks")
        self.assertFalse(support.supported)
        self.assertEqual(second["outcome"], "completed")
        self.assertFalse(second["retried_without_stream_options"], "it was not asked again")
        self.assertEqual(engine.stats()["started"], 2)

    def test_a_request_cancelled_at_its_deadline_frees_the_slot(self):
        engine = self._engine(slots=1, ttft_ms=10, tokens_per_s=20)
        endpoint = Endpoint(engine.base_url)
        rec = stream_request(endpoint, _body(400), klass="critical",
                             deadline_ms=250, cancel_at_deadline=True)

        self.assertEqual(rec["outcome"], "cancelled_at_deadline")
        self.assertIsNotNone(rec["ttft_ms"], "it had started streaming")
        self.assertFalse(rec["first_token_late"])
        self.assertTrue(rec["completion_late"])
        self.assertIsNone(rec["e2e_ms"])
        self.assertLess(rec["chunks"], 400)
        # 400 tokens at 20 a second is 20 s. The slot must come back long
        # before that, because the client hung up.
        _wait_for(lambda: engine.stats()["running"] == 0, "the slot to be freed", timeout=5)
        self.assertEqual(engine.stats()["cancelled_running"], 1)
        after = stream_request(endpoint, _body(1), klass="critical")
        self.assertEqual(after["outcome"], "completed")
        self.assertLess(after["ttft_ms"], 5000)

    def test_a_request_cancelled_while_waiting_leaves_the_queue(self):
        engine = self._engine(slots=1, ttft_ms=10, tokens_per_s=100)
        endpoint = Endpoint(engine.base_url)
        holder = threading.Thread(
            target=stream_request, args=(endpoint, _body(120)), kwargs={"klass": "background"})
        holder.start()
        _wait_for(lambda: engine.stats()["running"] == 1, "the slot to be taken")

        rec = stream_request(endpoint, _body(2), klass="critical",
                             deadline_ms=200, cancel_at_deadline=True)

        self.assertEqual(rec["outcome"], "cancelled_at_deadline")
        self.assertIsNone(rec["ttft_ms"])
        self.assertTrue(rec["first_token_late"], "no token by the deadline is a miss")
        _wait_for(lambda: engine.stats()["cancelled_waiting"] == 1,
                  "the cancelled request to leave the queue", timeout=5)
        self.assertEqual(engine.stats()["waiting"], 0)
        holder.join(timeout=20)

    def test_without_cancel_a_late_request_is_counted_late_and_left_to_finish(self):
        engine = self._engine(slots=1, ttft_ms=300, tokens_per_s=100)
        rec = stream_request(Endpoint(engine.base_url), _body(2), klass="critical",
                             deadline_ms=100)
        self.assertEqual(rec["outcome"], "completed")
        self.assertTrue(rec["first_token_late"])
        self.assertTrue(rec["completion_late"])

    def test_an_endpoint_nobody_listens_on_is_a_failed_record_not_an_exception(self):
        with socket.socket() as placeholder:
            placeholder.bind(("127.0.0.1", 0))
            port = placeholder.getsockname()[1]
        rec = stream_request(Endpoint(f"http://127.0.0.1:{port}/v1"), _body(1),
                             klass="critical", stall_timeout_s=0.5)
        self.assertEqual(rec["outcome"], "failed")
        self.assertIsNone(rec["http_status"])
        self.assertTrue(rec["error"])


def _run_bench(engine: MockEngine, out_dir: str, name: str, *extra: str):
    """robot_bench.main() against a mock: (exit code, result JSON, Markdown)."""
    out = os.path.join(out_dir, f"{name}.json")
    argv = ["--base-url", engine.base_url, "--duration-s", "1.5", "--warmup-s", "0.3",
            "--critical-period-ms", "100", "--critical-max-tokens", "2",
            "--critical-deadline-ms", "250", "--background-max-tokens", "60",
            "--drain-timeout-s", "20", "--out", out, *extra]
    stdout, stderr = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        code = robot_bench.main(argv)
    result = None
    if os.path.exists(out):
        with open(out, encoding="utf-8") as handle:
            result = json.load(handle)
    return code, result, stdout.getvalue(), stderr.getvalue()


def _row(result: dict, phase: str, klass: str) -> dict:
    return next(r for r in result["summary"] if r["phase"] == phase and r["class"] == klass)


class Harness(unittest.TestCase):
    """Three whole runs, each against a two-slot mock whose background
    generations last about 0.6 s:

      fcfs       four background generations, first come first served
      priority   the same load, queue ordered by priority, non-preemptive
      free_slot  priority again, but one background generation: a slot is
                 always free when a critical request arrives
    """

    @classmethod
    def setUpClass(cls):
        cls._tmp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls._tmp.cleanup)
        cls.runs = {}
        scenarios = {
            "fcfs": ("fcfs", ["--priority-mode", "none", "--background-concurrency", "4"]),
            "priority": ("priority", ["--priority-mode", "vllm",
                                      "--background-concurrency", "4"]),
            "free_slot": ("priority", ["--priority-mode", "vllm",
                                       "--background-concurrency", "1", "--seed", "7"]),
        }
        for name, (queue, extra) in scenarios.items():
            with MockEngine(slots=2, ttft_ms=20, tokens_per_s=100, queue=queue) as engine:
                code, result, text, _ = _run_bench(engine, cls._tmp.name, name, *extra)
            cls.runs[name] = {"code": code, "result": result, "markdown": text}

    def test_both_phases_complete_in_every_run(self):
        for name, run in self.runs.items():
            with self.subTest(run=name):
                result = run["result"]
                self.assertEqual(run["code"], 0)
                self.assertEqual([p["name"] for p in result["phases"]], ["baseline", "loaded"])
                self.assertEqual(
                    [(r["phase"], r["class"]) for r in result["summary"]],
                    [("baseline", "critical"), ("loaded", "critical"),
                     ("loaded", "background")])
                for phase in ("baseline", "loaded"):
                    row = _row(result, phase, "critical")
                    self.assertGreaterEqual(row["sent"], 10)
                    self.assertEqual(row["completed"], row["sent"], row)
                    self.assertEqual(row["failed"], 0)
                    self.assertEqual(row["http_status"], {"200": row["sent"]})
                background = _row(result, "loaded", "background")
                self.assertGreater(background["completed"], 0)
                self.assertGreater(background["output_tokens_per_s"], 0)
                self.assertIsNone(background["deadline_miss_first_token"],
                                  "the background has no deadline to miss")

    def test_fcfs_under_saturation_delays_the_critical_stream(self):
        result = self.runs["fcfs"]["result"]
        base, loaded = _row(result, "baseline", "critical"), _row(result, "loaded", "critical")
        self.assertGreater(loaded["ttft_p50_ms"], base["ttft_p50_ms"] + 150)
        self.assertGreater(loaded["ttft_p50_ms"], 3 * base["ttft_p50_ms"])
        self.assertGreater(result["comparison"]["ttft_p50_ms"]["ratio"], 3)
        # A 250 ms deadline against a wait of several hundred: most are
        # missed under load, and more than at baseline.
        self.assertGreater(loaded["deadline_miss_first_token"], loaded["sent"] // 2)
        self.assertGreater(loaded["deadline_miss_first_token"],
                           base["deadline_miss_first_token"])
        self.assertGreaterEqual(loaded["deadline_miss_completion"],
                                loaded["deadline_miss_first_token"])

    def test_priority_with_a_free_slot_does_not_delay_the_critical_stream(self):
        result = self.runs["free_slot"]["result"]
        base, loaded = _row(result, "baseline", "critical"), _row(result, "loaded", "critical")
        self.assertLess(loaded["ttft_p50_ms"], base["ttft_p50_ms"] + 150)
        saturated = _row(self.runs["priority"]["result"], "loaded", "critical")
        self.assertLess(loaded["ttft_p50_ms"], saturated["ttft_p50_ms"])

    def test_priority_with_every_slot_busy_still_waits(self):
        # Non-preemptive: being first in the queue is not having a slot.
        result = self.runs["priority"]["result"]
        base, loaded = _row(result, "baseline", "critical"), _row(result, "loaded", "critical")
        self.assertGreater(loaded["ttft_p90_ms"], base["ttft_p90_ms"] + 150)
        self.assertGreater(loaded["deadline_miss_first_token"],
                           base["deadline_miss_first_token"])

    def test_priority_helps_against_fcfs_without_reaching_baseline(self):
        fcfs = _row(self.runs["fcfs"]["result"], "loaded", "critical")
        priority = _row(self.runs["priority"]["result"], "loaded", "critical")
        self.assertLess(priority["ttft_p50_ms"], fcfs["ttft_p50_ms"])

    def test_each_mode_sends_the_priority_it_documents(self):
        def sent(name):
            return {(r["class"], r["priority"]) for r in self.runs[name]["result"]["requests"]}
        self.assertEqual(sent("fcfs"), {("critical", None), ("background", None)})
        self.assertEqual(sent("priority"), {("critical", 0), ("background", 10)})
        self.assertEqual(robot_bench.PRIORITIES["sglang"], (10, 0))

    def test_the_json_carries_what_a_reader_needs_to_trust_or_rerun_it(self):
        result = self.runs["free_slot"]["result"]
        self.assertEqual(result["command"][:2], ["python", "robot_bench.py"])
        self.assertIn("--priority-mode", result["command"])
        self.assertIn("--priority-mode vllm", result["command_line"])
        self.assertGreater(result["finished_unix_ms"], result["started_unix_ms"])
        self.assertRegex(result["started_utc"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        self.assertTrue(result["host"]["platform"])
        self.assertGreaterEqual(result["host"]["cpu_count"], 1)
        self.assertTrue(result["engine"]["is_mock"])
        self.assertEqual(result["engine"]["models"]["data"][0]["id"], "mock-model")
        self.assertEqual(result["engine"]["version"]["engine"], "aura-bench-mock")
        self.assertNotIn("props", result["engine"], "the mock has no /props; it is left out")
        self.assertIn("nearest-rank", result["percentile_method"])
        self.assertTrue(result["stream_options_supported"])
        self.assertEqual(result["config"]["seed"], 7)
        self.assertNotIn("api_key", result["config"])
        self.assertEqual([p["outcome"] for p in result["probe"]], ["completed", "completed"])

        requests = result["requests"]
        self.assertEqual(len(requests), sum(r["sent"] for r in result["summary"]))
        self.assertEqual(len({r["id"] for r in requests}), len(requests))
        for rec in requests:
            self.assertIn(rec["phase"], ("baseline", "loaded"))
            self.assertIn(rec["outcome"], ("completed", "aborted"))
            if rec["outcome"] == "completed":
                self.assertEqual(rec["usage"]["completion_tokens"], rec["max_tokens"])
                self.assertGreater(rec["e2e_ms"], rec["ttft_ms"])
        # Engine-reported usage, cache hits included, summed per class.
        critical = _row(result, "loaded", "critical")
        self.assertEqual(critical["usage_reported_n"], critical["sent"])
        self.assertEqual(critical["completion_tokens"], 2 * critical["sent"])
        self.assertGreater(critical["cached_tokens"], 0)
        self.assertLessEqual(critical["cached_tokens"], critical["prompt_tokens"])

    def test_the_markdown_is_labelled_as_mock_output_and_has_both_tables(self):
        text = self.runs["fcfs"]["markdown"]
        self.assertIn("MOCK ENGINE", text)
        self.assertIn("| phase | class | sent | TTFT n | TTFT p50 ms |", text)
        self.assertIn("missed deadline: first token", text)
        self.assertIn("| baseline | critical |", text)
        self.assertIn("| loaded | background |", text)
        self.assertIn("loaded against baseline", text)
        self.assertIn("nearest-rank", text)
        self.assertNotIn("None", text)
        self.assertNotIn("nan", text)

    def test_a_class_with_no_samples_prints_na_and_never_a_number(self):
        phases = [{"name": "baseline", "background_concurrency": 0},
                  {"name": "loaded", "background_concurrency": 4}]
        refused = {
            "id": 1, "class": "critical", "phase": "loaded", "outcome": "failed",
            "t_send_s": 0.0, "t_end_s": 0.01, "http_status": 503, "error": "HTTP 503: busy",
            "ttft_ms": None, "e2e_ms": None, "gen_ms": None, "usage": None,
            "output_tokens": 0, "output_tokens_source": "chunks", "deadline_ms": 250,
            "first_token_late": True, "completion_late": True}
        summary = robot_bench.summarize([refused], phases)
        result = dict(self.runs["fcfs"]["result"], summary=summary,
                      comparison=robot_bench.compare(summary))
        text = robot_bench.markdown(result)

        # Nothing was sent at baseline; one request was sent under load and
        # refused. Neither row has a latency to report.
        self.assertIn("| baseline | critical | 0 | 0 | n/a | n/a | n/a | n/a | 0 | "
                      "n/a | n/a | n/a | n/a | n/a |", text)
        self.assertIn("| loaded | critical | 1 | 0 | n/a | n/a | n/a | n/a | 0 | "
                      "n/a | n/a | n/a | 0.0 | n/a |", text)
        self.assertIn("| loaded | background | 0 | 0 | n/a |", text)
        self.assertIn("| loaded | critical | 1 | 0 | 1 | 0 | 0 | 503 x1 | 1 of 1 | 1 of 1 | "
                      "n/a | n/a | n/a |", text)
        self.assertIn("p50 n/a, p99 n/a", text)
        self.assertIn("HTTP 503: busy", text)
        self.assertNotIn("None", text)
        for row in summary:
            for key in ("ttft_p50_ms", "ttft_p90_ms", "ttft_p99_ms", "ttft_max_ms",
                        "e2e_p50_ms", "e2e_p90_ms", "e2e_p99_ms"):
                self.assertIsNone(row[key])


class HarnessEdges(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = tmp.name

    def test_cancel_at_deadline_is_counted_and_the_engine_sees_the_hang_ups(self):
        with MockEngine(slots=1, ttft_ms=20, tokens_per_s=100, queue="fcfs") as engine:
            code, result, text, _ = _run_bench(
                engine, self.tmp, "cancel", "--duration-s", "0.8", "--warmup-s", "0.2",
                "--background-concurrency", "3", "--cancel-at-deadline")
            loaded = _row(result, "loaded", "critical")
            # Waited for, not read once: the mock notices a hang-up on its
            # next poll, up to 20 ms after the client has moved on. The count
            # also includes the background requests cut when the phase ended,
            # so this is a floor; one cancel, one freed slot is pinned down
            # exactly in MockQueue.
            _wait_for(lambda: engine.stats()["cancelled_waiting"]
                      + engine.stats()["cancelled_running"] >= loaded["cancelled_at_deadline"],
                      "the engine to notice the hang-ups", timeout=5)

        self.assertEqual(code, 0)
        self.assertGreater(loaded["cancelled_at_deadline"], 0)
        self.assertEqual(_row(result, "baseline", "critical")["cancelled_at_deadline"], 0)
        self.assertEqual(loaded["failed"], 0, "a cancel is not a failure")
        self.assertEqual(loaded["sent"], loaded["completed"] + loaded["cancelled_at_deadline"])
        self.assertGreaterEqual(loaded["deadline_miss_first_token"],
                                loaded["sent"] - loaded["ttft_n"])
        self.assertIn("cancelled at the deadline", text)
        self.assertIn("describe only the requests that made it", text)

    def test_vllm_mode_against_an_fcfs_engine_stops_before_measuring_anything(self):
        with MockEngine(slots=2, queue="fcfs") as engine:
            code, result, text, err = _run_bench(
                engine, self.tmp, "refused", "--priority-mode", "vllm",
                "--background-concurrency", "2")
        self.assertEqual(code, 2)
        self.assertIsNone(result, "no output file for a run that never ran")
        self.assertEqual(text, "")
        self.assertIn("--scheduling-policy priority", err)

    def test_the_api_key_is_sent_and_never_written_down(self):
        self.assertEqual(
            robot_bench._redact(["--api-key", "sk-secret", "--model", "m", "--api-key=sk-2"]),
            ["--api-key", "***", "--model", "m", "--api-key=***"])
        self.assertEqual(Endpoint("http://h:1/v1", "sk-secret").headers()["Authorization"],
                         "Bearer sk-secret")
        self.assertNotIn("Authorization", Endpoint("http://h:1/v1").headers())

    def test_the_base_url_is_split_into_api_prefix_and_server_root(self):
        endpoint = Endpoint("http://10.0.0.2:8000/v1/")
        self.assertEqual((endpoint.host, endpoint.port), ("10.0.0.2", 8000))
        self.assertEqual((endpoint.prefix, endpoint.root), ("/v1", ""))
        proxied = Endpoint("https://example.test/engines/a/v1")
        self.assertEqual((proxied.port, proxied.prefix, proxied.root),
                         (443, "/engines/a/v1", "/engines/a"))
        with self.assertRaises(ValueError):
            Endpoint("127.0.0.1:8080")

    def test_a_prompt_file_replaces_only_what_it_names(self):
        path = os.path.join(self.tmp, "prompts.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump({"critical": {"system": "", "user": "halt?"},
                       "background": {"user": ["a", "b"]}}, handle)
        prompts = robot_bench.load_prompts(path, seed=0)
        self.assertEqual(prompts["critical"], {"system": "", "user": ["halt?"]})
        self.assertEqual(prompts["background"]["user"], ["a", "b"])
        self.assertEqual(prompts["background"]["system"],
                         robot_bench.DEFAULT_BACKGROUND_SYSTEM)

        with open(path, "w", encoding="utf-8") as handle:
            json.dump({"critical": {"user": []}}, handle)
        with self.assertRaises(ValueError):
            robot_bench.load_prompts(path, seed=0)

    def test_the_built_in_prompts_are_the_same_for_the_same_seed(self):
        self.assertEqual(robot_bench.default_prompts(3), robot_bench.default_prompts(3))
        self.assertNotEqual(robot_bench.default_prompts(3)["critical"]["user"],
                            robot_bench.default_prompts(4)["critical"]["user"])


class LeastSquares(unittest.TestCase):
    def test_it_recovers_planted_coefficients_exactly(self):
        truth = [12.0, 0.5, 0.05, 5.0]
        rows = [[1.0, fresh, cached, out]
                for fresh, cached in ((200, 0), (0, 200), (1000, 0), (0, 1000), (4000, 30))
                for out in (4, 64, 256)]
        y = [sum(c * v for c, v in zip(truth, row)) for row in rows]
        fit = calibrate_tokens.least_squares(rows, y)
        for got, want in zip(fit["coef"], truth):
            self.assertAlmostEqual(got, want, places=6)
        self.assertAlmostEqual(fit["rms"], 0.0, places=6)
        self.assertAlmostEqual(fit["r2"], 1.0, places=9)
        self.assertEqual((fit["n"], fit["dof"]), (15, 11))

    def test_noise_shows_up_in_the_residual_and_the_standard_errors(self):
        rows = [[1.0, float(x)] for x in range(1, 21)]
        y = [3.0 + 2.0 * x + (0.5 if x % 2 else -0.5) for x in range(1, 21)]
        fit = calibrate_tokens.least_squares(rows, y)
        self.assertAlmostEqual(fit["coef"][1], 2.0, delta=0.05)
        self.assertAlmostEqual(fit["rms"], 0.5, delta=0.05)
        self.assertGreater(fit["stderr"][1], 0)
        self.assertLess(fit["stderr"][1], 0.05)

    def test_terms_the_samples_cannot_tell_apart_are_refused(self):
        # The output length never changes, so it is indistinguishable from
        # the per-request constant.
        rows = [[1.0, float(n), 16.0] for n in (100, 200, 300, 400)]
        with self.assertRaises(calibrate_tokens.SingularFit):
            calibrate_tokens.least_squares(rows, [1.0, 2.0, 3.0, 4.0])
        with self.assertRaises(calibrate_tokens.SingularFit):
            calibrate_tokens.least_squares([[1.0, 2.0, 3.0]], [1.0])
        with self.assertRaises(calibrate_tokens.SingularFit):
            calibrate_tokens.least_squares([[1.0, 0.0], [1.0, 0.0], [1.0, 0.0]], [1.0, 2.0, 3.0])


def _sample(send, prompt, cached, output, wall):
    return {"pair": 0, "send": send, "target_prompt_tokens": prompt, "max_tokens": output,
            "outcome": "completed", "error": None, "wall_ms": wall, "ttft_ms": None,
            "prompt_tokens": prompt, "cached_tokens": cached, "completion_tokens": output,
            "engine_timings": None}


class Calibration(unittest.TestCase):
    def _samples(self, cached_of):
        """Wall times from a known cost: 0.4 ms fresh, 0.04 cached, 6 output."""
        out = []
        for prompt in (300, 1200):
            for output in (8, 48):
                for send in ("first", "second"):
                    cached = prompt if send == "second" else 0
                    wall = 10 + 0.4 * (prompt - cached) + 0.04 * cached + 6.0 * output
                    out.append(_sample(send, prompt, cached_of(cached), output, wall))
        return out

    def test_alpha_and_beta_are_the_ratios_of_the_fitted_costs(self):
        analysis = calibrate_tokens.analyse(self._samples(lambda cached: cached))
        self.assertIsNone(analysis["refusal"])
        self.assertAlmostEqual(analysis["alpha"], 0.1, places=6)
        self.assertAlmostEqual(analysis["beta"], 15.0, places=6)
        self.assertEqual(analysis["fit"]["n"], 8)

    def test_no_fit_when_the_engine_never_reports_cached_tokens(self):
        analysis = calibrate_tokens.analyse(self._samples(lambda cached: None))
        self.assertIsNone(analysis["alpha"])
        self.assertIsNone(analysis["beta"])
        self.assertIsNone(analysis["fit"])
        self.assertIn("did not report", analysis["refusal"])
        # What is left: beta from the first sends, whose prompts were fresh.
        self.assertAlmostEqual(analysis["cold_beta"], 15.0, places=6)

    def test_no_fit_when_cached_tokens_never_became_non_zero(self):
        analysis = calibrate_tokens.analyse(self._samples(lambda cached: 0))
        self.assertIsNone(analysis["alpha"])
        self.assertIsNone(analysis["fit"])
        self.assertIn("= 0 on every response", analysis["refusal"])

    def test_no_fit_when_nothing_carried_token_counts(self):
        samples = self._samples(lambda cached: cached)
        for sample in samples:
            sample["prompt_tokens"] = sample["completion_tokens"] = None
        analysis = calibrate_tokens.analyse(samples)
        self.assertIsNone(analysis["alpha"])
        self.assertIsNone(analysis["cold_fit"])
        self.assertIn("nothing to fit", analysis["refusal"])

    def _calibrate(self, engine, *extra):
        out = os.path.join(self._tmp, "calibration.json")
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = calibrate_tokens.main([
                "--base-url", engine.base_url, "--prompt-tokens", "150,600",
                "--max-tokens", "4,24", "--repeats", "1", "--out", out, *extra])
        with open(out, encoding="utf-8") as handle:
            return code, json.load(handle), stdout.getvalue()

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self._tmp = tmp.name

    def test_smoke_against_the_mock_and_the_output_says_it_is_meaningless(self):
        # The mock is told what each kind of token costs: 0.5 ms fresh,
        # 0.05 ms cached, 5 ms output. So alpha is 0.1 and beta is 10, give
        # or take the timer. The bounds are loose because this is a check
        # that the pipeline runs end to end, not a measurement.
        with MockEngine(slots=1, ttft_ms=10, tokens_per_s=200, ms_per_input_token=0.5,
                        ms_per_cached_token=0.05) as engine:
            code, result, text = self._calibrate(engine)

        self.assertEqual(code, 0)
        self.assertEqual(text.count("MOCK ENGINE"), 2, "said at the top and at the bottom")
        self.assertIn("NOT a calibration", text)
        self.assertIn("alpha (cached input / fresh input)", text)
        analysis = result["analysis"]
        self.assertEqual((analysis["samples"], analysis["usable"]), (8, 8))
        self.assertEqual(analysis["cached_nonzero"], 8)
        self.assertGreater(analysis["alpha"], 0.0)
        self.assertLess(analysis["alpha"], 0.6)
        self.assertGreater(analysis["beta"], 3.0)
        self.assertLess(analysis["beta"], 30.0)
        seconds = [s for s in result["samples"] if s["send"] == "second"]
        self.assertTrue(all(s["cached_tokens"] == s["prompt_tokens"] for s in seconds),
                        "the repeat was confirmed as a cache hit from the engine's own count")

    def test_against_an_engine_that_hides_cached_tokens_it_refuses_to_print_alpha(self):
        with MockEngine(slots=1, ttft_ms=5, tokens_per_s=400, ms_per_input_token=0.2,
                        report_cached=False) as engine:
            code, result, text = self._calibrate(engine)

        self.assertEqual(code, calibrate_tokens.EXIT_NO_FIT)
        self.assertIn("NOT REPORTED", text)
        self.assertNotIn("alpha (cached input", text)
        self.assertIsNone(result["analysis"]["alpha"])
        self.assertIn("MOCK ENGINE", text)


if __name__ == "__main__":
    unittest.main()
