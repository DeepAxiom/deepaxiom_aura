"""
notify-alert — motor.notify.alert.

Final stage of the deepaxiom-vision-guard pipeline. This is the only motor.* skill in
the graph: the edge that feeds it (reasoner.alert_out -> notifier.alert_in,
see examples/vision/graph.json) carries "gate": "human-approval" — the
kernel holds the alert and asks a human to approve or deny before this
handler ever runs.

Always writes the alert to ./alerts/ so the demo is provably runnable with
zero external services. Optionally also calls a real Home Assistant notify
service (see README "Optional: wire it to Home Assistant") if HA_BASE_URL
and HA_TOKEN are set — this is the honest, minimal version of "act in the
real world": a webhook call, degrading silently to local-only if unset or
unreachable, never crashing the handler over a network hiccup.
"""
import json
import logging
import os
import re
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("notifier")

OUT_DIR = Path("alerts")
HA_BASE_URL = os.getenv("HA_BASE_URL")
HA_TOKEN = os.getenv("HA_TOKEN")

skill = Skill()


def _slug(text: str) -> str:
    first_line = text.strip().splitlines()[0] if text.strip() else "alert"
    slug = re.sub(r"[^a-z0-9]+", "-", first_line.lower()).strip("-")
    return slug[:48] or "alert"


def _notify_home_assistant(message: str) -> bool:
    if not (HA_BASE_URL and HA_TOKEN):
        return False
    body = json.dumps({"message": message, "title": "deepaxiom-vision-guard"}).encode("utf-8")
    req = urllib.request.Request(
        f"{HA_BASE_URL.rstrip('/')}/api/services/notify/notify",
        data=body,
        headers={
            "Authorization": f"Bearer {HA_TOKEN}",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=10):
            return True
    except (urllib.error.URLError, TimeoutError) as exc:
        log.warning("Home Assistant notify failed, alert still written locally: %s", exc)
        return False


@skill.on("alert_in")
async def handle(ctx: Context) -> None:
    alert = ((ctx.payload or {}).get("text") or "").strip()
    if not alert:
        return

    OUT_DIR.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    path = OUT_DIR / f"{stamp}-{_slug(alert)}.md"
    path.write_text(alert, encoding="utf-8")

    delivered_ha = _notify_home_assistant(alert)
    log.info("alert delivered: %s (home_assistant=%s)", path, delivered_ha)

    detail = f"written to {path}" + (" + sent to Home Assistant" if delivered_ha else "")
    await ctx.status("alert_out", "done", detail)


if __name__ == "__main__":
    skill.run()
