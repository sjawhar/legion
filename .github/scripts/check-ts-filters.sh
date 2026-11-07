#!/usr/bin/env bash
# ci.yaml's `changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on the
# merge group that lands it, after which nothing runs it. check-ci-filters.sh holds every filter
# to every file ci_job_reads.py says its job reads; for a Go job that set is derived from
# `go list`, so an import cannot slip past it, while for a Bun job the declared reads are
# hand-kept beside the filter, and the two go short together.
#
# This check derives the TypeScript half of them. For each pair of a changes filter and the tsc
# project the job that filter gates typechecks (.github/scripts/ci_ts_programs.py, PROGRAMS), it
# runs `bunx tsc -p <project> --noEmit --listFilesOnly`, keeps the files inside the repository
# and outside node_modules, makes each path relative to the repository root, and holds every one
# of them to that filter, failing with the filter, the file and the program it came from. The
# pairs are hand-kept and few; the file list is derived, which is the point: an import that
# reaches a new package fails this check on the pull request that writes it, with nobody
# remembering to widen a list. --listFilesOnly lists the program and stops without checking a
# type, so this is no second typecheck: the job's own step is what compiles it.
#
# The pairs cover every gate whose job runs a typecheck, each read from that job's own steps:
# contracts, envoy_client (packages/envoy-client and packages/broker-client), pi_shared,
# pi_envoy, pi_legion, claude_envoy, dispatch_ui (packages/dispatch and packages/proof-editor),
# docs (docs/site, in the docs-site.yaml job ci.yaml's docs job calls), conflicts_script
# (scripts/conflicts) and test (scripts/dispatch, scripts/github-apps and scripts/sessions).
# The gates left out are the ones whose job runs TypeScript but no typecheck, so there is no
# program to derive: `dispatch`, whose job builds the dashboard and runs the browser suite while
# the dashboard's and the proof editor's typechecks are dispatch_ui's (ci.yaml's comment on
# dispatch-ui says why they were split out), `dispatch_browser_multiprocess`, which is that
# browser suite again in two-process mode, `envoy_session_stall`, whose job builds and runs the
# stall entries, and `envoy_e2e_local`, whose live proof renders with @legion/envoy-client at
# run time. What such a job loads is a runtime closure rather than a tsc program, and stays a
# hand-kept read of ci_job_reads.py: envoy_client_dep for the dispatch and envoy_e2e_local
# gates, a read of its own for the envoy_session_stall gate.
#
# Files a test reads by relative path - the JSON fixtures and corpora a test of one package
# reads out of another package's directory - are part of no tsc program, so they stay hand-kept
# too: the list of them is the tester's case in .github/scripts/check-ci-filters.test.sh, "every
# filter of a job that compiles or reads another package's files selects them", which measured
# each pair and holds it to the filter; ci_job_reads.py declares each of them as a read, and
# check-ci-filters.sh holds the filter to it.
#
# A file is held to the filter as workflow_paths.Filter reads it, over the filter's patterns with
# its anchors flattened into the list that names them, which the engine of check-ci-filters.sh
# reads out of the changes job (ci_filter_coverage.changes_filters) rather than this check
# reading that step a second way. A covered filter with a `!` pattern is refused instead of read:
# GitHub's `paths`, which Filter models, takes the last pattern that matches, while
# dorny/paths-filter's `some-with-excludes` lets a `!` pattern exclude and its default `some`
# makes one match every file outside it, so the three disagree about such a filter. ci.yaml's own
# comment on the quantifier requires every filter but worker_image, which gates no job of this
# table, to stay without one.
#
# Why ci.yaml's lint job and not ci-filters: the ci-filters job has no Bun and no node_modules -
# its steps are the checkout, actions/setup-go, check-ci-filters.sh and the ci-ok report - so
# `bunx tsc` there would install Bun and the dependency tree first, while lint already runs
# `bun install --frozen-lockfile` for the Biome pass; lint is gated by no `changes` output, so a
# change anywhere starts it, which is what a check of every filter needs; and the cost is small
# against that job's 30 minutes, 36.9 s measured over the nine package programs and 55 s over
# all fourteen of this table, in a pod with node_modules already installed.
#
# Run from anywhere: .github/scripts/check-ts-filters.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The pairs (ci_ts_programs.py), the path model (workflow_paths.py) and the engine this borrows
# the changes job's filters from (ci_filter_coverage.py) sit beside this script in the repository
# it checks. PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import subprocess
import sys
from pathlib import Path

