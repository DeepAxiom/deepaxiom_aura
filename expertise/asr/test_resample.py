"""Tests for the resampler (resample.py).

The claim under test is the one linear interpolation could not make: a tone
above the new Nyquist frequency is REMOVED, not folded down into the speech
band. numpy is required to run them and is not installed in CI, so they skip
there rather than fail — which also means CI does not exercise the resampler;
run them locally after touching it.

    cd expertise/asr && PYTHONPATH=../../sdk/python/src python -m unittest -v
"""
import subprocess
import sys
import unittest

try:
    import numpy as np
except ImportError:  # CI installs only pyyaml + websockets
    np = None

import engine
import resample as resample_module
from resample import resample

needs_numpy = unittest.skipIf(np is None, "numpy is not installed")


def tone(freq, rate, seconds=1.0, amplitude=0.5):
    t = np.arange(int(rate * seconds)) / rate
    return (amplitude * np.sin(2 * np.pi * freq * t)).astype(np.float32)


def rms(x):
    x = np.asarray(x, dtype=np.float64)
    return float(np.sqrt(np.mean(x * x)))


def db(ratio):
    return 20 * np.log10(max(ratio, 1e-12))


def middle(x, rate, margin=0.1):
    """Drop the filter's start-up and tail at the edges."""
    cut = int(rate * margin)
    return x[cut:-cut]


def linear_interp(audio, src, dst):
    """What the skill did before: the reference for 'this is the bug'."""
    idx = np.linspace(0, len(audio) - 1, int(len(audio) * dst / src))
    return np.interp(idx, np.arange(len(audio)), audio).astype(np.float32)


class LazyImports(unittest.TestCase):
    def test_importing_the_skill_modules_pulls_in_no_heavy_dependency(self):
        """CI has neither numpy nor faster-whisper. This runs in a fresh
        interpreter so it holds even when other tests already imported them."""
        code = (
            "import sys; import engine, resample, streaming, scheduler; "
            "bad = [m for m in ('numpy', 'faster_whisper', 'ctranslate2') "
            "if m in sys.modules]; "
            "sys.exit('imported at module level: %s' % bad if bad else 0)")
        done = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True)
        self.assertEqual(done.returncode, 0, done.stderr)


@needs_numpy
class AntiAliasing(unittest.TestCase):
    def test_a_tone_above_the_new_nyquist_is_attenuated(self):
        """12 kHz at 48 kHz, brought to 16 kHz. Nyquist is now 8 kHz; decimate
        without filtering and this tone reappears at 4 kHz, full strength."""
        x = tone(12000, 48000)
        y = middle(resample(x, 48000, 16000), 16000)
        self.assertLess(db(rms(y) / rms(x)), -60)

    def test_the_old_linear_interpolation_let_it_through(self):
        """Not a test of new code: it pins down what was wrong, so the test
        above cannot be passing for an uninteresting reason."""
        x = tone(12000, 48000)
        aliased = middle(linear_interp(x, 48000, 16000), 16000)
        self.assertGreater(db(rms(aliased) / rms(x)), -6)

    def test_out_of_band_tones_are_attenuated_across_the_stopband(self):
        for src, freq in ((48000, 9000), (48000, 15000), (48000, 23000),
                          (44100, 10000), (44100, 20000), (32000, 9500),
                          (22050, 10500), (24000, 11000)):
            with self.subTest(src=src, freq=freq):
                x = tone(freq, src)
                y = middle(resample(x, src, 16000), 16000)
                self.assertLess(db(rms(y) / rms(x)), -50)

    def test_noise_above_nyquist_does_not_reach_the_speech_band(self):
        """The realistic case: speech-band tone plus loud hiss above 8 kHz."""
        rng = np.random.default_rng(7)
        hiss = rng.standard_normal(48000).astype(np.float32)
        spectrum = np.fft.rfft(hiss)
        freqs = np.fft.rfftfreq(hiss.size, 1 / 48000)
        spectrum[freqs < 9000] = 0
        hiss = np.fft.irfft(spectrum, hiss.size).astype(np.float32)
        hiss *= 0.5 / rms(hiss)

        y = middle(resample(hiss, 48000, 16000), 16000)
        self.assertLess(db(rms(y) / rms(hiss)), -50)


