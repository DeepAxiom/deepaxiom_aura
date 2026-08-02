"""
vision-reasoner — cognitive.vision.reasoner.

Takes one object-detection event (JSON, as produced by
examples/vision/bridge/bridge.py from Frigate or its --demo generator) and
decides two independent things:

  1. narration  — always. A short spoken sentence for motor.tts.speak, so
     the system can narrate what it sees even for routine events.
  2. alert      — only when the event looks actionable. This is advisory
     only: the actual write happens in ../notify-alert/main.py, and the edge
     feeding it (reasoner.alert_out -> notifier.alert_in, see
     examples/vision/graph.json) carries "gate": "human-approval". The model
     proposes; a human decides. Safety policy lives in the graph, not the
     prompt — same rule the planner skill (skills/planner/) uses for
     motor.* edges.

Same local-or-cloud ChatBackend as skills/llm-chat — see aura.llm.ChatBackend
for the OPENAI_API_KEY / OPENAI_BASE_URL toggle (Gemini via its
OpenAI-compatible endpoint, or any other OpenAI-compatible provider, without
touching this file).
"""
import json
import logging
import os

from aura import ChatBackend, Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("vision-reasoner")

SYSTEM_PROMPT = (
    "You are the perception-narration layer of a home/site monitoring "
    "assistant. You receive one JSON object-detection event (camera, label, "
    "zone, score, time, and sometimes sub_label or a note about how long the "
    "object lingered). Reply with STRICT JSON and nothing else, in this "
    'exact shape: {"narration": "<one short spoken sentence describing the '
    'event, present tense>", "alert": <null, or a short imperative string '
    'describing the recommended action if this event is actionable>}. '
    "An event is actionable only if it is unusual or risky for its context "
    "(e.g. a person in a restricted zone, a person lingering at an entry "
    "point late at night, a vehicle that isn't a known pattern) — routine "
    "events (a cat in the yard, a car passing on the street, a known "
    "delivery window) get a narration and alert: null. Reply in the same "
    "language the event's free-text fields are written in, defaulting to "
    "Spanish if none."
)

skill = Skill()
backend = ChatBackend(
    model_repo=os.getenv("AURA_MODEL_REPO", "Qwen/Qwen2.5-1.5B-Instruct-GGUF"),
    model_file=os.getenv("AURA_MODEL_FILE", "qwen2.5-1.5b-instruct-q4_k_m.gguf"),
)


def _parse_decision(raw: str) -> dict:
    """Fuzzy LLM output -> exact code: pull the JSON object out of the
    reply even if the model wrapped it in prose or a code fence, and fall
    back to treating the whole reply as narration if it still doesn't parse."""
    start, end = raw.find("{"), raw.rfind("}")
    if start != -1 and end != -1 and end > start:
        try:
            decision = json.loads(raw[start : end + 1])
            if isinstance(decision, dict) and "narration" in decision:
                return decision
        except json.JSONDecodeError:
            pass
    return {"narration": raw.strip(), "alert": None}


@skill.on("event_in")
async def handle(ctx: Context) -> None:
    event_text = ((ctx.payload or {}).get("text") or "").strip()
    if not event_text:
        return

    if not backend.is_cloud:
        await ctx.status("status_out", "working", "reasoning about the event (local model)...")

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": event_text},
    ]
    reply = "".join(backend.stream_chat(messages, max_tokens=256))
    decision = _parse_decision(reply)

    narration = (decision.get("narration") or "").strip() or "Detected an event with no description."
    await ctx.emit("narration_out", {"text": narration, "final": True})

    alert = decision.get("alert")
    if alert:
        log.info("proposing alert: %s", alert)
        await ctx.emit("alert_out", {"text": str(alert).strip(), "final": True})


if __name__ == "__main__":
    skill.run()
