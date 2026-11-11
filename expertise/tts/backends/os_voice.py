"""OS voice backend — the degradation target when piper is not installed or
fails. See ../main.py's header for why this path cannot stream: `pyttsx3`
only exposes whole-file synthesis, so the clause is synthesised completely
and only then chunked. Same output contract as piper, worse latency.
"""
import logging
import tempfile
from pathlib import Path

from audio import _wav_to_pcm

log = logging.getLogger("tts.backends.os_voice")


def _os_voice(text: str, config: dict) -> tuple[bytes, int]:
    """Whole-file synthesis. Cannot be interrupted once started."""
    import pyttsx3

    engine = pyttsx3.init()
    wanted = (config["voice"] or "").lower()
    if wanted:
        for voice in engine.getProperty("voices"):
            if wanted in voice.name.lower() or wanted in voice.id.lower():
                engine.setProperty("voice", voice.id)
                break
    engine.setProperty("rate", config["rate"])
    with tempfile.TemporaryDirectory() as tmp:
        out = Path(tmp) / "speech.wav"
        engine.save_to_file(text, str(out))
        engine.runAndWait()
        try:
            engine.stop()
        except Exception:  # noqa: BLE001
            pass
        return _wav_to_pcm(out.read_bytes())
