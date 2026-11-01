"""Compatibility entrypoint; implementation lives in tools.validation."""
import pathlib
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2]))
from tools.validation.tests.test_performance import *  # legacy unittest discovery
