"""Credential resolution for the Python SDK.

A node binds loopback and mints a bearer token by default, and `/ws/skill` sits
behind it like every other route — so this is what stands between
`python main.py` and a skill that retries forever against its own node. It
shipped unimplemented once; these pin the behaviour down.

    cd sdk/python && PYTHONPATH=src python -m unittest test_token -v
"""
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent / "src"))

from aura.skill import _default_data_dir, _header_kwarg, _status_of, resolve_token  # noqa: E402


class ResolveToken(unittest.TestCase):
    def test_env_wins(self):
        with mock.patch.dict(os.environ, {"AURA_TOKEN": "from-env"}):
            self.assertEqual(resolve_token(), "from-env")

    def test_env_is_trimmed(self):
        with mock.patch.dict(os.environ, {"AURA_TOKEN": "  padded  "}):
            self.assertEqual(resolve_token(), "padded")

    def test_falls_back_to_the_token_file(self):
        with mock.patch.dict(os.environ, {"AURA_TOKEN": ""}):
            with mock.patch("aura.skill._default_data_dir") as data_dir:
                token_file = mock.MagicMock()
                token_file.read_text.return_value = "from-file\n"
                data_dir.return_value.__truediv__.return_value = token_file
                self.assertEqual(resolve_token(), "from-file")

    def test_missing_file_is_not_an_error(self):
        """A --no-auth node has no token file and asks for no credential."""
        with mock.patch.dict(os.environ, {"AURA_TOKEN": ""}):
            with mock.patch("aura.skill._default_data_dir") as data_dir:
                token_file = mock.MagicMock()
                token_file.read_text.side_effect = FileNotFoundError
                data_dir.return_value.__truediv__.return_value = token_file
                self.assertEqual(resolve_token(), "")

    def test_unreadable_file_is_not_an_error(self):
        """Another user's node directory: nothing to send, not a crash."""
        with mock.patch.dict(os.environ, {"AURA_TOKEN": ""}):
            with mock.patch("aura.skill._default_data_dir") as data_dir:
                token_file = mock.MagicMock()
                token_file.read_text.side_effect = PermissionError
                data_dir.return_value.__truediv__.return_value = token_file
                self.assertEqual(resolve_token(), "")

    def test_default_data_dir_matches_the_kernel(self):
        self.assertEqual(_default_data_dir().name, ".aura")


class HandshakeHelpers(unittest.TestCase):
    def test_header_kwarg_is_one_websockets_accepts(self):
        """websockets 14 renamed extra_headers; the SDK declares >=12."""
        self.assertIn(_header_kwarg(), ("additional_headers", "extra_headers"))

    def test_status_of_reads_the_modern_shape(self):
        exc = mock.MagicMock(spec=["response"])
        exc.response = mock.MagicMock(spec=["status_code"])
        exc.response.status_code = 401
        self.assertEqual(_status_of(exc), 401)

    def test_status_of_reads_the_legacy_shape(self):
        exc = mock.MagicMock(spec=["status_code"])
        exc.status_code = 401
        self.assertEqual(_status_of(exc), 401)

    def test_status_of_is_none_for_an_ordinary_failure(self):
        self.assertIsNone(_status_of(ConnectionRefusedError("no node")))


class SkillWiring(unittest.TestCase):
    """The constructor's token parameter, without opening a socket."""

    MANIFEST = {
        "id": "example/logical/t",
        "version": "1.0.0",
        "protocol": "1",
        "name": "t",
        "description": "d",
        "capability": "logical.t",
        "type": "logical",
        "format": "source",
        "ports": {
            "ingress": [{"name": "text_in", "schema": "std/text@1"}],
            "egress": [{"name": "text_out", "schema": "std/text@1"}],
        },
    }

    def test_explicit_token_wins_over_the_environment(self):
        from aura import Skill

        with mock.patch.dict(os.environ, {"AURA_TOKEN": "from-env"}):
            skill = Skill(manifest=dict(self.MANIFEST), token="explicit")
            self.assertEqual(skill._token, "explicit")

    def test_explicit_empty_token_means_send_nothing(self):
        from aura import Skill

        with mock.patch.dict(os.environ, {"AURA_TOKEN": "from-env"}):
            skill = Skill(manifest=dict(self.MANIFEST), token="")
            self.assertEqual(skill._token, "", "an empty token is a choice, not an absence")

    def test_default_reads_the_environment(self):
        from aura import Skill

        with mock.patch.dict(os.environ, {"AURA_TOKEN": "from-env"}):
            skill = Skill(manifest=dict(self.MANIFEST))
            self.assertEqual(skill._token, "from-env")


if __name__ == "__main__":
    unittest.main()
