"""Voice engines for motor.tts.speak, one module per backend.

Split out of main.py so each engine's SDK (`pyttsx3`, `piper`) is imported
lazily inside its own function, not at module load — a machine with neither
installed can still import this package and ../audio.py's pure PCM/WAV
helpers, which is what keeps test_audio.py runnable on the light CI lane.
"""
