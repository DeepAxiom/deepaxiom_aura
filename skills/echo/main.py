"""
echo — logical.echo (the reference minimal skill).

This is the skill the C1 manifest specification uses as its worked example,
and the one the seeded `echo` graph resolves. It exists so a node with no
model downloaded, no API key and no network can still prove the whole path:
client → kernel → graph executor → skill → back.

Run it from the repo root:

    pip install -r skills/echo/requirements.txt
    PYTHONPATH=sdk/python/src python skills/echo/main.py
"""
import logging

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("echo")

skill = Skill()


@skill.on("text_in")
async def handle(ctx: Context) -> None:
    text = (ctx.payload or {}).get("text", "")
    await ctx.emit("text_out", {"text": skill.config["prefix"] + text, "final": True})


if __name__ == "__main__":
    skill.run()
