"""Arrival outcome evidence must preserve limits and correctness failures."""
import tempfile
import unittest

from tools.validation.gateway.arrival import validate_evidence, schedule
from tools.validation.gateway.runner import GatewayRun


class ArrivalEvidenceTest(unittest.TestCase):
    def fixture(self, status='passed'):
        records = [dict(index=0, method='GET', attempted=True, incorrect=False),
                   dict(index=1, method='GET', attempted=True, incorrect=False)]
        summary = dict(planned=2, attempted=2, skipped=0, errors=0, incorrect=0,
                       success=2, arrival=dict(status=status))
        if status == 'client-limited':
            records[1]['attempted'] = False
            records[1]['drop_reason'] = 'scheduler_late'
            summary.update(attempted=1, skipped=1, success=1)
        elif status == 'completed-with-errors':
            records[1]['error'] = 'non-200 response'
            summary.update(errors=1, success=1)
        return summary, records

    def test_non_clean_outcomes_remain_non_clean(self):
        for status in ['passed', 'client-limited', 'completed-with-errors']:
            with self.subTest(status=status):
                summary, records = self.fixture(status)
                self.assertEqual(validate_evidence(summary, records), status)
                if status != 'passed':
                    summary['arrival']['status'] = 'passed'
                    with self.assertRaises(AssertionError):
                        validate_evidence(summary, records)

    def test_partial_corrupt_duplicate_or_write_records_fail(self):
        for change in ['missing', 'corrupt', 'duplicate', 'write']:
            with self.subTest(change=change):
                summary, records = self.fixture()
                if change == 'missing':
                    records.pop()
                elif change == 'corrupt':
                    records[0]['incorrect'] = True
                    summary.update(incorrect=1, success=1)
                elif change == 'duplicate':
                    records[1]['index'] = 0
                else:
                    records[0]['method'] = 'PUT'
                with self.assertRaises(AssertionError):
                    validate_evidence(summary, records)

    def test_arrival_lifecycle_and_schedule_bounds(self):
        with tempfile.TemporaryDirectory() as directory:
            context = GatewayRun(directory, environ={'GATEWAY_ARRIVAL': '1'})
            self.assertTrue(context.arrival)
            self.assertEqual(context.results['suite'], 'arrival')
            self.assertFalse(context.created)
            self.assertFalse(context.report.exists())
        stages = schedule()
        self.assertLessEqual(sum(s['seconds'] for s in stages), 120)
        self.assertLessEqual(sum(s['rate'] * s['seconds'] for s in stages), 100000)
        self.assertEqual(stages[0]['rate'], stages[-1]['rate'])
        self.assertGreater(max(s['rate'] for s in stages), stages[0]['rate'])

class ArrivalSamplingTest(unittest.TestCase):
    def test_missing_samples_and_changed_process_fail(self):
        import json
        import pathlib
        from tools.validation.gateway.arrival import summarize_samples
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'timeline.jsonl'
            rows = [dict(cache={f'products-{n}': {'process_start_time_seconds': 100} for n in range(3)}, errors=[]) for _ in range(3)]
            path.write_text('\n'.join(json.dumps(r) for r in rows))
            self.assertEqual(summarize_samples(path)['status'], 'complete')
            rows[1]['errors'] = ['kubelet: TimeoutExpired']
            path.write_text('\n'.join(json.dumps(r) for r in rows))
            self.assertEqual(summarize_samples(path)['status'], 'incomplete')
            rows[1]['cache']['products-0']['process_start_time_seconds'] = 101
            path.write_text('\n'.join(json.dumps(r) for r in rows))
            with self.assertRaises(AssertionError):
                summarize_samples(path)
            path.write_text(json.dumps(rows[0]))
            with self.assertRaises(AssertionError):
                summarize_samples(path)

class ArrivalResourceSummaryTest(unittest.TestCase):
    def test_resources_keep_processes_separate_and_detect_counter_reset(self):
        import json
        import pathlib
        from tools.validation.gateway.arrival import resource_summary
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'timeline.jsonl'
            path.write_text(json.dumps({'cache': {'products-0': {
                'process_resident_memory_bytes': 100, 'simplecache_source_pending': 2,
                'simplecache_source_inflight': 3}}, 'kubelet': {'pods': [
                    {'podRef': {'name': 'gateway-bench'}, 'memory': {'workingSetBytes': 50}},
                    {'podRef': {'name': 'postgres'}, 'memory': {'workingSetBytes': 70}}]}}))
            result = resource_summary(path, {'products-0': {'nr_throttled': 2}},
                                      {'products-0': {'nr_throttled': 5}})
            self.assertEqual(result['sampled_maxima']['cache_rss_bytes'], 100)
            self.assertEqual(result['sampled_maxima']['gateway-bench_working_set_bytes'], 50)
            self.assertEqual(result['container_counter_deltas']['products-0']['nr_throttled'], 3)
            with self.assertRaises(AssertionError):
                resource_summary(path, {'products-0': {'nr_throttled': 5}},
                                 {'products-0': {'nr_throttled': 2}})

class ArrivalSelectionTest(unittest.TestCase):
    def test_retest_selection_is_explicit_and_validated(self):
        from tools.validation.gateway.arrival import distributions
        self.assertEqual(distributions({}), ['hot', 'scan', 'hot-scan', 'hit-miss'])
        self.assertEqual(distributions({'GATEWAY_ARRIVAL_DISTRIBUTIONS':'hot,hot-scan'}), ['hot', 'hot-scan'])
        for value in ['', 'hot,hot', 'unknown', 'hot,']:
            with self.assertRaises(ValueError):
                distributions({'GATEWAY_ARRIVAL_DISTRIBUTIONS':value})
