"""Expertise and Skill are one thing under two names, and so is the manifest file.

    cd sdk/python && PYTHONPATH=src python -m unittest test_naming -v
"""
import os
import tempfile
import unittest
from pathlib import Path

import aura
from aura import Expertise, Skill, default_manifest_path, load_manifest

MANIFEST = """
id: "acme/logical/echo"
version: "1.0.0"
protocol: "1"
name: "echo"
description: "test"
capability: "logical.echo"
type: logical
format: source
ports:
  ingress:
    - name: text_in
      schema: "std/text@1"
  egress:
    - name: text_out
      schema: "std/text@1"
"""


class OneClassTwoNames(unittest.TestCase):
    def test_expertise_is_skill(self):
        self.assertIs(Expertise, Skill)
        self.assertIs(aura.Expertise, aura.Skill)
        self.assertIn("Expertise", aura.__all__)
        self.assertIn("Skill", aura.__all__)


class ManifestFile(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.prev = os.getcwd()
        os.chdir(self.tmp.name)

    def tearDown(self):
        os.chdir(self.prev)
        self.tmp.cleanup()

    def test_a_package_written_before_the_rename_loads_unchanged(self):
        Path("skill.yaml").write_text(MANIFEST, encoding="utf-8")
        self.assertEqual(default_manifest_path(), "skill.yaml")
        self.assertEqual(Skill(token="").manifest["id"], "acme/logical/echo")
        self.assertEqual(Expertise(token="").manifest["id"], "acme/logical/echo")
        # An explicit path, as existing callers pass it, still works.
        self.assertEqual(Skill("skill.yaml", token="").manifest["id"], "acme/logical/echo")
        self.assertEqual(load_manifest("skill.yaml")["capability"], "logical.echo")

    def test_the_current_name_is_read(self):
        Path("expertise.yaml").write_text(MANIFEST, encoding="utf-8")
        self.assertEqual(default_manifest_path(), "expertise.yaml")
        self.assertEqual(Expertise(token="").manifest["id"], "acme/logical/echo")

    def test_the_current_name_wins_when_both_exist(self):
        Path("expertise.yaml").write_text(MANIFEST, encoding="utf-8")
        Path("skill.yaml").write_text(MANIFEST.replace("acme/logical/echo", "acme/logical/old"),
                                      encoding="utf-8")
        self.assertEqual(Skill(token="").manifest["id"], "acme/logical/echo")

    def test_a_missing_manifest_names_the_file_to_create(self):
        with self.assertRaises(FileNotFoundError) as caught:
            Skill(token="")
        self.assertIn("expertise.yaml", str(caught.exception))
        self.assertIn("skill.yaml", str(caught.exception))

    def test_another_directory(self):
        sub = Path("pkg")
        sub.mkdir()
        (sub / "skill.yaml").write_text(MANIFEST, encoding="utf-8")
        self.assertEqual(Path(default_manifest_path(sub)), sub / "skill.yaml")


if __name__ == "__main__":
    unittest.main()
