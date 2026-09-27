#!/usr/bin/env bash
# A Bun frozen install resolves every root workspace from its manifest. Docker's cache-friendly
# install layer therefore must COPY every root workspace package.json before each
# `bun install --frozen-lockfile` in the Dockerfile stage that runs it. Missing one fails the
# install only after a root workspace is added, so check the Dockerfile contract directly.
#
# Run from anywhere: .github/scripts/check-bun-workspace-manifests.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import re
import shlex
import sys
from pathlib import Path

SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
FROZEN_INSTALL = re.compile(r"\bbun\s+install\s+--frozen-lockfile\b")


def fatal(message: str) -> None:
    print(f"::error file=package.json::{message}", file=sys.stderr)
    raise SystemExit(1)


def read_text(path: Path) -> str:
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as error:
        print(f"::error file={path}::{path} is not readable as text: {error}", file=sys.stderr)
        raise SystemExit(1)


def dockerfiles() -> list[Path]:
    found = []
    stack = [Path(".")]
    while stack:
        for entry in sorted(stack.pop().iterdir()):
            if entry.is_symlink() and entry.is_dir():
                continue
            if entry.is_dir():
                if entry.name not in SKIP_DIRS:
                    stack.append(entry)
            elif entry.name == "Dockerfile" or entry.name.startswith("Dockerfile.") or entry.name.endswith(
                ".Dockerfile"
            ):
                found.append(entry)
    return found


def joined_lines(text: str):
    """(line number, instruction) with Dockerfile continuations joined."""
    pending, start = "", 0
    for number, raw in enumerate(text.splitlines(), start=1):
        stripped = raw.strip()
        if not pending:
            start = number
        if stripped.startswith("#"):
            continue
        if stripped.endswith("\\"):
            pending += stripped[:-1] + " "
            continue
        yield start, (pending + stripped).strip()
        pending = ""
    if pending:
        yield start, pending.strip()


class CopyParseError(ValueError):
    """A COPY instruction this check cannot read. Reading zero sources from one would look
    exactly like a manifest that is copied, so it is reported at its line instead."""


def copy_sources(instruction: str) -> list[str]:
    """Source paths for one Docker COPY instruction; flags and destination do not count."""
    arguments = instruction[4:].strip()
    if arguments.startswith("["):
        try:
            entries = json.loads(arguments)
        except json.JSONDecodeError as error:
            raise CopyParseError(f"malformed JSON-array COPY: {error.msg}") from error
        if not isinstance(entries, list) or not all(isinstance(item, str) for item in entries):
            raise CopyParseError("malformed JSON-array COPY: not an array of strings")
        if len(entries) < 2:
            raise CopyParseError("malformed JSON-array COPY: no source and destination")
        return [entry.removeprefix("./") for entry in entries[:-1]]
    try:
        entries = shlex.split(arguments)
    except ValueError as error:
        raise CopyParseError(f"unparseable COPY: {error}") from error
    paths = [entry.removeprefix("./") for entry in entries if not entry.startswith("--")]
    if len(paths) < 2:
        raise CopyParseError("unparseable COPY: no source and destination")
    return paths[:-1]


try:
    root_manifest = json.loads(read_text(Path("package.json")))
except FileNotFoundError:
    fatal("package.json is missing: root workspaces cannot be checked")
except json.JSONDecodeError as error:
    fatal(f"package.json is not valid JSON: {error.msg}")

workspaces = root_manifest.get("workspaces") if isinstance(root_manifest, dict) else None
if not isinstance(workspaces, list) or not all(isinstance(workspace, str) and workspace for workspace in workspaces):
    fatal("package.json must contain a non-empty array of workspace paths")

manifests = [f"{workspace.removesuffix('/')}/package.json" for workspace in workspaces]
missing_manifests = [manifest for manifest in manifests if not Path(manifest).is_file()]
if missing_manifests:
    for manifest in missing_manifests:
        print(f"::error file=package.json::workspace manifest {manifest} is missing", file=sys.stderr)
    raise SystemExit(1)

problems: list[tuple[str, str]] = []
checked = dockerfiles()
covered_installs = 0
for dockerfile in checked:
    copied: set[str] = set()
    frozen_installs = 0
    for line, instruction in joined_lines(read_text(dockerfile)):
        command = instruction.split(maxsplit=1)[0].upper() if instruction else ""
        if command == "FROM":
            copied.clear()
        elif command == "COPY":
            try:
                copied.update(copy_sources(instruction))
            except CopyParseError as error:
                problems.append((str(dockerfile), f"{dockerfile}:{line}: {error}"))
        elif command == "RUN" and FROZEN_INSTALL.search(instruction):
            frozen_installs += 1
            print(
                f"{dockerfile}: checked {len(manifests)} root workspaces before "
                f"bun install --frozen-lockfile at line {line}"
            )
            for manifest in manifests:
                if manifest not in copied:
                    problems.append(
                        (
                            str(dockerfile),
                            f"{dockerfile}:{line}: missing COPY for {manifest} before "
                            "bun install --frozen-lockfile",
                        )
                    )
    covered_installs += frozen_installs
    if not frozen_installs:
        print(f"{dockerfile}: checked {len(manifests)} root workspaces; no bun install --frozen-lockfile")

# Zero covered installs is the one result that would pass while checking nothing: a renamed
# Dockerfile, a moved install step, or a walk that found no Dockerfile at all.
if not covered_installs:
    problems.append(
        (
            "package.json",
            "no Dockerfile runs bun install --frozen-lockfile: this check covered 0 frozen "
            f"installs across {len(checked)} Dockerfile(s), so it proves nothing",
        )
    )

for path, problem in problems:
    print(f"::error file={path}::{problem}", file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} problem(s) across {len(checked)} Dockerfile(s)",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(
    f"every frozen Bun install copies {len(manifests)} root workspace manifests "
    f"({covered_installs} frozen install(s) across {len(checked)} Dockerfile(s))"
)
PY
