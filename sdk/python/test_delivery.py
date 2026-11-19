"""Inference through the node: naming the delivery a request is for, and
never sending a node credential anywhere but the node.

    cd sdk/python && PYTHONPATH=src python -m unittest test_delivery -v
"""
import json
import os
import unittest
from unittest.mock import patch

from aura import Context, DeliveryRef, Envelope, Skill, node_headers, node_inference_url, via_node
from aura.llm import ChatBackend

MANIFEST = {
    "id": "acme/logical/echo", "version": "1.0.0", "protocol": "1",
    "name": "echo", "description": "test", "capability": "logical.echo",
    "type": "logical", "format": "source",
    "ports": {"ingress": [{"name": "text_in", "schema": "std/text@1"}],
              "egress": [{"name": "text_out", "schema": "std/text@1"}]},
}

_ENV = ("OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL", "AURA_INFERENCE_URL",
        "AURA_INFERENCE_MODEL", "AURA_INFERENCE_EXTRA", "AURA_INFERENCE_PRIORITY",
        "AURA_WS_URL", "AURA_TOKEN")


class _Env(unittest.TestCase):
    """Each test starts from an environment with none of the knobs set."""

    def setUp(self):
        self._patch = patch.dict(os.environ, {}, clear=False)
        self._patch.start()
        for name in _ENV:
            os.environ.pop(name, None)
        # A data directory with no token file, so the developer's own node
        # token never leaks into what these tests assert about.
        self._no_token = patch("aura.skill._default_data_dir",
                               return_value=__import__("pathlib").Path("/nonexistent-aura-data-dir"))
        self._no_token.start()

    def tearDown(self):
        self._no_token.stop()
        self._patch.stop()


class TheReference(_Env):
    def test_headers_name_the_delivery(self):
        ref = DeliveryRef(session="sess-1", id="deliv-1", node="brain", port="text_in")
        self.assertEqual(ref.headers(), {
            "X-Aura-Session": "sess-1", "X-Aura-Delivery": "deliv-1",
            "X-Aura-Node": "brain", "X-Aura-Port": "text_in"})

    def test_what_is_not_known_is_left_out(self):
        self.assertEqual(DeliveryRef(session="s", id="d").headers(),
                         {"X-Aura-Session": "s", "X-Aura-Delivery": "d"})

    def test_a_handler_gets_one_for_its_own_delivery(self):
        incoming = Envelope.from_wire({
            "v": "1", "id": "in-1", "kind": "data", "session": "sess-9", "node": "brain",
            "port": "text_in", "idem": "s:1", "schema": "std/text@1", "payload": {"text": "hi"}})
        ref = Context(Skill(manifest=MANIFEST, token=""), incoming).delivery
        self.assertEqual(ref, DeliveryRef(session="sess-9", id="in-1", node="brain", port="text_in"))

    def test_it_is_safe_to_hand_to_a_thread(self):
        ref = DeliveryRef(session="s", id="d")
        with self.assertRaises(Exception):
            ref.session = "other"  # frozen: nothing a worker does can change what it names


class WhereTheNodeIs(_Env):
    def test_derived_from_the_socket_address(self):
        for ws, want in {
            "ws://localhost:9080/ws/skill": "http://localhost:9080/v1/inference",
            "wss://robot.local:9443/ws/skill": "https://robot.local:9443/v1/inference",
            "ws://10.0.0.7:9080/ws/expertise?token=abc": "http://10.0.0.7:9080/v1/inference",
            "ws://[::1]:9080/ws/skill": "http://[::1]:9080/v1/inference",
        }.items():
            self.assertEqual(node_inference_url(ws), want)

    def test_follows_the_same_variable_the_socket_does(self):
        os.environ["AURA_WS_URL"] = "ws://edge-7:9080/ws/skill"
        self.assertEqual(node_inference_url(), "http://edge-7:9080/v1/inference")

    def test_the_default_is_the_default_node(self):
        self.assertEqual(node_inference_url(), "http://localhost:9080/v1/inference")

    def test_the_word_node_is_what_turns_it_on(self):
        self.assertFalse(via_node())
        for value in ("node", "NODE", "  Node  "):
            os.environ["AURA_INFERENCE_URL"] = value
            self.assertTrue(via_node(), value)
        os.environ["AURA_INFERENCE_URL"] = "http://127.0.0.1:8000/v1"
        self.assertFalse(via_node())


