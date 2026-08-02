"""
ocr — sensorial.ocr.image (model plane driver: ONNX Runtime via RapidOCR).

Input:  std/document@1 with image bytes ({"mime": "image/png", "bytes_b64": ...})
Output: std/text@1 with the recognized text (reading order).
"""
import asyncio
import base64
import logging

from aura import Context, Skill

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("ocr")

skill = Skill()
_engine = None


def _load():
    global _engine
    if _engine is None:
        from rapidocr_onnxruntime import RapidOCR
        log.info("loading RapidOCR (ONNX Runtime, CPU) ...")
        _engine = RapidOCR()
        log.info("engine ready")
    return _engine


def _recognize(image_bytes: bytes) -> str:
    engine = _load()
    result, _elapse = engine(image_bytes)
    if not result:
        return ""
    return "\n".join(text for _box, text, _score in result)


@skill.on("image_in")
async def handle(ctx: Context) -> None:
    payload = ctx.payload or {}
    b64 = payload.get("bytes_b64", "")
    if not b64:
        await ctx.error("status_out", "image_in expects std/document@1 with bytes_b64")
        return
    if _engine is None:
        await ctx.status("status_out", "working", "loading OCR models...")
    try:
        text = await asyncio.to_thread(_recognize, base64.b64decode(b64))
        await ctx.emit("text_out", {"text": text, "final": True})
    except ImportError:
        await ctx.error("status_out",
                        "rapidocr-onnxruntime is not installed (pip install rapidocr-onnxruntime)")
    except Exception as exc:  # noqa: BLE001
        await ctx.error("status_out", f"recognition failed: {exc}")


if __name__ == "__main__":
    skill.run()
