#!/usr/bin/env python3
"""The psql calls the search-measure scripts share.

Rows come back as JSON, one object per line: a document's markdown holds every character a text
format would have to escape, and `row_to_json` escapes them all.
"""

import json
import subprocess


def rows(dsn, sql):
    """Every row of sql as a dict.

    `select row_to_json(...)` under `-At`, not `copy ... to stdout`: COPY's text format escapes a
    backslash, which would double every escape the JSON already holds, and row_to_json escapes
    every control character itself, so each row is one line.
    """
    statement = f"select row_to_json(t)::text from ({sql}) t"
    result = subprocess.run(
        ["psql", dsn, "-v", "ON_ERROR_STOP=1", "-q", "-At", "-c", statement],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise SystemExit(f"psql: {result.stderr.strip()[:2000]}")
    return [json.loads(line) for line in result.stdout.splitlines() if line]


def table(dsn, sql):
    """sql as psql prints it, for a count a report quotes."""
    result = subprocess.run(
        ["psql", dsn, "-v", "ON_ERROR_STOP=1", "-c", sql], capture_output=True, text=True
    )
    if result.returncode != 0:
        raise SystemExit(f"psql: {result.stderr.strip()[:2000]}")
    return result.stdout