class TheCredential(_Env):
    def test_the_node_token_and_the_delivery_travel_together(self):
        os.environ["AURA_TOKEN"] = "node-token"
        got = node_headers(DeliveryRef(session="s", id="d"))
        self.assertEqual(got["Authorization"], "Bearer node-token")
        self.assertEqual(got["X-Aura-Delivery"], "d")

    def test_a_node_without_auth_gets_no_header(self):
        self.assertNotIn("Authorization", node_headers())

    def test_work_for_no_delivery_names_none(self):
        os.environ["AURA_TOKEN"] = "node-token"
        self.assertEqual(node_headers(), {"Authorization": "Bearer node-token"})


class _Sent:
    """Captures the one request ChatBackend makes."""

    def __init__(self) -> None:
        self.url = ""
        self.headers: dict[str, str] = {}
        self.body: dict = {}

    def __call__(self, req, timeout=None):
        self.url = req.full_url
        self.headers = {k.lower(): v for k, v in req.header_items()}
        self.body = json.loads(req.data)

        class _Resp:
            def __enter__(self_inner):
                return self_inner

            def __exit__(self_inner, *_exc):
                return False

            def __iter__(self_inner):
                return iter([b'data: {"choices":[{"delta":{"content":"hi"}}]}\n', b"data: [DONE]\n"])

        return _Resp()


def _chat(sent: _Sent, **kwargs) -> str:
    backend = ChatBackend()
    with patch("urllib.request.urlopen", side_effect=sent):
        return "".join(backend.stream_chat([{"role": "user", "content": "hey"}], **kwargs))


class ChatBackendThroughTheNode(_Env):
    def setUp(self):
        super().setUp()
        os.environ["AURA_INFERENCE_URL"] = "node"
        os.environ["AURA_WS_URL"] = "ws://robot.local:9080/ws/skill"
        os.environ["AURA_TOKEN"] = "node-token"

    def test_the_request_goes_to_the_node_with_its_credential_and_delivery(self):
        sent = _Sent()
        ref = DeliveryRef(session="sess-1", id="deliv-1", node="brain", port="text_in")
        self.assertEqual(_chat(sent, delivery=ref), "hi")
        self.assertEqual(sent.url, "http://robot.local:9080/v1/inference/chat/completions")
        self.assertEqual(sent.headers["authorization"], "Bearer node-token")
        self.assertEqual(sent.headers["x-aura-session"], "sess-1")
        self.assertEqual(sent.headers["x-aura-delivery"], "deliv-1")
        self.assertEqual(sent.headers["x-aura-node"], "brain")
        self.assertEqual(sent.headers["x-aura-port"], "text_in")

    def test_without_a_delivery_it_is_still_served_as_unattributed_work(self):
        sent = _Sent()
        self.assertEqual(_chat(sent), "hi")
        self.assertEqual(sent.headers["authorization"], "Bearer node-token")
        self.assertFalse([h for h in sent.headers if h.startswith("x-aura-")])

    def test_the_priority_is_the_nodes_to_state(self):
        # Even told how the engine spells one. Through the node, this process
        # naming its own urgency is exactly what the node takes back out.
        os.environ["AURA_INFERENCE_PRIORITY"] = "vllm"
        sent = _Sent()
        _chat(sent, priority=9, delivery=DeliveryRef(session="s", id="d"))
        self.assertNotIn("priority", sent.body)

    def test_it_counts_as_a_remote_backend(self):
        backend = ChatBackend()
        self.assertTrue(backend.is_cloud)
        self.assertEqual(backend.attestation().engine, "openai-compatible")


class TheCredentialStaysHome(_Env):
    """The node token must reach the node and nothing else. These are the
    other two places a request can go."""

    def test_not_to_an_engine_reached_directly(self):
        os.environ["AURA_INFERENCE_URL"] = "http://127.0.0.1:8000/v1"
        os.environ["AURA_TOKEN"] = "node-token"
        sent = _Sent()
        _chat(sent, delivery=DeliveryRef(session="s", id="d"))
        self.assertEqual(sent.url, "http://127.0.0.1:8000/v1/chat/completions")
        self.assertNotIn("authorization", sent.headers)
        self.assertFalse([h for h in sent.headers if h.startswith("x-aura-")])

    def test_not_to_a_hosted_api(self):
        os.environ["OPENAI_API_KEY"] = "sk-hosted"
        os.environ["AURA_INFERENCE_URL"] = "node"  # a hosted key still wins
        os.environ["AURA_TOKEN"] = "node-token"
        sent = _Sent()
        _chat(sent, delivery=DeliveryRef(session="s", id="d"))
        self.assertTrue(sent.url.startswith("https://api.openai.com/"))
        self.assertEqual(sent.headers["authorization"], "Bearer sk-hosted")
        self.assertFalse([h for h in sent.headers if h.startswith("x-aura-")])


if __name__ == "__main__":
    unittest.main()
