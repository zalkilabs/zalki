#!/usr/bin/env python3
"""Preserve make's linter arguments; add a JSON artifact for strict mode only."""
import os
import sys

arguments = sys.argv[1:]
if '--issues-exit-code' in arguments:
    arguments += ['--output.json.path', os.environ['STRICT_REPORT'], '--output.text.path', '/dev/null']
# Replace this wrapper so the caller owns and joins the actual linter process.
os.execv(os.environ['LINT_REAL_BINARY'], [os.environ['LINT_REAL_BINARY'], *arguments])
