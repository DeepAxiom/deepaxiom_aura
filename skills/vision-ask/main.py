"""
vision-ask — cognitive.vision.ask.

On-demand vision Q&A: "what's on the porch camera right now" — a live
snapshot, not a past event. This is a different capability from
../vision-reasoner (cognitive.vision.reasoner), which only reacts to
Frigate's proactive event stream and never sees an image; this skill fetches a real
frame and asks a vision-capable model about it, on demand, so
cognitive.planner can route a spoken question to it like any other skill.

No local fallback for vision, unlike every other cognitive skill in this
repository: skills/llm-chat and this repo's other reasoners always have
llama.cpp + Qwen2.5 to fall back on for text. There is no vision-capable
model in this repo's local stack, so this skill is cloud-only — it needs
OPENAI_API_KEY pointed at a real vision-capable model (Gemini or OpenAI) to
answer anything. Without one, it still connects and registers (so it's
visible in the catalog and explains itself instead of silently failing),
it just answers every question the same honest way.

Config (env):
  CAMERA_BASE_URL   default http://localhost:5000 (Frigate's real default
                     port; ./mock_camera.py serves the same path shape)
  DEFAULT_CAMERA    default "front_door" — used when the question doesn't
                     name one (see skill.yaml's description)
  OPENAI_API_KEY    required to actually answer — see above
  OPENAI_BASE_URL   default https://api.openai.com/v1
  OPENAI_MODEL      default gpt-4o-mini (vision-capable); for Gemini:
                     OPENAI_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai/
                     OPENAI_MODEL=gemini-3-flash (or any Gemini 3.x model)
"""
import base64
import json
import logging
import os
import urllib.error
import urllib.request

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("vision-qa")

CAMERA_BASE_URL = os.getenv("CAMERA_BASE_URL", "http://localhost:5000").rstrip("/")
DEFAULT_CAMERA = os.getenv("DEFAULT_CAMERA", "front_door")
KNOWN_CAMERAS = {"front_door", "driveway", "backyard", "side_gate", "street"}

API_KEY = os.getenv("OPENAI_API_KEY")
BASE_URL = os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1").rstrip("/")
MODEL = os.getenv("OPENAI_MODEL", "gpt-4o-mini")

skill = Skill()


def _parse_question(text: str) -> tuple[str, str]:
    """'front_door: is there a package?' -> ('front_door', 'is there a package?')."""
    if ":" in text:
        prefix, rest = text.split(":", 1)
        candidate = prefix.strip().lower()
        if candidate in KNOWN_CAMERAS and rest.strip():
            return candidate, rest.strip()
    return DEFAULT_CAMERA, text.strip()


def _fetch_snapshot(camera: str) -> bytes:
    url = f"{CAMERA_BASE_URL}/api/{camera}/latest.jpg"
    with urllib.request.urlopen(url, timeout=10) as resp:
        return resp.read()


def _ask_vision_model(camera: str, question: str, image_bytes: bytes) -> str:
    b64 = base64.b64encode(image_bytes).decode("ascii")
    body = json.dumps({
        "model": MODEL,
        "max_tokens": 300,
        "messages": [{
            "role": "user",
            "content": [
                {"type": "text", "text": f"Camera '{camera}'. Question: {question}"},
                {"type": "image_url", "image_url": {"url": f"data:image/jpeg;base64,{b64}"}},
            ],
        }],
    }).encode("utf-8")
    req = urllib.request.Request(
        f"{BASE_URL}/chat/completions",
        data=body,
        headers={"Authorization": f"Bearer {API_KEY}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        parsed = json.loads(resp.read())
    return parsed["choices"][0]["message"]["content"]


def _recover_text(payload: dict) -> str:
    """Fuzzy LLM -> exact code: a small local planner model sometimes
    extracts a structured {"camera": "front_door"} instead of putting the
    question in "text" (this skill's only declared ingress shape is
    std/text@1) — seen live with the local Qwen2.5-1.5B backend, since
    skill.yaml's description names "camera" prominently. Recover a usable
    question instead of silently doing nothing; a bigger planner model
    (cloud-backed) tends to get the shape right on the first try."""
    text = (payload.get("text") or "").strip()
    if text:
        return text
    camera = payload.get("camera") or payload.get("entity_id")
    question = payload.get("question") or payload.get("query")
    if camera or question:
        return f"{camera or DEFAULT_CAMERA}: {question or 'qué ves'}"
    return ""


@skill.on("question_in")
async def handle(ctx: Context) -> None:
    text = _recover_text(ctx.payload or {})
    if not text:
        return

    camera, question = _parse_question(text)

    if not API_KEY:
        await ctx.emit("answer_out", {
            "text": (
                "La visión bajo demanda necesita OPENAI_API_KEY apuntando a un "
                "modelo con capacidad de visión (Gemini o OpenAI) — no hay "
                "modelo local de respaldo para esto todavía."
            ),
            "final": True,
        })
        return

    await ctx.status("status_out", "working", f"mirando la cámara {camera}...")

    try:
        image_bytes = _fetch_snapshot(camera)
    except (urllib.error.URLError, TimeoutError) as exc:
        await ctx.emit("answer_out", {
            "text": f"No pude obtener una imagen de la cámara {camera}: {exc}",
            "final": True,
        })
        return

    answer = _ask_vision_model(camera, question, image_bytes)
    log.info("camera=%s question=%r -> %r", camera, question, answer)
    await ctx.emit("answer_out", {"text": answer, "final": True})


if __name__ == "__main__":
    skill.run()
