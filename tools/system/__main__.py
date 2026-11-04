"""Explicit retained development lifecycle; importing never starts infrastructure."""
import argparse
import pathlib
from .dev import Development, OperationBusy


def main():
    parser = argparse.ArgumentParser(description='Complete retained SimpleCache development environment')
    parser.add_argument('action', choices=['up','status','down','schema-upgrade','destroy'])
    arguments = parser.parse_args()
    environment = Development(pathlib.Path(__file__).resolve().parents[2])
    try:
        try:
            with environment.operation():
                getattr(environment, arguments.action.replace('-', '_'))()
        except OperationBusy:
            if arguments.action != 'status': raise
            print('Development operation in progress. Phase:', environment.state.get('phase'))
            print('Log:', environment.report/'commands.log')
    finally:
        environment.close()


if __name__ == '__main__': main()
