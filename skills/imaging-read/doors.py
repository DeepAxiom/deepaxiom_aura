"""Por qué puerta sale una lectura, y qué queda escrito de eso.

Google sirve los mismos modelos por dos entradas, y no son intercambiables para
quien despliega esto:

- **La API de Gemini** (`generativelanguage.googleapis.com`), que abre una
  ``GEMINI_API_KEY``. Es la que casi todo el mundo usa y la más simple de poner
  en marcha. **No está cubierta por el acuerdo de tratamiento de datos (BAA) de
  Google**, así que un despliegue que la elija está decidiendo que estas
  imágenes salen hacia un tercero sin ese contrato detrás.
- **Vertex AI**, sobre un proyecto con el acuerdo en vigor y credenciales de
  aplicación.

**La elección es de quien despliega, no de este archivo.** Lo que sí es de este
archivo: que la elección sea explícita —una variable u otra, nunca una por
omisión silenciosa— y que **cuál contestó quede en la atestación C5** junto al
modelo. Un año después, la pregunta «¿por dónde salió esta imagen?» tiene que
tener respuesta en el registro y no en la memoria de alguien.

Lo que se manda es un fotograma renderizado: sin etiquetas DICOM, y con lo que
el aparato escribió encima ya recortado por quien lo envía. No lo vuelve
anónimo --una imagen puede identificar por sí sola-- pero sí quita el nombre,
el expediente y las fechas.
"""
from __future__ import annotations

import logging

log = logging.getLogger("imaging-read")

# The two doors, by the name that appears in the attestation.
GEMINI = "gemini-api"
VERTEX = "vertex-ai"


class NoReader(RuntimeError):
    """No door is configured, or the one configured cannot be used."""


def ask(door: str, model: str, key: str, system: str, parts: list, schema: dict,
        temperature: float, thinking: str, timeout: float = 180.0) -> str:
    """One read through the Gemini API, as text.

    `parts` is the SDK's content list: images first, the question last. What
    comes back is the JSON the response format asked for, unparsed -- reading it
    is `findings.py`'s job, and it is the same job whichever door answered.
    """
    if door != GEMINI:
        raise NoReader(f"puerta desconocida: {door}")
    if not key:
        raise NoReader(
            "este nodo no tiene lector de imagen configurado: falta la llave de la "
            "API de Gemini")
    try:
        from google import genai  # type: ignore
    except ImportError as err:  # pragma: no cover - depends on the install
        raise NoReader("falta google-genai en este nodo, así que no hay con qué leer") from err

    client = genai.Client(api_key=key)
    try:
        interaction = client.interactions.create(
            model=model,
            system_instruction=system,
            input=parts,
            response_format={
                "type": "text",
                "mime_type": "application/json",
                "schema_": schema,
            },
            generation_config={
                "temperature": temperature,
                "thinking_level": thinking,
            },
            # Que no se guarde del otro lado. `store` decide si la petición y su
            # respuesta quedan disponibles para recuperarlas después, y lo que
            # va en esta petición son imágenes de una persona: lo que no hace
            # falta guardar, no se guarda.
            store=False,
            # Sin herramientas. La búsqueda web mandaría el contenido a otro
            # sistema y no ayuda: lo que se pregunta es qué se ve en estas
            # imágenes, no qué dice internet.
            tools=[],
            timeout=timeout,
        )
    except NoReader:
        raise
    except Exception as err:  # noqa: BLE001 - the SDK raises its own hierarchy
        raise NoReader(f"el lector no contestó: {err}") from err

    return _text_of(interaction)


def _text_of(interaction) -> str:
    """The text of the last step, however this SDK version shapes it.

    Defensive on purpose: the answer travels through three optional shapes
    depending on version --`output_text`, a step with content blocks, a plain
    string-- and a reader that assumed one of them would break on an upgrade
    with a stack trace instead of a sentence.
    """
    direct = getattr(interaction, "output_text", None)
    if isinstance(direct, str) and direct.strip():
        return direct

    steps = getattr(interaction, "steps", None) or []
    for step in reversed(list(steps)):
        content = getattr(step, "content", None)
        if isinstance(content, str) and content.strip():
            return content
        for block in list(content or []):
            text = getattr(block, "text", None)
            if isinstance(text, str) and text.strip():
                return text
    log.warning("la respuesta del lector no traía texto que leer")
    return ""