@needs_numpy
class PassBand(unittest.TestCase):
    def test_speech_band_tones_pass_at_unit_gain(self):
        for src in (48000, 44100, 32000, 24000, 22050, 8000, 11025):
            top = min(src, 16000) / 2
            for freq in (200, 1000, 3000, 0.8 * top):
                with self.subTest(src=src, freq=freq):
                    x = tone(freq, src)
                    y = middle(resample(x, src, 16000), 16000)
                    self.assertAlmostEqual(db(rms(y) / rms(middle(x, src))), 0.0, delta=0.5)

    def test_a_tone_keeps_its_frequency(self):
        for src in (48000, 44100, 8000):
            with self.subTest(src=src):
                y = resample(tone(1000, src, seconds=2.0), src, 16000)
                spectrum = np.abs(np.fft.rfft(y * np.hanning(y.size)))
                peak = np.fft.rfftfreq(y.size, 1 / 16000)[int(np.argmax(spectrum))]
                self.assertAlmostEqual(peak, 1000, delta=2)

    def test_there_is_no_delay(self):
        """Output sample n is input time n*src/dst: a click stays where it
        was. Word timestamps are cut points for the audio buffer, so a
        resampler that shifted the signal would shift every cut."""
        x = np.zeros(48000, dtype=np.float32)
        x[24000] = 1.0
        y = resample(x, 48000, 16000)
        self.assertEqual(int(np.argmax(np.abs(y))), 8000)

    def test_dc_is_preserved(self):
        y = resample(np.full(44100, 0.25, dtype=np.float32), 44100, 16000)
        self.assertAlmostEqual(float(np.mean(middle(y, 16000))), 0.25, places=4)

    def test_it_matches_the_waveform_not_just_the_level(self):
        """Resampling a band-limited signal should land on the same signal
        sampled at the new rate."""
        seconds = 1.0
        y = resample(tone(440, 44100, seconds), 44100, 16000)
        want = tone(440, 16000, seconds)
        n = min(y.size, want.size)
        err = middle(y[:n] - want[:n], 16000)
        self.assertLess(db(rms(err) / rms(want)), -50)


@needs_numpy
class Shape(unittest.TestCase):
    def test_output_length_follows_the_rate_ratio(self):
        for src, n in ((48000, 48000), (44100, 44100), (8000, 8000),
                       (44100, 12345), (48000, 1), (22050, 7)):
            with self.subTest(src=src, n=n):
                y = resample(np.zeros(n, dtype=np.float32), src, 16000)
                self.assertEqual(y.size, -(-n * 16000 // src))
                self.assertEqual(y.dtype, np.float32)

    def test_same_rate_and_empty_input_are_passed_through(self):
        x = tone(440, 16000, 0.1)
        self.assertIs(resample(x, 16000, 16000), x)
        self.assertEqual(resample(np.zeros(0, dtype=np.float32), 48000, 16000).size, 0)

    def test_a_buffer_longer_than_one_block_has_no_seams(self):
        """The loop works in blocks of output samples; a discontinuity at a
        block edge would be a click every half second."""
        seconds = 3.0
        self.assertGreater(seconds * 16000, 2 * resample_module._BLOCK)
        y = resample(tone(300, 48000, seconds), 48000, 16000)
        want = tone(300, 16000, seconds)
        err = middle(y - want[:y.size], 16000)
        self.assertLess(float(np.max(np.abs(err))), 1e-3)

    def test_bad_rates_are_refused(self):
        with self.assertRaises(ValueError):
            resample(np.zeros(10, dtype=np.float32), 0, 16000)

    def test_the_filter_is_designed_once_per_ratio(self):
        resample(tone(440, 48000, 0.1), 48000, 16000)
        first = resample_module._filters[(1, 3)]
        resample(tone(440, 48000, 0.1), 48000, 16000)
        self.assertIs(resample_module._filters[(1, 3)], first)


@needs_numpy
class EngineUsesIt(unittest.TestCase):
    def test_pcm_at_16k_is_only_converted_to_float(self):
        pcm = np.array([0, 16384, -32768, 32767], dtype="<i2").tobytes()
        audio = engine._audio_16k(pcm, 16000)
        np.testing.assert_allclose(audio, [0, 0.5, -1.0, 32767 / 32768], rtol=1e-6)
        self.assertEqual(audio.dtype, np.float32)

    def test_pcm_at_another_rate_goes_through_the_filtered_resampler(self):
        loud = (tone(12000, 48000) * 32767).astype("<i2").tobytes()
        audio = engine._audio_16k(loud, 48000)
        self.assertEqual(audio.size, 16000)
        self.assertLess(db(rms(middle(audio, 16000)) / 0.35), -60)

    def test_a_trailing_odd_byte_is_ignored(self):
        self.assertEqual(engine._audio_16k(b"\x00\x01\x02", 16000).size, 1)


if __name__ == "__main__":
    unittest.main()