import yaml

import ci_filter_coverage as engine
import workflow_paths as wp
from ci_ts_programs import PROGRAMS

TABLE = ".github/scripts/ci_ts_programs.py"

problems: list[str] = []


def listed(project: str) -> list | None:
    """Every file of the project's tsc program that is inside the repository and outside
    node_modules, repository-relative. None when the command fails: the program is then unknown,
    which is a failure of its own rather than a program of no files that every filter covers."""
    command = ["bunx", "tsc", "-p", project, "--noEmit", "--listFilesOnly"]
    result = subprocess.run(command, cwd=engine.REPO, capture_output=True, text=True)
    if result.returncode != 0:
        output = " / ".join((result.stdout + result.stderr).split()) or "no output"
        problems.append(f"::error file={project}::`{' '.join(command)}` failed (exit {result.returncode}), so the "
                        f"files its program holds are unknown: {output[:500]}")
        return None
    files = set()
    for line in result.stdout.splitlines():
        if not line.strip():
            continue
        path = Path(line.strip())
        if not path.is_absolute():
            path = engine.REPO / path
        try:
            relative = path.resolve().relative_to(engine.REPO)
        except ValueError:  # tsc's own lib files and anything else outside this repository
            continue
        if "node_modules" in relative.parts:
            continue
        files.add(relative.as_posix())
    return sorted(files)


document = yaml.safe_load((engine.REPO / engine.WORKFLOW).read_text())
filters, _quantifiers = engine.changes_filters(document, problems)

for name, project, why in PROGRAMS:
    patterns = filters.get(name)
    if patterns is None:
        problems.append(f"::error file={TABLE}::{name} is no filter of the changes job, so the program "
                        f"{project} ({why}) is held to nothing")
        continue
    negated = [pattern for pattern in patterns if pattern.startswith("!")]
    if negated:
        problems.append(f"::error file={engine.WORKFLOW}::filter {name} has {', '.join(negated)}, a `!` pattern, "
                        f"which GitHub's paths and dorny/paths-filter's two quantifiers read three ways; this "
                        f"check reads none of them for a filter it covers")
        continue
    if not (engine.REPO / project).exists():
        problems.append(f"::error file={TABLE}::{project} ({why}) is no path of this repository, so filter {name} "
                        f"is held to no program")
        continue
    files = listed(project)
    if files is None:
        continue
    covers = wp.Filter(patterns).covers
    uncovered = [path for path in files if not covers(path)]
    print(f"{project}: {len(files)} files, {len(uncovered)} uncovered (filter {name})")
    for path in uncovered[:engine.SHOWN]:
        problems.append(f"::error file={path}::filter {name} does not select {path}, which the tsc program of "
                        f"{project} compiles ({why}): the filter must select it, or the change that breaks that "
                        f"program starts no job")
    if len(uncovered) > engine.SHOWN:
        problems.append(f"::error file={engine.WORKFLOW}::filter {name} misses "
                        f"{len(uncovered) - engine.SHOWN} more file(s) of {project}")

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(f"::error::{len(problems)} problem(s) across {len(PROGRAMS)} tsc program(s)", file=sys.stderr)
    raise SystemExit(1)
print(f"every changes filter selects every file its job's typecheck compiles "
      f"({len(PROGRAMS)} programs, {len({name for name, _, _ in PROGRAMS})} filters)")
PY
