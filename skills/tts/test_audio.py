"""Tests for the pure PCM/WAV helpers in audio.py.

Deliberately imports only `audio`, never `main` or anything under
`backends/` — the whole point of splitting audio.py out is that it has no
`pyttsx3`/`piper` dependency, so this file has to prove that by staying
import-clean itself. If this ever grows a `import main` or an import that
drags in a backend, it stops being able to run on the light CI lane that has
neither optional TTS dependency installed.

    cd skills/tts && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import unittest

from audio import _pcm_chunks, _pcm_to_wav, _wav_to_pcm


def _silence(num_samples: int) -> bytes:
    """num_samples frames of 16-bit mono silence."""
    return b"\x00\x00" * num_samples


class PcmChunks(unittest.TestCase):
    def test_splits_into_chunk_ms_pieces_at_a_given_rate(self):
        # 1 second of 16kHz mono 16-bit PCM (32000 bytes), chunked at 200ms
        # -> 5 pieces of 6400B (3200 samples/chunk * 2 bytes/sample).
        pcm = _silence(16000)
        chunks = _pcm_chunks(pcm, sample_rate=16000, chunk_ms=200)

        self.assertEqual(len(chunks), 5)
        self.assertTrue(all(len(c) == 6400 for c in chunks))
        self.assertEqual(b"".join(chunks), pcm)

    def test_a_larger_chunk_ms_yields_fewer_bigger_pieces(self):
        pcm = _silence(16000)
        chunks_small = _pcm_chunks(pcm, sample_rate=16000, chunk_ms=100)
        chunks_large = _pcm_chunks(pcm, sample_rate=16000, chunk_ms=1000)

        self.assertGreater(len(chunks_small), len(chunks_large))
        self.assertEqual(b"".join(chunks_small), b"".join(chunks_large))

    def test_a_different_sample_rate_changes_bytes_per_chunk_not_count(self):
        # Same duration (1s), different rate -> same number of chunks at a
        # fixed chunk_ms, but each chunk carries more bytes at the higher rate.
        pcm_16k = _silence(16000)
        pcm_22k = _silence(22050)

        chunks_16k = _pcm_chunks(pcm_16k, sample_rate=16000, chunk_ms=200)
        chunks_22k = _pcm_chunks(pcm_22k, sample_rate=22050, chunk_ms=200)

        self.assertEqual(len(chunks_16k), len(chunks_22k))
        self.assertGreater(len(chunks_22k[0]), len(chunks_16k[0]))

    def test_odd_length_input_does_not_lose_the_remainder(self):
        # A tail shorter than a full chunk still has to come out somewhere.
        pcm = _silence(100) + b"\x01\x02"  # 100 whole frames + 1 stray byte
        chunks = _pcm_chunks(pcm, sample_rate=16000, chunk_ms=200)

        self.assertEqual(b"".join(chunks), pcm)

    def test_empty_pcm_yields_no_chunks(self):
        self.assertEqual(_pcm_chunks(b"", sample_rate=16000, chunk_ms=200), [])

    def test_chunk_ms_too_small_for_one_frame_still_advances(self):
        # size is clamped to at least 2 bytes (one 16-bit frame) so this
        # cannot regress into an infinite loop or a zero-length slice.
        pcm = _silence(10)
        chunks = _pcm_chunks(pcm, sample_rate=16000, chunk_ms=0)

        self.assertEqual(len(chunks), 10)
        self.assertTrue(all(len(c) == 2 for c in chunks))


class WavRoundtrip(unittest.TestCase):
    def test_pcm_to_wav_to_pcm_is_lossless(self):
        pcm = _silence(8000) + b"\x10\x20" * 100
        wav_bytes = _pcm_to_wav(pcm, sample_rate=16000)

        recovered_pcm, recovered_rate = _wav_to_pcm(wav_bytes)

        self.assertEqual(recovered_pcm, pcm)
        self.assertEqual(recovered_rate, 16000)

    def test_wav_header_carries_mono_16bit_format(self):
        import io
        import wave

        wav_bytes = _pcm_to_wav(_silence(100), sample_rate=22050)
        with wave.open(io.BytesIO(wav_bytes), "rb") as w:
            self.assertEqual(w.getnchannels(), 1)
            self.assertEqual(w.getsampwidth(), 2)
            self.assertEqual(w.getframerate(), 22050)

    def test_different_sample_rates_survive_the_roundtrip(self):
        for rate in (8000, 16000, 22050, 44100, 48000):
            with self.subTest(rate=rate):
                wav_bytes = _pcm_to_wav(_silence(10), sample_rate=rate)
                _, recovered_rate = _wav_to_pcm(wav_bytes)
                self.assertEqual(recovered_rate, rate)

    def test_empty_pcm_produces_a_valid_empty_wav(self):
        wav_bytes = _pcm_to_wav(b"", sample_rate=16000)
        pcm, rate = _wav_to_pcm(wav_bytes)

        self.assertEqual(pcm, b"")
        self.assertEqual(rate, 16000)


if __name__ == "__main__":
    unittest.main()
