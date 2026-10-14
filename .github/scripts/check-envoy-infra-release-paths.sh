#!/usr/bin/env bash
# release.yaml's envoy_infra job releases the envoy-infra Pulumi plugin only for a commit that
# touches one of the paths it names, in three places: the workflow's push paths (whether the run
# starts), the `changes` job's envoy_infra filter (whether the job runs) and the paths after `--`
# of the job's release-bump.sh call (whether a version is cut). The plugin compiles in packages of
# packages/envoy as well as of its own module, and renders grants from Envoy's exported subject
# names, so a commit to one of those packages must release it, and a list that misses one leaves
# that change unreleased with no error anywhere.
#
# So this check derives the set from the build itself: every package of this repository's own Go
# modules that `go list -deps ./cmd/...` names from packages/envoy/infra, for each target
# .github/envoy-infra-targets.json lists (GOOS and GOARCH decide which files, and so which imports,
# a build compiles in), and the go.mod and go.sum of each of those modules and the go.work and
# go.work.sum that bind them, which set the versions it builds with. It fails, naming each
# directory or file and each list that misses it.
#
# An entry covers a path when it is that path or a directory above it: in the push paths and the
# filter, the path itself or `<dir>/**`; after `--`, the path itself or `<dir>/`. Any other glob,
# a `!` negation or a `:(...)` pathspec is refused rather than guessed at.
#
# Run from anywhere: .github/scripts/check-envoy-infra-release-paths.sh
# CI runs it in the envoy-infra-go job of envoy-and-contracts.yaml, and its tests in the test job of
# pr-and-main.yaml.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

import yaml

WORKFLOW = Path(".github/workflows/release.yaml")
TARGETS = Path(".github/envoy-infra-targets.json")
MODULE_DIR = Path("packages/envoy/infra")
ROOT = Path.cwd()

problems: list[str] = []


def refuse(message: str) -> None:
    print(f"check-envoy-infra-release-paths: {message}", file=sys.stderr)
    sys.exit(1)


def go(*args: str, env: dict[str, str] | None = None) -> list[str]:
    result = subprocess.run(
        ["go", *args],
        cwd=MODULE_DIR,
        env={**os.environ, **(env or {})},
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        refuse(f"`go {' '.join(args)}` in {MODULE_DIR} failed:\n{result.stderr}")
    return [line for line in result.stdout.splitlines() if line]


def relative(path: str) -> str:
    return Path(path).resolve().relative_to(ROOT).as_posix()


# The repository's own modules: those the module's go.work binds, as `go list -m` names them. Their
# go.mod and go.sum, and the go.work and go.work.sum binding them, set the versions the build uses.
modules = {}
for line in go("list", "-m", "-f", "{{.Path}}\t{{.Dir}}\t{{.GoMod}}"):
    path, directory, gomod = line.split("\t")
    modules[path] = (relative(directory), relative(gomod))

required: dict[str, str] = {}
for module_path, (_, gomod) in modules.items():
    required[gomod] = f"{module_path}'s go.mod"
    gosum = Path(gomod).with_name("go.sum")
    if gosum.is_file():
        required[gosum.as_posix()] = f"{module_path}'s go.sum"
(gowork,) = go("env", "GOWORK")
required[relative(gowork)] = "the workspace file"
if Path(gowork + ".sum").is_file():
    required[relative(gowork + ".sum")] = "the workspace's checksums"

targets = json.loads(TARGETS.read_text())["envoy-infra"]
if not targets:
    refuse(f"{TARGETS} lists no envoy-infra target")
for target in targets:
    goos, _, goarch = target.partition("/")
    rows = go(
        "list",
        "-deps",
        "-f",
        "{{with .Module}}{{.Path}}{{end}}\t{{.ImportPath}}\t{{.Dir}}",
        "./cmd/...",
        env={"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"},
    )
    for row in rows:
        module_path, import_path, directory = row.split("\t")
        if module_path in modules:
            required[relative(directory) + "/"] = import_path

document = yaml.safe_load(WORKFLOW.read_text())
jobs = document["jobs"]


def step(job: str, step_id: str) -> dict:
    if job not in jobs:
        refuse(f"{WORKFLOW}: no job {job}")
    found = [s for s in jobs[job]["steps"] if s.get("id") == step_id]
    if len(found) != 1:
        refuse(f"{WORKFLOW}: expected one step with id {step_id} in job {job}, found {len(found)}")
    return found[0]


triggers = document.get("on", document.get(True))
push_paths = triggers["push"]["paths"]

filters = yaml.safe_load(step("changes", "filter")["with"]["filters"])
filter_paths = filters.get("envoy_infra")
if not filter_paths:
    refuse(f"{WORKFLOW}: the changes job's filter step has no envoy_infra filter")

# The version step's release-bump.sh call, without its comments and with its line continuations
# joined: the words after `--`.
script = "\n".join(
    line for line in step("envoy_infra", "version")["run"].splitlines() if not line.lstrip().startswith("#")
).replace("\\\n", " ")
calls = re.findall(r"release-bump\.sh([^\n]*)\)", script)
if len(calls) != 1:
    refuse(f"{WORKFLOW}: expected one release-bump.sh call in envoy_infra's version step, found {len(calls)}")
words = shlex.split(calls[0])
if words.count("--") != 1:
    refuse(f"{WORKFLOW}: envoy_infra's release-bump.sh call has no single `--` before its paths")
bump_paths = words[words.index("--") + 1 :]


def glob_readable(entry: str) -> bool:
    return not entry.startswith("!") and not any(c in entry.removesuffix("/**") for c in "*?[{")


def glob_covers(entry: str, path: str) -> bool:
    if entry.endswith("/**"):
        return path.startswith(entry.removesuffix("**"))
    return path == entry


def pathspec_readable(entry: str) -> bool:
    return not entry.startswith(":") and not any(c in entry for c in "*?[")


def pathspec_covers(entry: str, path: str) -> bool:
    base = entry.rstrip("/")
    return path.rstrip("/") == base or path.startswith(base + "/")


lists = [
    ("the workflow's push paths", push_paths, glob_readable, glob_covers, "a path or <dir>/**"),
    ("the changes job's envoy_infra filter", filter_paths, glob_readable, glob_covers, "a path or <dir>/**"),
    (
        "the paths after -- of envoy_infra's release-bump.sh call",
        bump_paths,
        pathspec_readable,
        pathspec_covers,
        "a path or <dir>/",
    ),
]
for name, entries, readable, _, form in lists:
    for entry in entries:
        if not readable(entry):
            refuse(f"{WORKFLOW}: {name} holds {entry!r}, which this check cannot read; it reads {form}")
for path, origin in sorted(required.items()):
    for name, entries, _, covers, _ in lists:
        if not any(covers(entry, path) for entry in entries):
            problems.append(f"{WORKFLOW}: {path} ({origin}) is missing from {name}")

if problems:
    print("\n".join(problems), file=sys.stderr)
    print(
        f"\nThe envoy-infra plugin builds from every path above (go list in {MODULE_DIR}, for each "
        f"target {TARGETS} lists), so a commit to one must release it: add each to the list that "
        "misses it.",
        file=sys.stderr,
    )
    sys.exit(1)
for path, origin in sorted(required.items()):
    print(f"  {path} ({origin})")
print(
    f"release.yaml's envoy_infra push paths, filter and bump paths cover the {len(required)} "
    f"packages and module files above, which the plugin builds from across {len(targets)} targets"
)
PY
