"""The planner's remote endpoint, its decoding grammar and its prompt.

No model and no network: urlopen is faked, and the grammar is checked with a
small GBNF interpreter written here rather than against llama.cpp, which CI
does not install.

    cd expertise/planner && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import json
import os
import re
import unittest
from unittest import mock

import backends
from catalog import catalog_lines

CATALOG = [
    {"id": "acme/motor/email", "capability": "motor.email.send",
     "description": "Send an email", "ports": {"ingress": [{"schema": "std/text@1"}]}},
    {"id": "acme/logical/crm", "capability": "logical.api.crm.get_customer",
     "description": "Fetch a customer", "ports": {"ingress": [{"schema": "std/api-request@1"}]}},
]
CONFIG = {"openai_model": "gpt-4o-mini", "max_tokens": 256, "temperature": 0.1}


class _Response:
    def __init__(self, body: dict) -> None:
        self._body = json.dumps(body).encode()

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def read(self, *_a):
        return self._body


def _answer(steps):
    content = json.dumps({"reasoning": "because", "steps": steps})
    return _Response({"choices": [{"message": {"content": content}}]})


class RemoteEndpoint(unittest.TestCase):
    def setUp(self):
        self.env = mock.patch.dict(os.environ, {}, clear=False)
        self.env.start()
        for name in ("OPENAI_API_KEY", "OPENAI_BASE_URL", "AURA_INFERENCE_URL",
                     "AURA_INFERENCE_MODEL"):
            os.environ.pop(name, None)

    def tearDown(self):
        self.env.stop()

    def _plan(self):
        seen = {}

        def fake_urlopen(req, timeout=None):
            seen["url"] = req.full_url
            seen["auth"] = req.get_header("Authorization")
            seen["body"] = json.loads(req.data)
            return _answer([{"capability": "motor.email.send", "payload": {}}])

        with mock.patch("urllib.request.urlopen", side_effect=fake_urlopen):
            result = backends.plan_openai("email bob", CATALOG, CONFIG)
        return seen, result

    def test_default_is_still_openai(self):
        os.environ["OPENAI_API_KEY"] = "sk-1"
        seen, (reasoning, steps) = self._plan()
        self.assertEqual(seen["url"], "https://api.openai.com/v1/chat/completions")
        self.assertEqual(seen["auth"], "Bearer sk-1")
        self.assertEqual(seen["body"]["model"], "gpt-4o-mini")
        self.assertEqual(steps[0]["capability"], "motor.email.send")
        self.assertEqual(reasoning, "because")

    def test_the_base_url_the_chat_skill_honours_is_honoured_here_too(self):
        os.environ["OPENAI_API_KEY"] = "g-1"
        os.environ["OPENAI_BASE_URL"] = "https://generativelanguage.googleapis.com/v1beta/openai/"
        seen, _ = self._plan()
        self.assertEqual(
            seen["url"],
            "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions")

    def test_a_self_hosted_server_needs_no_key(self):
        os.environ["AURA_INFERENCE_URL"] = "http://127.0.0.1:8080/v1"
        os.environ["AURA_INFERENCE_MODEL"] = "qwen3-4b"
        seen, _ = self._plan()
        self.assertEqual(seen["url"], "http://127.0.0.1:8080/v1/chat/completions")
        self.assertIsNone(seen["auth"])
        self.assertEqual(seen["body"]["model"], "qwen3-4b")

    def test_auto_prefers_a_configured_server_over_loading_weights(self):
        os.environ["AURA_INFERENCE_URL"] = "http://127.0.0.1:8080/v1"
        calls = []
        with mock.patch.dict(backends._BACKEND_FNS, {
                "openai": lambda g, c, cfg: (calls.append("openai") or ("r", [{"capability": "x", "payload": {}}])),
                "local": lambda g, c, cfg: (calls.append("local") or ("r", [])),
        }):
            used, _, _ = backends.make_plan("goal", CATALOG, backend="auto", config=CONFIG)
        self.assertEqual((used, calls), ("openai", ["openai"]))

    def test_auto_without_any_endpoint_is_unchanged(self):
        calls = []
        with mock.patch.dict(backends._BACKEND_FNS, {
                "openai": lambda g, c, cfg: calls.append("openai"),
                "local": lambda g, c, cfg: (calls.append("local") or ("r", [{"capability": "x", "payload": {}}])),
        }):
            used, _, _ = backends.make_plan("goal", CATALOG, backend="auto", config=CONFIG)
        self.assertEqual((used, calls), ("local", ["local"]))

    def test_a_dead_server_degrades_to_the_next_backend(self):
        os.environ["AURA_INFERENCE_URL"] = "http://127.0.0.1:1/v1"
        with mock.patch("urllib.request.urlopen", side_effect=OSError("refused")), \
                mock.patch.dict(backends._BACKEND_FNS, {
                    "local": lambda g, c, cfg: ("r", [{"capability": "x", "payload": {}}])}):
            used, _, _ = backends.make_plan("goal", CATALOG, backend="auto", config=CONFIG)
        self.assertEqual(used, "local")


# ── a GBNF interpreter small enough to trust ───────────────────────────────
#
# Supports exactly what plan_grammar emits: literals, character classes,
# rule references, grouping, alternation, and the ? * + suffixes. Matching
# is by backtracking over the set of reachable positions.

_TOKEN = re.compile(r'''
    \s*(?:
      (?P<lit>"(?:[^"\\]|\\.)*")
    | (?P<cls>\[(?:[^\]\\]|\\.)*\])
    | (?P<name>[A-Za-z_][A-Za-z0-9_-]*)
    | (?P<op>[()|?*+])
    )''', re.X)


def _unescape(body: str) -> str:
    out, i = [], 0
    while i < len(body):
        ch = body[i]
        if ch == "\\":
            nxt = body[i + 1]
            if nxt == "x":
                out.append(chr(int(body[i + 2:i + 4], 16)))
                i += 4
                continue
            out.append({"n": "\n", "t": "\t", "r": "\r"}.get(nxt, nxt))
            i += 2
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def _class(body: str):
    negate = body.startswith("^")
    if negate:
        body = body[1:]
    chars = []
    i = 0
    while i < len(body):
        if body[i] == "\\":
            if body[i + 1] == "x":
                chars.append(chr(int(body[i + 2:i + 4], 16)))
                i += 4
            else:
                chars.append({"n": "\n", "t": "\t", "r": "\r"}.get(body[i + 1], body[i + 1]))
                i += 2
        else:
            chars.append(body[i])
            i += 1
    ranges, j = [], 0
    while j < len(chars):
        if j + 2 < len(chars) and chars[j + 1] == "-":
            ranges.append((chars[j], chars[j + 2]))
            j += 3
        else:
            ranges.append((chars[j], chars[j]))
            j += 1
    return ("cls", negate, ranges)


def _parse(grammar: str) -> dict:
    rules = {}
    for line in grammar.strip().splitlines():
        name, _, body = line.partition("::=")
        tokens = []
        pos = 0
        body = body.strip()
        while pos < len(body):
            m = _TOKEN.match(body, pos)
            assert m, f"cannot tokenize {body[pos:pos + 20]!r}"
            pos = m.end()
            if m.group("lit"):
                tokens.append(("lit", _unescape(m.group("lit")[1:-1])))
            elif m.group("cls"):
                tokens.append(_class(m.group("cls")[1:-1]))
            elif m.group("name"):
                tokens.append(("ref", m.group("name")))
            else:
                tokens.append(("op", m.group("op")))
        rules[name.strip()] = _alt(tokens, [0])
    return rules


def _alt(tokens, i):
    options = [_seq(tokens, i)]
    while i[0] < len(tokens) and tokens[i[0]] == ("op", "|"):
        i[0] += 1
        options.append(_seq(tokens, i))
    return ("alt", options)


def _seq(tokens, i):
    items = []
    while i[0] < len(tokens) and tokens[i[0]] not in (("op", "|"), ("op", ")")):
        tok = tokens[i[0]]
        i[0] += 1
        if tok == ("op", "("):
            node = _alt(tokens, i)
            assert tokens[i[0]] == ("op", ")")
            i[0] += 1
        else:
            node = tok
        while i[0] < len(tokens) and tokens[i[0]][0] == "op" and tokens[i[0]][1] in "?*+":
            node = ("rep", tokens[i[0]][1], node)
            i[0] += 1
        items.append(node)
    return ("seq", items)


def _ends(rules, node, text, starts):
    kind = node[0]
    if kind == "lit":
        return {s + len(node[1]) for s in starts if text.startswith(node[1], s)}
    if kind == "cls":
        _, negate, ranges = node
        out = set()
        for s in starts:
            if s < len(text) and (any(a <= text[s] <= b for a, b in ranges) != negate):
                out.add(s + 1)
        return out
    if kind == "ref":
        return _ends(rules, rules[node[1]], text, starts)
    if kind == "alt":
        out = set()
        for option in node[1]:
            out |= _ends(rules, option, text, starts)
        return out
    if kind == "seq":
        current = set(starts)
        for item in node[1]:
            current = _ends(rules, item, text, current)
            if not current:
                break
        return current
    if kind == "rep":
        _, op, inner = node
        reached = set(starts) if op in "?*" else set()
        frontier = set(starts)
        while frontier:
            step = _ends(rules, inner, text, frontier) - reached
            reached |= step
            frontier = step if op != "?" else set()
        return reached
    raise AssertionError(kind)


def _matches(grammar: str, text: str) -> bool:
    rules = _parse(grammar)
    return len(text) in _ends(rules, rules["root"], text, {0})


class PlanGrammar(unittest.TestCase):
    def setUp(self):
        self.grammar = backends.plan_grammar(CATALOG)

    def _plan(self, capability, payload=None, **extra):
        return json.dumps({"reasoning": "r", "steps": [
            {"capability": capability, "payload": payload if payload is not None else {}}], **extra})

    def test_a_real_capability_is_accepted(self):
        self.assertTrue(_matches(self.grammar, self._plan("motor.email.send")))
        self.assertTrue(_matches(self.grammar, self._plan(
            "logical.api.crm.get_customer",
            {"params": {"id": "42"}, "query": {"full": True, "n": -1.5e3}, "tags": ["a", None]})))

    def test_an_invented_capability_cannot_be_emitted(self):
        for wrong in ("motor.email.sent", "motor.email", "MOTOR.EMAIL.SEND", ""):
            self.assertFalse(_matches(self.grammar, self._plan(wrong)), wrong)

    def test_several_steps_and_pretty_printing_are_fine(self):
        text = json.dumps({"reasoning": "two things", "steps": [
            {"capability": "logical.api.crm.get_customer", "payload": {"params": {"id": "7"}}},
            {"capability": "motor.email.send", "payload": {"text": "hola \"Bob\"\n"}},
        ]}, indent=2, ensure_ascii=False)
        self.assertTrue(_matches(self.grammar, text))

    def test_no_steps_is_not_a_plan(self):
        self.assertFalse(_matches(self.grammar, json.dumps({"reasoning": "r", "steps": []})))

    def test_anything_after_the_object_is_refused(self):
        self.assertFalse(_matches(self.grammar, self._plan("motor.email.send") + " trailing"))

    def test_what_the_grammar_accepts_is_json_the_planner_can_read(self):
        text = self._plan("motor.email.send", {"body": {"to": "bob"}})
        self.assertTrue(_matches(self.grammar, text))
        self.assertEqual(json.loads(text)["steps"][0]["capability"], "motor.email.send")

    def test_capabilities_are_deduplicated_and_ordered(self):
        doubled = CATALOG + [dict(CATALOG[0], id="other/motor/email")]
        self.assertEqual(backends.plan_grammar(doubled), self.grammar)

    def test_an_empty_catalog_has_no_grammar(self):
        with self.assertRaises(ValueError):
            backends.plan_grammar([])

    def test_without_llama_cpp_the_old_json_mode_is_used(self):
        # CI has no llama-cpp-python: the import fails and the constraint
        # falls back instead of raising.
        with mock.patch.dict("sys.modules", {"llama_cpp": None}):
            self.assertEqual(backends._local_constraint(CATALOG),
                             {"response_format": {"type": "json_object"}})


class StablePrompt(unittest.TestCase):
    def test_catalog_order_does_not_change_the_prompt(self):
        self.assertEqual(catalog_lines(CATALOG), catalog_lines(list(reversed(CATALOG))))


if __name__ == "__main__":
    unittest.main()
