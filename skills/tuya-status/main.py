"""
tuya-status — sensorial.api.tuya.status.

Reads the live status of a Tuya-connected device (light, plug, switch) by
name. Talks directly to Tuya's Cloud API — not through Home Assistant. As a
reseller with your own Tuya IoT Platform project (client_id/client_secret,
see the OEM App / App SDK docs), this reads devices under *your* project
directly, the same way your own white-label app would.

Every business call needs Tuya's HMAC-SHA256 request signature
(https://developer.tuya.com/en/docs/iot/new-singnature) — see _sign() below.
Read-only (sensorial), so it's always live: no human-approval gate.

Config (env):
  TUYA_BASE_URL       default http://localhost:8124 (examples/smart-home/tuya/
                       mock_tuya_cloud.py's default port — a same-shape
                       stand-in for Tuya's real Cloud API, including real
                       signature verification). Point at your data center's
                       real endpoint (e.g. https://openapi.tuyaus.com) when
                       you have a real Tuya IoT Platform project.
  TUYA_CLIENT_ID      required for a real project (mock accepts anything)
  TUYA_CLIENT_SECRET  required for a real project (mock accepts anything)
  TUYA_DEVICE_MAP     "alias:device_id,alias:device_id" — overrides the
                       built-in aliases below, which match
                       mock_tuya_cloud.py's fake devices
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
log = logging.getLogger("tuya-status")

BASE_URL = os.getenv("TUYA_BASE_URL", "http://localhost:8124").rstrip("/")
CLIENT_ID = os.getenv("TUYA_CLIENT_ID", "mock-client-id")
CLIENT_SECRET = os.getenv("TUYA_CLIENT_SECRET", "mock-client-secret")

DEVICES = {"cocina": "mock-dev-cocina", "sala": "mock-dev-sala", "ventilador": "mock-dev-ventilador"}
if os.getenv("TUYA_DEVICE_MAP"):
    DEVICES = {
        alias.strip(): device_id.strip()
        for alias, device_id in (pair.split(":", 1) for pair in os.environ["TUYA_DEVICE_MAP"].split(","))
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


def _recover_name(payload: dict) -> str:
    """Fuzzy LLM -> exact code: the local planner model sometimes extracts a
    structured {"body": {"name": "cocina"}} (an HTTP-call shape, params+body)
    instead of putting it in "text" (this skill's only declared ingress
    shape is std/text@1) — same family of quirk already seen and documented
    in ../vision-ask/main.py's _recover_text(), verified live with
    the local Qwen2.5-1.5B backend. A cloud-backed planner tends to get the
    shape right on the first try."""
    candidates = [payload, payload.get("body") or {}, payload.get("params") or {}]
    for c in candidates:
        text = (c.get("text") or "").strip()
        if text:
            return text.lower()
        for key in ("device", "name", "entity", "entity_id", "device_name"):
            if c.get(key):
                return str(c[key]).strip().lower()
    return ""


@skill.on("device_in")
async def handle(ctx: Context) -> None:
    name = _recover_name(ctx.payload or {})
    device_id = DEVICES.get(name)
    if device_id is None:
        known = ", ".join(sorted(DEVICES))
        await ctx.emit("status_out", {
            "text": f"No conozco un dispositivo llamado {name!r}. Conocidos: {known}.",
            "final": True,
        })
        return

    await ctx.status("progress_out", "working", f"consultando estado de {name}...")
    try:
        parsed = _signed_request("GET", f"/v1.0/iot-03/devices/{device_id}/status")
    except (urllib.error.URLError, TimeoutError, KeyError) as exc:
        await ctx.emit("status_out", {"text": f"No pude consultar {name}: {exc}", "final": True})
        return

    if not parsed.get("success", True):
        await ctx.emit("status_out", {
            "text": f"Tuya respondió con error para {name}: {parsed.get('msg', parsed)}",
            "final": True,
        })
        return

    points = parsed.get("result", [])
    summary = ", ".join(f"{p['code']}={p['value']}" for p in points) or "sin datos"
    log.info("device=%s status=%s", name, summary)
    await ctx.emit("status_out", {"text": f"{name}: {summary}", "final": True})


if __name__ == "__main__":
    skill.run()
