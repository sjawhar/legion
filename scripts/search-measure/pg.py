#!/usr/bin/env python3
"""The psql calls the search-measure scripts share.

Rows come back as JSON, one object per line: a document's markdown holds every character a text
format would have to escape, and `row_to_json` escapes them all.
"""

import json
import subprocess


def run(dsn, *arguments, label="psql", limit=2000, tolerate=False):
    """psql against dsn with `ON_ERROR_STOP=1` and arguments, answering its stdout.

    A non-zero return raises `SystemExit` reading "<label>: <stderr>", unless tolerate. label is
    what that line calls this call and limit bounds the stderr it quotes (None quotes all of it):
    both are the caller's, because these lines are the harness's own failure messages.
    """
    result = subprocess.run(
        ["psql", dsn, "-v", "ON_ERROR_STOP=1", *arguments], capture_output=True, text=True
    )
    if result.returncode != 0 and not tolerate:
        stderr = result.stderr.strip()
        raise SystemExit(f"{label}: {stderr if limit is None else stderr[:limit]}")
    return result.stdout


def rows(dsn, sql):
    """Every row of sql as a dict.

    `select row_to_json(...)` under `-At`, not `copy ... to stdout`: COPY's text format escapes a
    backslash, which would double every escape the JSON already holds, and row_to_json escapes
    every control character itself, so each row is one line.
    """
    statement = f"select row_to_json(t)::text from ({sql}) t"
    out = run(dsn, "-q", "-At", "-c", statement)
    return [json.loads(line) for line in out.splitlines() if line]
