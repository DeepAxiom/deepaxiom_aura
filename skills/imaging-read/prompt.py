"""What the reader is asked, and the shape it must answer in.

Kept apart from the transport for the reason every prompt should be: it is the
part a clinician would want to read before trusting the output, and it should
be possible to read it without reading an HTTP client.

Two things it does that are not style:

**It asks for a draft, never a diagnosis.** Not politeness -- the whole
arrangement downstream depends on it. The output reaches a record only when a
named clinician approves it at the gate, so a reader that writes as though it
had decided is a reader inviting somebody to sign without reading.

**It forbids inventing an image.** Every finding must name the instance it sits
on, from the list it was given. `findings.py` drops the ones that do not, and
this is the other half of that check: the model is told the rule, and the code
does not trust that it followed it.
"""
from __future__ import annotations

# The response shape, handed to Vertex so the model cannot answer prose where a
# rectangle belongs. `box_2d` is Gemini's own convention -- [ymin, xmin, ymax,
# xmax] over 0..1000 -- and it is asked for in the form the model is trained to
# produce rather than in ours; findings.py converts.
SCHEMA = {
    "type": "OBJECT",
    "properties": {
        "findings": {
            "type": "ARRAY",
            "items": {
                "type": "OBJECT",
                "properties": {
                    "label": {"type": "STRING"},
                    "note": {"type": "STRING"},
                    "instance_uid": {"type": "STRING"},
                    "box_2d": {"type": "ARRAY", "items": {"type": "INTEGER"}},
                    "confidence": {"type": "NUMBER"},
                },
                "required": ["label", "instance_uid", "box_2d"],
            },
        },
        "correlation": {"type": "STRING"},
        "impression": {"type": "STRING"},
        "limitations": {"type": "STRING"},
    },
    "required": ["findings", "limitations"],
}

_ES = """Eres un lector de imagen médica que prepara un BORRADOR para que un
médico con nombre y cédula lo revise, lo corrija y decida. No estás
diagnosticando y no estás decidiendo nada: lo que escribas no llega a ningún
expediente hasta que esa persona lo apruebe y lo firme.

Reglas:
1. Cada hallazgo va sobre UNA imagen y nombra su `instance_uid`, tomado de la
   lista que se te dio. No inventes identificadores.
2. `box_2d` encierra lo que estás señalando, en [ymin, xmin, ymax, xmax] sobre
   una rejilla de 0 a 1000. Si no puedes ubicarlo, no lo reportes como hallazgo:
   descríbelo en `correlation`.
3. Correlaciona las imágenes entre sí antes de concluir. Un hallazgo que sólo
   aparece en un corte y no en los vecinos merece decirse así.
4. Si hay informe previo, dilo cuando coincidas y dilo cuando no. No lo repitas.
5. `limitations` es obligatorio: qué no se puede afirmar con estas imágenes, qué
   haría falta. Si no ves nada, `findings` vacío es una respuesta correcta.
6. No inventes medidas, ni densidades, ni valores que no puedas leer.

Responde en español."""

_EN = """You are a medical imaging reader preparing a DRAFT for a named,
licensed clinician to review, correct and decide on. You are not diagnosing and
you are not deciding: nothing you write reaches a record until that person
approves and signs it.

Rules:
1. Every finding sits on ONE image and names its `instance_uid`, taken from the
   list you were given. Do not invent identifiers.
2. `box_2d` encloses what you are pointing at, as [ymin, xmin, ymax, xmax] on a
   0..1000 grid. If you cannot locate it, do not report it as a finding --
   describe it in `correlation`.
3. Correlate the frames against each other before concluding. A finding on one
   slice and not its neighbours deserves to be said that way.
4. When there is a report on file, say where you agree and say where you do not.
   Do not repeat it.
5. `limitations` is required: what cannot be said from these frames, and what
   would be needed. Seeing nothing is a correct answer -- return no findings.
6. Do not invent measurements, densities or values you cannot read.

Answer in English."""


def system(language: str) -> str:
    return _ES if language.startswith("es") else _EN


def question(payload: dict, uids: list[str], language: str) -> str:
    """The part that changes per study: what it is, what was asked, what is known."""
    spanish = language.startswith("es")
    lines = []

    def say(es: str, en: str, value: str) -> None:
        if value:
            lines.append(f"{es if spanish else en}: {value}")

    say("Modalidad", "Modality", str(payload.get("modality") or ""))
    say("Región", "Body part", str(payload.get("body_part") or ""))
    say("Estudio", "Study", str(payload.get("description") or ""))
    say("Pregunta clínica", "Clinical question", str(payload.get("question") or ""))
    say("Informe en expediente", "Report on file", str(payload.get("report") or ""))
    say("Estudio previo", "Prior study", str(payload.get("prior") or ""))

    lines.append("")
    lines.append(
        ("Imágenes, en orden. Usa estos identificadores:" if spanish
         else "Images, in order. Use these identifiers:"))
    for index, uid in enumerate(uids, start=1):
        lines.append(f"  {index}. {uid}")
    return "\n".join(lines)
