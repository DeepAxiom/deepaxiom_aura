"""
tuya-command — motor.api.tuya.command.

Turns a Tuya-connected device on/off by name. The only motor.* skill in
this piece — cognitive.planner auto-gates every edge into a motor skill
behind human approval (see skills/planner/main.py, "policy: acting requires
human approval"), so nothing reaches a real device without an explicit yes,
the same mechanism motor.notify.alert uses elsewhere in this repo.

Talks directly to Tuya's Cloud API using the same HMAC-SHA256 signing as
../tuya-status/main.py (duplicated rather than shared, on purpose — every
skill in this repo is meant to be a standalone file/folder, readable on its
own).

Config (env): same as ../tuya-status/main.py — TUYA_BASE_URL, TUYA_CLIENT_ID,
TUYA_CLIENT_SECRET, TUYA_DEVICE_MAP.
"""
import hashlib
import hmac
import json
import logging
import os
import time
import urllib.error
import urllib.request

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("tuya-command")

BASE_URL = os.getenv("TUYA_BASE_URL", "http://localhost:8124").rstrip("/")
CLIENT_ID = os.getenv("TUYA_CLIENT_ID", "mock-client-id")
CLIENT_SECRET = os.getenv("TUYA_CLIENT_SECRET", "mock-client-secret")

DEVICES = {"cocina": "mock-dev-cocina", "sala": "mock-dev-sala", "ventilador": "mock-dev-ventilador"}
if os.getenv("TUYA_DEVICE_MAP"):
    DEVICES = {
        alias.strip(): device_id.strip()
        for alias, device_id in (pair.split(":", 1) for pair in os.environ["TUYA_DEVICE_MAP"].split(","))
    }

ACTIONS = {
    "on": True, "encender": True, "prender": True, "activar": True, "turn_on": True, "true": True,
    "off": False, "apagar": False, "desactivar": False, "turn_off": False, "false": False,
}

skill = Skill()
_token_cache: dict = {}


def _sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _sign(payload: str) -> str:
    return hmac.new(CLIENT_SECRET.encode(), payload.encode(), hashlib.sha256).hexdigest().upper()


def _string_to_sign(method: str, body: bytes, path_and_query: str) -> str:
    return f"{method}\n{_sha256_hex(body)}\n\n{path_and_query}"


def _get_token() -> str:
    now = time.time()
    if _token_cache.get("expires_at", 0) > now:
        return _token_cache["access_token"]

    t = str(int(now * 1000))
    path = "/v1.0/token?grant_type=1"
    sign = _sign(f"{CLIENT_ID}{t}{_string_to_sign('GET', b'', path)}")
    req = urllib.request.Request(
        f"{BASE_URL}{path}",
        headers={"client_id": CLIENT_ID, "sign": sign, "t": t, "sign_method": "HMAC-SHA256"},
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        parsed = json.loads(resp.read())
    result = parsed["result"]
    _token_cache["access_token"] = result["access_token"]
    _token_cache["expires_at"] = now + result.get("expire_time", 7200) - 60
    return result["access_token"]


def _signed_request(method: str, path: str, body: dict | None = None) -> dict:
    access_token = _get_token()
    body_bytes = json.dumps(body).encode("utf-8") if body is not None else b""
    t = str(int(time.time() * 1000))
    sign = _sign(f"{CLIENT_ID}{access_token}{t}{_string_to_sign(method, body_bytes, path)}")
    req = urllib.request.Request(
        f"{BASE_URL}{path}",
        data=body_bytes if body is not None else None,
        method=method,
        headers={
            "client_id": CLIENT_ID,
            "access_token": access_token,
            "sign": sign,
            "t": t,
            "sign_method": "HMAC-SHA256",
            "Content-Type": "application/json",
        },
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read())


def _parse(text: str) -> tuple[str, str, bool] | None:
    if ":" not in text:
        return None
    name, action = text.split(":", 1)
    name = name.strip().lower()
    device_id = DEVICES.get(name)
    value = ACTIONS.get(action.strip().lower())
    if device_id is None or value is None:
        return None
    return name, device_id, value


def _recover_text(payload: dict) -> str:
    """Fuzzy LLM -> exact code: the local planner model sometimes extracts a
    structured {"body": {"name": "cocina", "action": "encender"}} (an
    HTTP-call shape, params+body) instead of 'cocina: encender' in "text"
    (this skill's only declared ingress shape is std/text@1) — same family
    of quirk already seen and documented in
    ../vision-ask/main.py's _recover_text(), verified live with the
    local Qwen2.5-1.5B backend. A cloud-backed planner tends to get the
    shape right on the first try."""
    candidates = [payload, payload.get("body") or {}, payload.get("params") or {}]
    for c in candidates:
        text = (c.get("text") or "").strip()
        if text:
            return text
        device = c.get("device") or c.get("name") or c.get("entity_id") or c.get("device_name")
        action = c.get("action") or c.get("command") or c.get("value") or c.get("state")
        if device and action:
            return f"{device}: {action}"
    return ""


@skill.on("command_in")
async def handle(ctx: Context) -> None:
    text = _recover_text(ctx.payload or {})
    parsed = _parse(text)
    if parsed is None:
        known = ", ".join(sorted(DEVICES))
        await ctx.emit("result_out", {
            "text": f"No entendí {text!r}. Formato: 'dispositivo: encender' o 'dispositivo: apagar'. Conocidos: {known}.",
            "final": True,
        })
        return
    name, device_id, value = parsed

    try:
        result = _signed_request(
            "POST", f"/v1.0/iot-03/devices/{device_id}/commands",
            {"commands": [{"code": "switch_1", "value": value}]},
        )
    except (urllib.error.URLError, TimeoutError) as exc:
        await ctx.emit("result_out", {"text": f"No pude enviar el comando a {name}: {exc}", "final": True})
        return

    if not result.get("success", True):
        await ctx.emit("result_out", {
            "text": f"Tuya rechazó el comando para {name}: {result.get('msg', result)}",
            "final": True,
        })
        return

    state = "encendido" if value else "apagado"
    log.info("device=%s -> switch_1=%s", name, value)
    await ctx.emit("result_out", {"text": f"{name} ahora está {state}.", "final": True})


if __name__ == "__main__":
    skill.run()
