"""Explicit CLI entrypoint; importing validation never starts an experiment."""
import argparse
import os
import pathlib
import signal

from .gateway.runner import GatewayRun, run_suite


def interrupted(signum, frame):
    raise KeyboardInterrupt(f'interrupted by signal {signum}')


def main(argv=None):
    parser = argparse.ArgumentParser(description='Owned Gateway cluster validation')
    parser.add_argument('--suite', choices=['smoke', 'extended', 'performance', 'arrival'],
                        help='Defaults to the existing GATEWAY_EXTENDED/GATEWAY_PERF environment flags')
    args = parser.parse_args(argv)
    settings = dict(os.environ)
    if args.suite:
        settings['GATEWAY_EXTENDED'] = str(int(args.suite == 'extended'))
        settings['GATEWAY_ARRIVAL'] = str(int(args.suite == 'arrival'))
        settings['GATEWAY_PERF'] = str(int(args.suite == 'performance'))
    previous = signal.signal(signal.SIGTERM, interrupted)
    try:
        root = pathlib.Path(__file__).resolve().parents[2]
        with GatewayRun(root, environ=settings) as context:
            run_suite(context)
    finally:
        signal.signal(signal.SIGTERM, previous)


if __name__ == '__main__':
    main()
