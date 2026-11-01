#!/usr/bin/env python3
"""Compatibility entrypoint; implementation lives in tools.validation."""
import pathlib
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
from tools.validation.__main__ import main

if __name__ == '__main__':
    main()
