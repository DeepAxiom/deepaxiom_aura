"""What a reader answers, and what it must not be trusted about.

Written as the list of ways a rectangle ends up somewhere it should not,
because that is the one failure a clinician cannot catch by reading the text: a
wrong sentence is visibly wrong, and a box over the wrong lobe looks exactly
like a box over the right one.
"""
import json
import unittest

import doors
import findings
import vertex

KNOWN = {"1.2.3", "1.2.4"}


def one(**over):
    finding = {"label": "nódulo", "instance_uid": "1.2.3", "box_2d": [100, 200, 300, 400]}
    finding.update(over)
    return finding


def read(payload, limit=12):
    return findings.read(json.dumps(payload), KNOWN, limit, "es")


class Coordinates(unittest.TestCase):
    def test_a_box_lands_where_the_model_pointed(self):
        out = read({"findings": [one()], "limitations": "muestra parcial"})
        # [ymin, xmin, ymax, xmax] sobre 0..1000 -> origen y tamaño en 0..1.
        self.assertEqual(out["findings"][0]["box"],
                         {"x": 0.2, "y": 0.1, "w": 0.2, "h": 0.2})

    def test_a_swapped_pair_is_ordered_and_not_drawn_inside_out(self):
        out = read({"findings": [one(box_2d=[300, 400, 100, 200])], "limitations": "x"})
        self.assertEqual(out["findings"][0]["box"],
                         {"x": 0.2, "y": 0.1, "w": 0.2, "h": 0.2})

    def test_a_box_outside_the_image_is_pulled_back_in(self):
        out = read({"findings": [one(box_2d=[-50, -50, 1200, 1200])], "limitations": "x"})
        self.assertEqual(out["findings"][0]["box"],
                         {"x": 0.0, "y": 0.0, "w": 1.0, "h": 1.0})

    def test_a_finding_with_no_usable_rectangle_is_dropped(self):
        for bad in ([], [1, 2, 3], "cerca del pezón", None, [0, 0, 0, 0]):
            with self.subTest(box=bad):
                out = read({"findings": [one(box_2d=bad)], "limitations": "x"})
                self.assertEqual(out["findings"], [])


class WhatItSaysItSawItOn(unittest.TestCase):
    def test_a_finding_on_an_image_nobody_sent_is_dropped(self):
        out = read({"findings": [one(instance_uid="9.9.9")], "limitations": "x"})
        self.assertEqual(out["findings"], [])

    def test_a_finding_with_no_label_is_dropped(self):
        out = read({"findings": [one(label="  ")], "limitations": "x"})
        self.assertEqual(out["findings"], [])


class WhenTheAnswerIsBad(unittest.TestCase):
    def test_an_answer_that_is_not_json_is_a_read_with_no_findings(self):
        out = findings.read("lo siento, no puedo", KNOWN, 12, "es")
        self.assertEqual(out["findings"], [])
        self.assertTrue(out["limitations"].strip())

    def test_limitations_are_never_empty(self):
        self.assertTrue(read({"findings": [], "limitations": ""})["limitations"].strip())

    def test_the_ceiling_holds(self):
        many = {"findings": [one() for _ in range(40)], "limitations": "x"}
        self.assertEqual(len(read(many, limit=5)["findings"]), 5)

    def test_confidence_is_bounded_or_absent(self):
        out = read({"findings": [one(confidence=1.7)], "limitations": "x"})
        self.assertEqual(out["findings"][0]["confidence"], 1.0)
        out = read({"findings": [one(confidence="mucha")], "limitations": "x"})
        self.assertNotIn("confidence", out["findings"][0])


