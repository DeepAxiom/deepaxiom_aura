"""imaging-read — cognitive.imaging.read.

Reads a sample of frames from one imaging study and answers with findings that
name the image they sit on and a rectangle over it, a correlation against the
report on file, a draft impression and what it could not tell.

**Cognitive, never motor.** Nothing here writes. The draft becomes a line in a
record only through a `motor.*` effect that a named clinician approves at the
gate, and the C5 attestation emitted with the answer is what makes that effect
able to say on what basis it happened -- which model, which revision, over
which prompt. Without it the ledger can say who signed and not what they were
shown.

**It reads through the Gemini API**, and that is a deployment decision with a
consequence worth naming once: that endpoint is not covered by Google's data
processing agreement. So the engine goes into the C5 attestation of every read
-- `gemini-api` -- and a year later "which way did this image leave" is
answerable from the record rather than from somebody's memory. Vertex was the
other way and is not built: see the roadmap for what would bring it back.

**It refuses rather than guesses**, and each refusal is a sentence rather than a
code: no key configured, or frames nobody de-identified -- it cannot check
pixels, and being the place where nobody checked is worse than declining.

Config (env, credentials only -- never in the skill's C1 config, which is
readable from the control plane):
  GEMINI_API_KEY                   the key this reader answers with
"""
import base64
import logging
import os

import findings as reading
import prompt as asking
import reader
from aura import Attestation, Context, Skill, run_all, sha256_text

# El nivel se puede subir sin tocar el codigo. Un lector que no dice nada
# mientras trabaja es un lector que no se puede diagnosticar en un despliegue.
logging.basicConfig(level=os.getenv("AURA_LOG_LEVEL", "INFO").upper())
log = logging.getLogger("imaging-read")

skill = Skill()




@skill.on("study_in")
async def handle(ctx: Context) -> None:
    payload = ctx.payload or {}
    frames = payload.get("frames") or []
    language = str(skill.config.get("language", "es"))
    # Un renglon al empezar, porque una lectura son segundos de espera y un
    # operador que no ve nada no puede distinguir «pensando» de «colgado».
    log.info("estudio recibido: %d imagenes, modalidad %s",
             len(frames), payload.get("modality") or "?")

    if not frames:
        await ctx.error("status_out", "no llegó ninguna imagen que leer")
        return
    # A statement by the sender, and the only one this skill takes on trust --
    # loudly, because an ultrasound carries the patient's name burned into the
    # pixels and no amount of tag scrubbing removes it.
    if payload.get("deidentified") is not True:
        await ctx.error(
            "status_out",
            "estas imágenes no vienen marcadas como des-identificadas, y este "
            "lector no puede comprobar los píxeles: quien las manda es quien "
            "quita el nombre, incluido el que está quemado en la imagen")
        return

    key = os.getenv("GEMINI_API_KEY", "").strip()
    model = setting("model", "IMAGING_MODEL", "models/gemini-3-flash-preview")
    if not key:
        await ctx.error(
            "status_out",
            "este nodo no tiene lector de imagen configurado: falta la llave de la "
            "API de Gemini")
        return

    uids, parts = [], []
    for index, frame in enumerate(frames, start=1):
        uid = str(frame.get("instance_uid") or "").strip() or f"frame-{index}"
        uids.append(uid)
        parts.append({
            "type": "image",
            "mime_type": str(frame.get("mime") or "image/jpeg"),
            "data": str(frame.get("bytes_b64") or ""),
        })

    # La pregunta va **al final**, despues de las imagenes: un lector al que se
    # le dice que buscar antes de ensenarle nada busca eso y no lo que hay.
    asked = asking.question(payload, uids, language)
    parts.append({"type": "text", "text": asked})

    temperature = float(skill.config.get("temperature", 0.0))
    await ctx.status("status_out", "working", f"leyendo {len(frames)} imágenes")
    try:
        text = await _call(model, key, asking.system(language), parts, temperature)
    except reader.NoReader as err:
        await ctx.error("status_out", str(err))
        return
    out = reading.read(text, set(uids), int(skill.config.get("max_findings", 12)), language)

    # C5. The prompt hash covers the words and the identifiers, not the pixels:
    # hashing megabytes of image on every read buys nothing the frame list does
    # not already pin down, and the envelope the kernel sealed carries them.
    await ctx.emit(
        "finding_out", out,
        attest=Attestation(
            # El motor, en el registro. Es la diferencia entre «un modelo de
            # Google leyo esto» y «esta imagen salio por un endpoint sin
            # acuerdo de tratamiento de datos», y la segunda es la que alguien
            # va a querer poder responder.
            engine=reader.ENGINE,
            model=model,
            params={
                "temperature": temperature,
                "frames": len(frames),
            },
            prompt_sha256=sha256_text(asking.system(language) + "\n" + asked),
            output_sha256=sha256_text(text),
        ),
    )
    log.info("lectura terminada: %d hallazgos", len(out["findings"]))
    await ctx.status("status_out", "ready", f"{len(out['findings'])} hallazgos")


def setting(key: str, variable: str, fallback: str) -> str:
    """C1 config first, then the environment, then the declared default.

    The config is the right place and the control plane is how it is normally
    set. The environment is here because a container has no control plane on
    first boot, and asking somebody to `PUT /v1/skills/config` before a node can
    read anything is asking them to configure a thing twice.

    Never a credential: `GOOGLE_APPLICATION_CREDENTIALS` names a file that
    google-auth reads, and nothing in this process ever holds its contents. A
    project id is not a secret; a key is, and a key in a config field is a key
    in the control plane's UI and in its audit log.
    """
    value = str(skill.config.get(key, "") or "").strip()
    if value:
        return value
    return os.getenv(variable, "").strip() or fallback


async def _call(model: str, key: str, system: str, parts: list,
                temperature: float) -> str:
    """The blocking request, off the event loop.

    A read is seconds of somebody else's compute, and the kernel's connection
    has other traffic on it -- an approval that queues behind an inference is
    the failure the whole separation of concerns exists to avoid.
    """
    import asyncio

    return await asyncio.to_thread(
        reader.ask, model, key, system, parts, asking.SCHEMA,
        temperature, str(skill.config.get("thinking", "high")))


if __name__ == "__main__":
    run_all([skill])
