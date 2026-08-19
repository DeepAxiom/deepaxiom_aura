"""
model-fit — sensorial.hardware.modelfit.

Answers the question that comes before `model-manager` is any use: *which of
these can this machine actually run?* A catalogue of models is a list of things
to download; without a fit estimate it is a list of things to download and then
find out about.

Two actions:

    { "action": "probe" }            what this machine has
    { "action": "rank" }             every catalogue model, best fit first

Deliberately dependency-free (stdlib only, see hardware.py) so it starts on a
machine where nothing is installed yet — which is exactly the machine whose
owner needs to know what to install.

Run it from its own directory:

    cd skills/model-fit && PYTHONPATH=../../sdk/python/src python main.py
"""
import logging

import hardware
import sizing
from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("model-fit")

skill = Skill()


def _human(n: int | None) -> str:
    if not n:
        return "unknown"
    return f"{n / sizing.GIB:.1f} GiB"


@skill.on("command_in")
async def handle(ctx: Context) -> None:
    payload = ctx.payload or {}
    action = payload.get("action", "rank")

    try:
        hw = hardware.detect()

        if action == "probe":
            await ctx.emit("result_out", {"action": "probe", "hardware": hw})
            await ctx.done("result_out")
            return

        if action != "rank":
            await ctx.error("result_out", f"unknown action {action!r} — use probe or rank")
            return

        context = int(payload.get("context_window") or skill.config["context_window"])
        ranked = sizing.rank(hw, context)

        # The summary line exists because a ranked table of five is still a
        # table to read. "3 of 5 fit, the best is X" is the answer.
        runnable = [r for r in ranked if r["verdict"] != "too-big"]
        best = runnable[0]["model_id"] if runnable else None

        await ctx.emit("result_out", {
            "action": "rank",
            "hardware": hw,
            "summary": {
                "ram": _human(hw["ram_bytes"]),
                "vram": _human(hw["vram_bytes"]) if hw["vram_bytes"] else "none detected",
                "runnable": len(runnable),
                "total": len(ranked),
                "best": best,
            },
            "models": ranked,
        })
        await ctx.done("result_out")

    except Exception as exc:  # noqa: BLE001
        # A probe that cannot read the machine must say so rather than rank
        # against zeros, which would recommend nothing and look like an answer.
        log.exception("model-fit failed")
        await ctx.error("result_out", f"could not size this machine: {exc}")


if __name__ == "__main__":
    hw = hardware.detect()
    log.info("hardware: %s · %s cores · RAM %s · VRAM %s",
             hw["cpu"], hw["cpu_cores"], _human(hw["ram_bytes"]),
             _human(hw["vram_bytes"]) if hw["vram_bytes"] else "none")
    skill.run()
