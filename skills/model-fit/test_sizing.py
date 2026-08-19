"""Sizing arithmetic, and the one invariant that spans two skills.

`model-fit` recommends by `model_id`; `model-manager` downloads by `model_id`.
They are separate processes with separate tables, so nothing stops one from
naming a model the other has never heard of — a recommendation for something
nobody can fetch. The first test closes that by reading the other skill's
catalogue directly.
"""
import importlib.util
import unittest
from pathlib import Path

import hardware
import sizing


def _model_manager_catalog() -> list[dict]:
    """Load the sibling skill's catalogue without importing it as a package."""
    path = Path(__file__).resolve().parent.parent / "model-manager" / "catalog.py"
    spec = importlib.util.spec_from_file_location("mm_catalog", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.CATALOG


class CatalogAgreement(unittest.TestCase):
    def test_every_sized_model_is_downloadable(self):
        known = {e["model_id"] for e in _model_manager_catalog()}
        unknown = set(sizing.MODELS) - known
        self.assertEqual(
            unknown, set(),
            f"model-fit sizes models model-manager cannot fetch: {sorted(unknown)}. "
            "Add them to skills/model-manager/catalog.py or drop them here.",
        )


class Sizing(unittest.TestCase):
    def test_weights_scale_with_quantization(self):
        q4 = sizing.weights_bytes(7.0, "q4_k_m")
        q8 = sizing.weights_bytes(7.0, "q8_0")
        self.assertLess(q4, q8)
        # 7B at ~4.5 bits ≈ 3.9 GB. A wide band, because the point is to catch
        # an order-of-magnitude slip, not to pin a constant nobody can verify.
        self.assertGreater(q4, 3.0e9)
        self.assertLess(q4, 4.5e9)

    def test_unknown_quantization_falls_back_rather_than_raising(self):
        self.assertEqual(
            sizing.weights_bytes(1.0, "something-new"),
            sizing.weights_bytes(1.0, "q4_k_m"),
        )

    def test_longer_context_needs_more_memory(self):
        short = sizing.required_bytes(7.0, "q4_k_m", 2048)
        long = sizing.required_bytes(7.0, "q4_k_m", 32768)
        self.assertGreater(long, short)

    def test_a_gpu_that_fits_is_placed_on_the_gpu(self):
        hw = {"ram_bytes": 32 * sizing.GIB, "vram_bytes": 24 * sizing.GIB,
              "gpus": [{"vendor": "nvidia", "vram_bytes": 24 * sizing.GIB}]}
        s = sizing.score("deepseek-r1-qwen-7b", hw)
        self.assertEqual(s["verdict"], "fits")
        self.assertEqual(s["placement"], "gpu")

    def test_no_gpu_but_enough_ram_still_runs(self):
        """The verdict a laptop depends on: slow is not the same as impossible."""
        hw = {"ram_bytes": 32 * sizing.GIB, "vram_bytes": 0, "gpus": []}
        s = sizing.score("deepseek-r1-qwen-7b", hw)
        self.assertEqual(s["verdict"], "fits")
        self.assertEqual(s["placement"], "cpu")

    def test_a_gpu_too_small_reports_cpu_fallback_not_failure(self):
        hw = {"ram_bytes": 32 * sizing.GIB, "vram_bytes": 2 * sizing.GIB,
              "gpus": [{"vendor": "nvidia", "vram_bytes": 2 * sizing.GIB}]}
        s = sizing.score("deepseek-r1-qwen-7b", hw)
        self.assertEqual(s["verdict"], "fits-on-cpu")

    def test_a_machine_too_small_says_so(self):
        hw = {"ram_bytes": 2 * sizing.GIB, "vram_bytes": 0, "gpus": []}
        s = sizing.score("deepseek-r1-qwen-7b", hw)
        self.assertEqual(s["verdict"], "too-big")

    def test_every_score_ships_its_assumptions(self):
        hw = {"ram_bytes": 16 * sizing.GIB, "vram_bytes": 0, "gpus": []}
        s = sizing.score("gemma-3-1b-it", hw)
        for key in ("bits_per_weight", "context_tokens", "bandwidth_gbs"):
            self.assertIn(key, s["assumptions"])

    def test_unknown_model_is_none_not_a_guess(self):
        hw = {"ram_bytes": 16 * sizing.GIB, "vram_bytes": 0, "gpus": []}
        self.assertIsNone(sizing.score("no-such-model", hw))

    def test_rank_puts_what_runs_first(self):
        hw = {"ram_bytes": 8 * sizing.GIB, "vram_bytes": 0, "gpus": []}
        ranked = sizing.rank(hw)
        verdicts = [r["verdict"] for r in ranked]
        self.assertEqual(verdicts, sorted(verdicts, key=lambda v: {"fits": 0, "fits-on-cpu": 1, "too-big": 2}[v]))
        self.assertEqual(len(ranked), len(sizing.MODELS), "a model that does not fit is still listed")


class HardwareProbe(unittest.TestCase):
    def test_detect_answers_on_this_machine(self):
        """Whatever this runs on, it must return a shape — never raise."""
        hw = hardware.detect()
        for key in ("os", "arch", "cpu", "cpu_cores", "ram_bytes", "gpus", "vram_bytes"):
            self.assertIn(key, hw)
        self.assertIsInstance(hw["gpus"], list)

    def test_ranking_survives_a_machine_that_says_nothing(self):
        blind = {"ram_bytes": None, "vram_bytes": 0, "gpus": []}
        ranked = sizing.rank(blind)
        self.assertTrue(all(r["verdict"] == "too-big" for r in ranked),
                        "unknown memory must be scored conservatively, not optimistically")


if __name__ == "__main__":
    unittest.main()
