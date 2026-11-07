#!/usr/bin/env python3
"""The corpus files the search-measure scripts write and read back, and the line they say.

A corpus directory holds one JSON file (`projects.json`, `coverage.json`) or one JSONL file
(`issues.jsonl` and the rest) per kind: `harvest.py` writes them, `load_fixture.py` loads them
into a database and `build_cases.py` reads them beside the cases. Both halves live here so the
two never disagree about what a corpus file is.

A blank line is no row: these readers raise `json.JSONDecodeError` on one rather than skip it,
because a corpus file with a hole in it is a harvest to run again, not a measurement to make.
"""

import json
import os
import sys


def read_json(corpus, name):
    with open(os.path.join(corpus, name)) as handle:
        return json.load(handle)


def read_jsonl(corpus, name):
    with open(os.path.join(corpus, name)) as handle:
        return [json.loads(line) for line in handle]


def write_json(out, name, value):
    with open(os.path.join(out, name), "w") as handle:
        json.dump(value, handle)


def write_jsonl(out, name, rows):
    with open(os.path.join(out, name), "w") as handle:
        for row in rows:
            handle.write(json.dumps(row) + "\n")


def say(line):
    """One line of progress, on stderr, so a run's own output stays what it answered."""
    print(line, file=sys.stderr, flush=True)
