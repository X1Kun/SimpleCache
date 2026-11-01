"""HTTP results must not mutate the command interface used by later steps."""
import io
import json
import tempfile
import types
import unittest
import urllib.error
from unittest.mock import patch

from tools.validation.gateway.runner import GatewayRun


class Response(io.BytesIO):
    status = 200
    headers = {'ETag': '"v1"', 'X-SimpleCache-Node': 'default/products-0'}


class TransportTest(unittest.TestCase):
    def test_http_success_and_error_preserve_kubernetes_command_method(self):
        with tempfile.TemporaryDirectory() as directory:
            context = GatewayRun(directory, environ={})
            commands = []
            def command(args, **kwargs):
                commands.append(args)
                kwargs['stdout'].write('{"items": []}')
                return types.SimpleNamespace(returncode=0)
            missing = urllib.error.HTTPError('http://proxy', 404, 'missing',
                        {'Content-Type': 'text/plain'}, io.BytesIO(b'missing'))
            replies = [Response(b'{"key": "product:42"}'), missing]
            with context, patch('urllib.request.urlopen', side_effect=replies), patch('subprocess.run', side_effect=command):
                for expected_status in [200, 404]:
                    status, headers, body = context.request('http://proxy', 'product:42')
                    self.assertEqual(status, expected_status)
                    self.assertTrue(callable(context.k))
                    self.assertEqual(context.obj('pods'), {'items': []})
                self.assertEqual(len(commands), 2)
                self.assertTrue(all(args[:3] == ['kubectl', '--context', 'kind-'+context.cluster] for args in commands))
                self.assertEqual(headers['content-type'], 'text/plain')
            records = [json.loads(line) for line in (context.report/'requests.jsonl').read_text().splitlines()]
            self.assertEqual([r['status'] for r in records], [200, 404])
            self.assertEqual(records[0]['etag'], '"v1"')
            self.assertTrue(all('Authorization' not in json.dumps(r) for r in records))
