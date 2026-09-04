"""What the model answered, turned into something a viewer can draw.

Two jobs, and both are about not trusting the shape of the answer.

**Coordinates.** Gemini returns boxes as ``[ymin, xmin, ymax, xmax]`` on a
0..1000 grid. ``std/imaging-finding@1`` carries a normalised rectangle with an
origin and a size. Converting is four lines; getting it wrong draws the finding
somewhere else on the image, which is the one failure a clinician cannot catch
by reading the text.

**Naming the image.** A finding that names an instance the request never sent
is dropped. The model is asked to echo the identifier back, and it can echo one
that does not exist -- a box on the wrong image is worse than no box.
"""
from __future__ import annotations

import json
import logging

log = logging.getLogger("imaging-read")

# The grid Gemini normalises boxes onto.
GRID = 1000.0


def clamp(value: float) -> float:
    return max(0.0, min(1.0, value))


def rectangle(raw: object) -> dict | None:
    """A normalised rectangle from whatever the model put in `box_2d`."""
    if not isinstance(raw, (list, tuple)) or len(raw) != 4:
        return None
    try:
        ymin, xmin, ymax, xmax = (float(v) / GRID for v in raw)
    except (TypeError, ValueError):
        return None
    # Ordered rather than assumed: a swapped pair produces a negative size, and
    # a negative size is a rectangle a renderer draws inside out or not at all.
    top, bottom = sorted((clamp(ymin), clamp(ymax)))
    left, right = sorted((clamp(xmin), clamp(xmax)))
    if bottom - top <= 0 or right - left <= 0:
        return None
    # Redondeado a diezmilesimas: una imagen no tiene diez mil pixeles de lado,
    # asi que eso es sub-pixel en cualquier pantalla, y sin redondear un ancho
    # sale como 0.19999999999999998 -- que viaja, se guarda y se lee asi.
    return {
        "x": round(left, 4),
        "y": round(top, 4),
        "w": round(right - left, 4),
        "h": round(bottom - top, 4),
    }


def read(text: str, known: set[str], limit: int, language: str) -> dict:
    """Turn the model's JSON into `std/imaging-finding@1`.

    Never raises for a badly shaped answer: a read that could not be parsed is
    a read with no findings and a limitation that says so, which is what a
    clinician needs to see. Raising would produce an error where a sentence
    belongs.
    """
    empty = {
        "findings": [],
        "correlation": "",
        "impression": "",
        "limitations": _cannot_parse(language),
    }
    if not text.strip():
        return empty
    try:
        answer = json.loads(text)
    except json.JSONDecodeError:
        log.warning("the reader answered something that is not JSON")
        return empty
    if not isinstance(answer, dict):
        return empty

    out = []
    for raw in (answer.get("findings") or [])[:limit]:
        if not isinstance(raw, dict):
            continue
        instance = str(raw.get("instance_uid") or "").strip()
        if instance not in known:
            log.warning("a finding named an image that was never sent; dropped")
            continue
        box = rectangle(raw.get("box_2d"))
        label = str(raw.get("label") or "").strip()
        if not box or not label:
            continue
        finding = {"label": label[:120], "instance_uid": instance, "box": box}
        if note := str(raw.get("note") or "").strip():
            finding["note"] = note[:2000]
        if isinstance(raw.get("frame"), int) and raw["frame"] >= 1:
            finding["frame"] = raw["frame"]
        confidence = raw.get("confidence")
        if isinstance(confidence, (int, float)):
            finding["confidence"] = clamp(float(confidence))
        out.append(finding)

    limitations = str(answer.get("limitations") or "").strip()
    return {
        "findings": out,
        "correlation": str(answer.get("correlation") or "").strip()[:4000],
        "impression": str(answer.get("impression") or "").strip()[:4000],
        # Never empty: the schema requires it, and a read that claims no
        # limitation at all is a read claiming more than it can.
        "limitations": (limitations or _nothing_stated(language))[:2000],
    }


def _cannot_parse(language: str) -> str:
    if language.startswith("es"):
        return "El lector no devolvió una lectura utilizable, así que no hay hallazgos que mostrar."
    return "The reader did not return a usable read, so there are no findings to show."


def _nothing_stated(language: str) -> str:
    if language.startswith("es"):
        return "El lector no declaró sus limitaciones, que ya es una limitación."
    return "The reader stated no limitations, which is itself one."
