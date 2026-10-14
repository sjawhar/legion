#!/usr/bin/env bash
# release-envoy-infra.yaml releases the envoy-infra Pulumi plugin only for a commit that touches a
# path it names, in two places: its push paths (whether the run starts) and
# .github/envoy-infra-release-paths, the pathspecs its version step passes release-bump.sh (whether
# a version is cut). The plugin compiles in packages of packages/envoy as well as of its own
# module, and renders grants from Envoy's exported subject names, so a commit to one of those
# packages must release it, and a list that misses one leaves that change unreleased with no error
# anywhere.
#
# So this check derives the set from the build itself: every package of this repository's own Go
# modules that `go list -deps ./cmd/...` names from packages/envoy/infra, for each target
# .github/envoy-infra-targets.json lists (GOOS and GOARCH decide which files, and so which imports,
# a build compiles in); the go.mod and go.sum of each of those modules and the go.work and
# go.work.sum that bind them, which set the versions it builds with; and
# .github/envoy-infra-release-paths itself, which the version step reads. It fails, naming each
# directory or file and each list that misses it.
#
# The push paths are read as GitHub reads them, with workflow_paths.py, the path model
# check-image-trigger-paths.sh uses: a package is covered when every file in its directory is. A
# line of .github/envoy-infra-release-paths covers a path when it is that path or a directory above
# it; a glob or a `:(...)` pathspec is refused rather than guessed at. So is a version step that
# does not pass release-bump.sh exactly the file's lines.
#
# Run from anywhere: .github/scripts/check-envoy-infra-release-paths.sh
# CI runs it in the required lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# PYTHONDONTWRITEBYTECODE keeps the import of workflow_paths.py from leaving a __pycache__ in the
# tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

import yaml
from workflow_paths import Filter, events, unread_patterns

WORKFLOW = Path(".github/workflows/release-envoy-infra.yaml")
RELEASE_PATHS = Path(".github/envoy-infra-release-paths")
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
for line in go("list", "-m", "-f", "{{.Path}}\t{{.GoMod}}"):
    path, gomod = line.split("\t")
    modules[path] = relative(gomod)

required: dict[str, str] = {RELEASE_PATHS.as_posix(): "the paths a version is cut from"}
for module_path, gomod in modules.items():
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

# The push paths, read with GitHub's meaning.
push = events(document).get("push")
push_paths = push.get("paths") if isinstance(push, dict) else None
if not push_paths:
    refuse(f"{WORKFLOW}: on.push names no paths")
unread = unread_patterns(push_paths)
if unread:
    refuse(
        f"{WORKFLOW}: the push paths hold {unread!r}, where GitHub reads ?, + and \\ with meanings "
        "this check does not follow; write them without those characters"
    )
push_filter = Filter(push_paths)

# The paths a version is cut from: one pathspec per line, a path or a directory.
release_paths = RELEASE_PATHS.read_text().splitlines()
for line in release_paths:
    if not line or line != line.strip() or line.startswith(":") or any(c in line for c in "*?["):
        refuse(
            f"{RELEASE_PATHS}: cannot read the line {line!r}; each line is a path or a directory, "
            "with no glob, `:(...)` pathspec or surrounding whitespace"
        )

# The version step must pass release-bump.sh exactly those lines.
steps = [s for job in document.get("jobs", {}).values() for s in job.get("steps", []) if s.get("id") == "version"]
if len(steps) != 1:
    refuse(f"{WORKFLOW}: expected one step with id version, found {len(steps)}")
script = "\n".join(
    line for line in steps[0]["run"].splitlines() if not line.lstrip().startswith("#")
).replace("\\\n", " ")
reads = rf"^\s*mapfile -t paths < {re.escape(RELEASE_PATHS.as_posix())}\s*$"
if not re.search(reads, script, re.M):
    refuse(f"{WORKFLOW}: the version step does not read {RELEASE_PATHS} with `mapfile -t paths < {RELEASE_PATHS}`")
calls = re.findall(r"release-bump\.sh([^\n]*)\)", script)
if len(calls) != 1:
    refuse(f"{WORKFLOW}: expected one release-bump.sh call in the version step, found {len(calls)}")
words = shlex.split(calls[0])
if words.count("--") != 1 or words[words.index("--") + 1 :] != ["${paths[@]}"]:
    refuse(
        f'{WORKFLOW}: the version step\'s release-bump.sh call must end `-- "${{paths[@]}}"`, the '
        f"lines of {RELEASE_PATHS}, and no other path"
    )


def files_of(path: str) -> list[str]:
    """The files a required path stands for: a package directory's own files, or the file."""
    if path.endswith("/"):
        return sorted(entry.as_posix() for entry in Path(path).iterdir() if entry.is_file())
    return [path]


def release_paths_cover(path: str) -> bool:
    return any(path.rstrip("/") == line.rstrip("/") or path.startswith(line.rstrip("/") + "/") for line in release_paths)


for path, origin in sorted(required.items()):
    uncovered = [f for f in files_of(path) if not push_filter.covers(f)]
    if uncovered:
        problems.append(f"{path} ({origin}) is missing from {WORKFLOW}'s push paths: {uncovered[0]} is not covered")
    if not release_paths_cover(path):
        problems.append(f"{path} ({origin}) is missing from {RELEASE_PATHS}")

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
    f"{WORKFLOW}'s push paths and {RELEASE_PATHS} cover the {len(required)} packages and files "
    f"above, which the plugin builds from across {len(targets)} targets"
)
PY
