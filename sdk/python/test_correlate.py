"""Que la respuesta se pueda emparejar con quien pregunto.

Un grafo es una tuberia y no una llamada: el sobre que emite una skill lo causa
la entrega que recibio, no el mensaje de cliente que empezo la cadena. Quien
espera una respuesta no puede emparejarla por causa, y lo unico que sobrevive
todos los saltos es la carga.
"""
import unittest

from aura.skill import _carry_correlation


class Correlation(unittest.TestCase):
    def test_the_token_comes_back(self):
        out = _carry_correlation({"correlate": "abc", "frames": []}, {"findings": []})
        self.assertEqual(out["correlate"], "abc")
        self.assertEqual(out["findings"], [])

    def test_a_delivery_without_one_changes_nothing(self):
        answer = {"findings": []}
        self.assertEqual(_carry_correlation({"frames": []}, answer), answer)

    def test_a_skill_that_means_something_else_by_it_keeps_it(self):
        out = _carry_correlation({"correlate": "del-cliente"}, {"correlate": "de-la-skill"})
        self.assertEqual(out["correlate"], "de-la-skill")

    def test_payloads_that_are_not_objects_pass_through(self):
        self.assertEqual(_carry_correlation("texto", {"a": 1}), {"a": 1})
        self.assertEqual(_carry_correlation({"correlate": "abc"}, "texto"), "texto")
        self.assertIsNone(_carry_correlation({"correlate": "abc"}, None))

    def test_a_token_that_is_not_a_string_is_ignored(self):
        answer = {"findings": []}
        self.assertEqual(_carry_correlation({"correlate": 7}, answer), answer)
        self.assertEqual(_carry_correlation({"correlate": ""}, answer), answer)


if __name__ == "__main__":
    unittest.main()
