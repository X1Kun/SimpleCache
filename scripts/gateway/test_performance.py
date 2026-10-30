"""Regression checks: measured read errors must survive; bad writes still stop."""
import json
import pathlib
import tempfile
import types
import unittest

from performance import driver, summarize_outcome


class ReadErrorEvidenceTest(unittest.TestCase):
    def fixture(self, path, *, write_status=200, incorrect=0, skipped=0):
        destination = path / 'mixed'
        destination.mkdir()
        summary = {'planned': 2 + skipped, 'attempted': 2, 'skipped': skipped,
                   'errors': 1, 'incorrect': incorrect, 'http_503': 1}
        (destination/'summary.json').write_text(json.dumps(summary))
        records = [dict(method='PUT', status=write_status, incorrect=False),
                   dict(method='GET', status=503, incorrect=bool(incorrect), error='non-200 response')]
        (destination/'requests.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in records))

        def kubectl(*args, **kwargs):
            if args[0] == 'exec':
                raise RuntimeError('driver reports measured read error')
        return types.SimpleNamespace(REPORT=path, k=kubectl), summary

    def test_read_error_remains_in_result_and_raw_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)
            runner, expected = self.fixture(path)
            actual = driver(runner, {'windows': 1}, 'mixed', collect_read_errors=True)
            self.assertEqual(actual, expected)
            self.assertEqual(actual['errors'], 1)
            self.assertIn('503', (path/'mixed/requests.jsonl').read_text())
            with self.assertRaises(AssertionError):
                driver(runner, {'windows': 1}, 'mixed')

    def test_failed_write_incorrect_value_or_missing_request_still_stops(self):
        for case in [dict(write_status=503), dict(incorrect=1), dict(skipped=1)]:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                runner, _ = self.fixture(pathlib.Path(directory), **case)
                with self.assertRaises(AssertionError):
                    driver(runner, {'windows': 1}, 'mixed', collect_read_errors=True)


class ContinuationOutcomeTest(unittest.TestCase):
    def test_retained_failures_survive_a_successful_rerun(self):
        cases = [
            ('incorrect', {'incorrect': 1}, [], 'failed'),
            ('skipped', {'skipped': 1, 'planned': 3}, [], 'failed'),
            ('write-error', {'errors': 1}, [{'method': 'PUT', 'error': 'non-200 response'}], 'failed'),
            ('read-error', {'errors': 1}, [{'method': 'GET', 'error': 'non-200 response'}], 'completed-with-read-errors'),
            ('mixed-errors', {'errors': 2}, [{'method': 'GET', 'error': 'timeout'}, {'method': 'PUT', 'error': 'timeout'}], 'failed'),
            ('empty', {'attempted': 0, 'planned': 0}, [], 'failed'),
            ('clean', {}, [], 'passed'),
        ]
        for name, changes, records, expected in cases:
            for retained in [True, False]:
                with self.subTest(name=name, retained=retained), tempfile.TemporaryDirectory() as directory:
                    path = pathlib.Path(directory)
                    (path/'attempt').mkdir()
                    (path/'attempt/requests.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in records))
                    clean = dict(attempted=2, planned=2, errors=0, incorrect=0, skipped=0)
                    summary = {**clean, **changes}
                    trial = dict(name='attempt', run_report=str(path))
                    manifest = {'trials': [{**trial, 'client': clean}], 'prior_failed_attempts': []}
                    if retained:
                        manifest['prior_failed_attempts'].append({**trial, 'summary': summary})
                    else:
                        manifest['trials'] = [{**trial, 'client': summary}]
                    outcome = summarize_outcome(manifest)
                    self.assertEqual(outcome['status'], expected)
                    self.assertEqual(outcome['counts']['write_errors'], sum(r['method'] == 'PUT' for r in records))
                    self.assertEqual(outcome['counts']['read_errors'], sum(r['method'] == 'GET' for r in records))
                    self.assertEqual(outcome['counts']['incorrect'], summary['incorrect'])

    def test_stale_or_mismatched_evidence_cannot_pass(self):
        for change, records in [
            ({'stale_at_start': 1}, []),
            ({'errors': 1}, []),
            ({'errors': 1}, [{'method': 'UNKNOWN', 'error': 'failure'}]),
            ({'planned': 3}, []),
        ]:
            with self.subTest(change=change, records=records), tempfile.TemporaryDirectory() as directory:
                path = pathlib.Path(directory)
                (path/'trial').mkdir()
                (path/'trial/requests.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in records))
                summary = dict(attempted=2, planned=2, errors=0, incorrect=0, skipped=0)
                manifest = {'trials': [dict(name='trial', run_report=str(path), client={**summary, **change})]}
                self.assertEqual(summarize_outcome(manifest)['status'], 'failed')
        self.assertEqual(summarize_outcome({'trials': []})['status'], 'failed')

    def test_missing_error_evidence_is_not_called_a_read_error(self):
        manifest = {'trials': [], 'prior_failed_attempts': [dict(name='missing', run_report='/missing',
                    summary=dict(attempted=1, planned=1, skipped=0, incorrect=0, errors=1))]}
        self.assertEqual(summarize_outcome(manifest)['status'], 'failed')


if __name__ == '__main__':
    unittest.main()
