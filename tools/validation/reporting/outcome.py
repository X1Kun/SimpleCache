"""Aggregate current and retained measurements without discarding failures."""
import collections
import json
import pathlib

def summarize_outcome(manifest):
    """Combine current and retained evidence; a rerun cannot erase failures."""
    counts = dict(errors=0, incorrect=0, stale_at_start=0, skipped=0,
                  empty_attempts=0, read_errors=0, write_errors=0, unclassified_errors=0)
    attempts = [(t, t['client']) for t in manifest['trials']]
    attempts += [(t, t['summary']) for t in manifest.get('prior_failed_attempts', [])]
    for attempt, summary in attempts:
        errors = summary.get('errors', 0)
        counts['errors'] += errors
        counts['incorrect'] += summary.get('incorrect', 0)
        counts['stale_at_start'] += summary.get('stale_at_start', 0)
        counts['skipped'] += max(summary.get('skipped', 0),
                                 summary['planned'] - summary['attempted'])
        counts['empty_attempts'] += summary['attempted'] == 0
        if not errors:
            continue
        # Older clients have no per-method error totals. Classify their retained
        # raw records rather than assuming every transport/status error is a GET.
        try:
            path = pathlib.Path(attempt['run_report']) / attempt['name'] / 'requests.jsonl'
            records = [json.loads(line) for line in path.read_text().splitlines()]
            by_method = collections.Counter(r.get('method') for r in records if r.get('error'))
            if sum(by_method.values()) != errors:
                raise ValueError('raw error count differs from summary')
        except (OSError, ValueError):
            counts['unclassified_errors'] += errors
            continue
        counts['read_errors'] += by_method.pop('GET', 0)
        for method in ['PUT', 'DELETE', 'POST', 'PATCH']:
            counts['write_errors'] += by_method.pop(method, 0)
        counts['unclassified_errors'] += sum(by_method.values())
    hard_failure = not attempts or any(counts[k] for k in [
        'incorrect', 'stale_at_start', 'skipped', 'empty_attempts', 'write_errors', 'unclassified_errors'])
    status = 'failed' if hard_failure else ('completed-with-read-errors' if counts['read_errors'] else 'passed')
    return {'status': status, 'counts': counts}

