"""The call to Vertex AI, and the reason it is Vertex and not the other door.

Google serves the same Gemini models through two endpoints. The Generative
Language API — ``generativelanguage.googleapis.com``, the one an AI Studio key
opens — is **not covered by Google's HIPAA BAA**, and neither is AI Studio
itself. Vertex AI on a project with the BAA executed is. What this skill sends
is a person's imaging, so the other door is not an option and is refused by
name rather than left off a list somebody could add to.

Credentials come from Application Default Credentials, the way every other
Google client on a node gets them: a service account file named by
``GOOGLE_APPLICATION_CREDENTIALS``, or the metadata server when the node runs on
Google Cloud. Nothing here reads a key out of the skill's config -- a
credential in a config field is a credential in the control plane's UI, in its
audit log and in a screenshot.
"""
from __future__ import annotations

import json
import logging
import urllib.error
import urllib.request

log = logging.getLogger("imaging-read")

# The endpoint that must not be used, kept here so the refusal can name it.
FORBIDDEN_HOST = "generativelanguage.googleapis.com"

SCOPE = "https://www.googleapis.com/auth/cloud-platform"


class NoReader(RuntimeError):
    """No reader is configured, or the one configured cannot be used."""


def endpoint(project: str, location: str, model: str) -> str:
    """The regional Vertex address for one model."""
    if not project:
        raise NoReader(
            "este nodo no tiene lector de imagen configurado: falta el proyecto de "
            "Google Cloud con el acuerdo de tratamiento de datos en vigor")
    host = f"{location}-aiplatform.googleapis.com"
    return (f"https://{host}/v1/projects/{project}/locations/{location}"
            f"/publishers/google/models/{model}:generateContent")


def token() -> str:
    """A bearer token for Vertex, from Application Default Credentials."""
    try:
        import google.auth  # type: ignore
        from google.auth.transport.requests import Request  # type: ignore
    except ImportError as err:  # pragma: no cover - depends on the install
        raise NoReader(
            "falta google-auth en este nodo, así que no hay cómo autenticarse "
            "contra Vertex AI") from err

    credentials, _ = google.auth.default(scopes=[SCOPE])
    credentials.refresh(Request())
    if not credentials.token:
        raise NoReader("las credenciales de Google no entregaron un token")
    return str(credentials.token)


def ask(url: str, bearer: str, body: dict, timeout: float = 120.0) -> dict:
    """One request, with the refusal that keeps the wrong endpoint unreachable."""
    if FORBIDDEN_HOST in url:
        raise NoReader(
            f"{FORBIDDEN_HOST} no está cubierto por el acuerdo de tratamiento de "
            "datos de Google, y lo que va en esta petición es imagen de una "
            "persona. Se usa Vertex AI sobre un proyecto con BAA.")

    request = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + bearer},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as answer:
            return json.loads(answer.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        detail = err.read().decode("utf-8", "replace")[:400]
        raise NoReader(f"Vertex AI contestó {err.code}: {detail}") from err
    except urllib.error.URLError as err:
        raise NoReader(f"no se pudo alcanzar Vertex AI: {err.reason}") from err


def text_of(answer: dict) -> str:
    """The one text part of a response, or nothing.

    Nothing is not an error here: a model that returned no candidate has said
    it has nothing, and the caller turns that into a read with no findings and
    a stated limitation rather than into a crash.
    """
    for candidate in answer.get("candidates") or []:
        for part in (candidate.get("content") or {}).get("parts") or []:
            if isinstance(part.get("text"), str) and part["text"].strip():
                return part["text"]
    return ""
