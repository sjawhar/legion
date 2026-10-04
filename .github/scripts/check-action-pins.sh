#!/usr/bin/env bash
# Every remote action this repository calls is pinned to the commit a tag resolves to, not to the
# tag itself (a `v5` can move to a commit nobody reviewed; a 40-hex SHA cannot). This check is the
# enforcement half of that convention: nothing else in CI refuses a `uses:` that names a floating
# tag.
#
# A `uses:` is in scope when it names a remote action (`owner/repo[/path]@ref`); a local path
# (`./...`) or a Docker reference (`docker://...`) is not pinned by a commit and is out of scope.
# A scoped `uses:` passes only when its ref, the text after the last `@`, is exactly 40 lowercase
# hex characters — a tag, a branch, a short SHA, or an upper-case SHA all fail, so a pin cannot
# half-drift back toward a moving ref. .github/dependabot.yml is what keeps a passing pin current;
# this script only refuses a pin that has already drifted or was never made.
#
# Scope: every workflow (.github/workflows/*.yml, *.yaml) and every composite action
# (**/action.yml, **/action.yaml), read as YAML, every step's `uses:` and every job's `uses:`
# (a reusable-workflow call). Files are discovered by walking the tree, not by asking git, so an
# unsnapshotted change cannot read green locally.
#
# Run from anywhere: .github/scripts/check-action-pins.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

import yaml


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where each key was written."""

    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
SHA = re.compile(r"^[0-9a-f]{40}$")


def walk() -> list[Path]:
    found = []
    stack = [Path(".")]
    while stack:
        for entry in sorted(stack.pop().iterdir()):
            if entry.is_symlink() and entry.is_dir():
                continue
            if entry.is_dir():
                if entry.name not in SKIP_DIRS:
                    stack.append(entry)
            else:
                found.append(entry)
    return found


files = walk()
targets = [
    path
    for path in files
    if (path.parent == Path(".github/workflows") and path.suffix in {".yml", ".yaml"})
    or path.name in {"action.yml", "action.yaml"}
]

problems: list[str] = []


def is_remote(uses: str) -> bool:
    """A local path or a Docker reference names no commit a tag could move; everything else
    (owner/repo[/path]@ref) is a pin this check covers."""
    return not (uses.startswith("./") or uses.startswith("../") or uses.startswith("docker://"))


def check_uses(path: Path, line: int, uses: str) -> None:
    if not is_remote(uses):
        return
    if "@" not in uses:
        problems.append(f"{path}:{line}: '{uses}' names no ref (no @) to pin")
        return
    ref = uses.rsplit("@", 1)[1]
    if not SHA.fullmatch(ref):
        problems.append(
            f"{path}:{line}: '{uses}' is pinned to '{ref}', not a 40-character commit SHA"
        )


def walk_steps(path: Path, steps: object) -> None:
    if not isinstance(steps, list):
        return
    for step in steps:
        if isinstance(step, Mapping) and "uses" in step:
            check_uses(path, step.lines.get("uses", 0), step["uses"])


for path in targets:
    try:
        doc = yaml.load(path.read_text(), Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"{path} is not valid YAML: {error}")
        continue
    if not isinstance(doc, Mapping):
        continue

    if path.name in {"action.yml", "action.yaml"}:
        runs = doc.get("runs")
        if isinstance(runs, Mapping):
            walk_steps(path, runs.get("steps"))
        continue

    jobs = doc.get("jobs")
    if not isinstance(jobs, Mapping):
        continue
    for job in jobs.values():
        if not isinstance(job, Mapping):
            continue
        if "uses" in job:
            check_uses(path, job.lines.get("uses", 0), job["uses"])
        walk_steps(path, job.get("steps"))

if problems:
    for problem in problems:
        print(f"::error::{problem}")
    print(f"{len(problems)} remote action(s) not pinned to a commit SHA", file=sys.stderr)
    sys.exit(1)

print(f"every remote action across {len(targets)} files is pinned to a commit SHA")
PY
