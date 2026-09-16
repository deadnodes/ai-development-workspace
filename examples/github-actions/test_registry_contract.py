import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('contract', Path(__file__).with_name('registry_contract.py'))
contract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(contract)

class RegistryContractTest(unittest.TestCase):
    def environment(self, output):
        return dict(IMAGE_REPOSITORY='ghcr.io/org/component', IMAGE_TAG='rcp-' + 'a'*40,
                    SOURCE_SHA='a'*40, OPERATION_ID='operation-1', GITHUB_ACTOR='robot',
                    GHCR_TOKEN='test-only-token', GITHUB_OUTPUT=output)

    def test_manifest_absence_is_explicit_and_does_not_create_report(self):
        with tempfile.TemporaryDirectory() as directory:
            output = str(Path(directory)/'output')
            with patch.dict(os.environ, self.environment(output), clear=True), patch.object(contract.sys, 'argv', ['contract']), patch.object(contract, 'request', side_effect=[(200, {}, b'{"token":"ephemeral"}'), (404, {}, b'')]):
                contract.main()
            self.assertEqual(Path(output).read_text(), 'exists=false\ndigest=\n')

    def test_access_failure_does_not_become_absence(self):
        with tempfile.TemporaryDirectory() as directory:
            output = str(Path(directory)/'output')
            with patch.dict(os.environ, self.environment(output), clear=True), patch.object(contract, 'request', side_effect=RuntimeError('Registry request failed: HTTP 403')):
                with self.assertRaisesRegex(RuntimeError, '403'):
                    contract.main()
            self.assertFalse(Path(output).exists())

    def test_digest_must_match_bytes_and_requested_identity(self):
        body = b'{"schemaVersion":2}'
        digest = 'sha256:' + hashlib.sha256(body).hexdigest()
        with tempfile.TemporaryDirectory() as directory:
            output = str(Path(directory)/'output')
            with patch.dict(os.environ, self.environment(output), clear=True), patch.object(contract.sys, 'argv', ['contract']), patch.object(contract, 'request', side_effect=[(200, {}, b'{"token":"ephemeral"}'), (200, {'Docker-Content-Digest':digest}, body)]):
                contract.main()
            self.assertIn(digest, Path(output).read_text())
            with patch.dict(os.environ, {**self.environment(output), 'EXPECTED_DIGEST':'sha256:'+'0'*64}, clear=True), patch.object(contract, 'request', side_effect=[(200, {}, b'{"token":"ephemeral"}'), (200, {'Docker-Content-Digest':digest}, body)]):
                with self.assertRaisesRegex(RuntimeError, 'different digest'):
                    contract.main()

    def test_invalid_inputs_never_contact_registry(self):
        for key,value in [('SOURCE_SHA','main'), ('IMAGE_TAG','$(command)'), ('IMAGE_REPOSITORY','https://other.example/repo'), ('OPERATION_ID','../../secret')]:
            with patch.dict(os.environ, {**self.environment('/unused'),key:value}, clear=True), patch.object(contract, 'request') as request:
                with self.assertRaises(RuntimeError):
                    contract.main()
                request.assert_not_called()

if __name__ == '__main__':
    unittest.main()
