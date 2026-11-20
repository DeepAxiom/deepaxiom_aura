"""Registration, as the Expertise sees it: what the node answers, and in what
order it may arrive.

    cd sdk/python && PYTHONPATH=src python -m unittest test_register -v
"""
import asyncio
import json
import unittest

import websockets

from aura import Context, Skill

MANIFEST = {
    "id": "acme/logical/echo", "version": "1.0.0", "protocol": "1",
    "name": "echo", "description": "test", "capability": "logical.echo",
    "type": "logical", "format": "source",
    "ports": {"ingress": [{"name": "text_in", "schema": "std/text@1"}],
              "egress": [{"name": "text_out", "schema": "std/text@1"}]},
}

ACK = {"v": "1", "id": "ack-1", "kind": "status",
       "payload": {"state": "registered", "skill": MANIFEST["id"],
                   "config": {"temperature": 0.2}, "grammars": {"text_out": "root ::= x"}}}


def delivery(n: int) -> dict:
    return {"v": "1", "id": f"in-{n}", "kind": "data", "session": "s", "node": "echo",
            "port": "text_in", "idem": f"s:{n}", "schema": "std/text@1",
            "payload": {"text": f"message {n}"}}


class Node:
    """A node that says exactly what a test tells it to, in that order."""

    def __init__(self, frames: list[dict]) -> None:
        self.frames = frames
        self.received: list[dict] = []
        self.server = None
        self.port = 0

    async def __aenter__(self) -> "Node":
        async def handler(ws) -> None:
            self.received.append(json.loads(await ws.recv()))  # the registration
            for frame in self.frames:
                await ws.send(json.dumps(frame))
            try:
                async for raw in ws:
                    self.received.append(json.loads(raw))
            except websockets.ConnectionClosed:
                pass

        self.server = await websockets.serve(handler, "127.0.0.1", 0)
        self.port = self.server.sockets[0].getsockname()[1]
        return self

    async def __aexit__(self, *_exc) -> None:
        self.server.close()
        await self.server.wait_closed()


async def run_against(frames: list[dict], expect: int) -> tuple[Skill, list[str]]:
    """Serve `frames` to a real Skill and return it with what its handler saw."""
    seen: list[str] = []
    done = asyncio.Event()
    async with Node(frames) as node:
        skill = Skill(manifest=MANIFEST, token="")
        skill._ws_url = f"ws://127.0.0.1:{node.port}"

        @skill.on("text_in")
        async def handle(ctx: Context) -> None:
            seen.append(ctx.payload["text"])
            if len(seen) >= expect:
                done.set()

        serving = asyncio.create_task(skill._serve())
        try:
            await asyncio.wait_for(done.wait(), 5)
        finally:
            serving.cancel()
            await asyncio.gather(serving, return_exceptions=True)
    return skill, seen


class RegistrationAnswer(unittest.TestCase):
    def test_the_usual_order(self):
        skill, seen = asyncio.run(run_against([ACK, delivery(1)], expect=1))
        self.assertEqual(seen, ["message 1"])
        self.assertEqual(skill.config.get("temperature"), 0.2)
        self.assertEqual(skill.grammars, {"text_out": "root ::= x"})

    def test_a_delivery_that_arrives_before_the_answer_is_not_lost(self):
        # A node up to 0.4.0 can do this under load. The delivery used to be
        # read as the answer and dropped, and the answer after it ignored.
        skill, seen = asyncio.run(run_against([delivery(1), ACK, delivery(2)], expect=2))
        self.assertEqual(seen, ["message 1", "message 2"])
        self.assertEqual(skill.config.get("temperature"), 0.2)
        self.assertEqual(skill.grammars, {"text_out": "root ::= x"})


if __name__ == "__main__":
    unittest.main()