class TheTwoDoors(unittest.TestCase):
    """Cual puerta contesta lo elige quien despliega; que quede escrito, no."""

    def test_the_vertex_client_refuses_to_be_aimed_at_the_other_door(self):
        # No es una regla de politica --esa se decide poniendo una llave u otra--
        # sino de correccion: otra forma de peticion y otra autenticacion.
        with self.assertRaises(vertex.NoReader) as refusal:
            vertex.ask("https://generativelanguage.googleapis.com/v1/x:generateContent",
                       "token", {})
        self.assertIn("generativelanguage", str(refusal.exception))

    def test_the_gemini_door_without_a_key_says_what_is_missing(self):
        with self.assertRaises(doors.NoReader) as refusal:
            doors.ask(doors.GEMINI, "models/gemini-3-flash-preview", "", "s", [], {}, 0.0, "high")
        self.assertIn("llave", str(refusal.exception))

    def test_an_unknown_door_is_not_guessed(self):
        with self.assertRaises(doors.NoReader):
            doors.ask("la-de-al-lado", "m", "k", "s", [], {}, 0.0, "high")

    def test_a_node_without_a_project_says_so_instead_of_guessing(self):
        with self.assertRaises(vertex.NoReader) as refusal:
            vertex.endpoint("", "us-central1", "gemini-3.7-flash")
        self.assertIn("proyecto", str(refusal.exception))

    def test_the_address_is_regional(self):
        url = vertex.endpoint("un-proyecto", "europe-west4", "gemini-3.7-flash")
        self.assertTrue(url.startswith("https://europe-west4-aiplatform.googleapis.com/"))
        self.assertIn("/projects/un-proyecto/locations/europe-west4/", url)

    def test_an_answer_with_no_candidate_is_empty_and_not_a_crash(self):
        self.assertEqual(vertex.text_of({}), "")
        self.assertEqual(vertex.text_of({"candidates": [{"content": {"parts": []}}]}), "")
        self.assertEqual(
            vertex.text_of({"candidates": [{"content": {"parts": [{"text": "hola"}]}}]}),
            "hola")




class WhereTheProjectComesFrom(unittest.TestCase):
    """C1 config primero, luego el entorno, luego lo declarado.

    El entorno esta ahi porque un contenedor no tiene plano de control en su
    primer arranque, y pedirle a alguien que haga un PUT antes de que un nodo
    pueda leer nada es pedirle que configure lo mismo dos veces.
    """

    def setUp(self):
        import main
        self.main = main
        self.saved = dict(main.skill.config)

    def tearDown(self):
        self.main.skill.config = self.saved

    def test_the_config_wins(self):
        import os
        from unittest import mock
        self.main.skill.config["project"] = "el-de-la-config"
        with mock.patch.dict(os.environ, {"GOOGLE_CLOUD_PROJECT": "el-del-entorno"}):
            self.assertEqual(
                self.main.setting("project", "GOOGLE_CLOUD_PROJECT", ""), "el-de-la-config")

    def test_the_environment_fills_in(self):
        import os
        from unittest import mock
        self.main.skill.config["project"] = ""
        with mock.patch.dict(os.environ, {"GOOGLE_CLOUD_PROJECT": "el-del-entorno"}):
            self.assertEqual(
                self.main.setting("project", "GOOGLE_CLOUD_PROJECT", ""), "el-del-entorno")

    def test_with_neither_there_is_no_reader(self):
        import os
        from unittest import mock
        self.main.skill.config["project"] = ""
        with mock.patch.dict(os.environ, {"GOOGLE_CLOUD_PROJECT": ""}):
            self.assertEqual(self.main.setting("project", "GOOGLE_CLOUD_PROJECT", ""), "")


class WhatCameBack(unittest.TestCase):
    """La respuesta viaja en tres formas segun la version del SDK.

    Un lector que asumiera una de ellas se rompe en una actualizacion con una
    traza en vez de una frase.
    """

    def test_output_text_wins(self):
        self.assertEqual(doors._text_of(_Answer(output_text="{}")), "{}")

    def test_a_step_with_string_content(self):
        answer = _Answer(steps=[_Step(content="hola")])
        self.assertEqual(doors._text_of(answer), "hola")

    def test_a_step_with_content_blocks(self):
        answer = _Answer(steps=[_Step(content=[_Block(text="  "), _Block(text="hola")])])
        self.assertEqual(doors._text_of(answer), "hola")

    def test_nothing_readable_is_empty_and_not_a_crash(self):
        self.assertEqual(doors._text_of(_Answer()), "")
        self.assertEqual(doors._text_of(_Answer(steps=[_Step(content=None)])), "")


class _Answer:
    def __init__(self, output_text=None, steps=None):
        self.output_text = output_text
        self.steps = steps or []


class _Step:
    def __init__(self, content=None):
        self.content = content


class _Block:
    def __init__(self, text=None):
        self.text = text


if __name__ == "__main__":
    unittest.main()
