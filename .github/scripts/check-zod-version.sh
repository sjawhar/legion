#!/usr/bin/env bash
# Every workspace that shares schemas shares one zod 4. Oh My Pi's schema converter reads
# internals only its own zod instance produces, and `@legion/contracts` hands schemas to
# claude-envoy's `z.toJSONSchema`, so two zod 4 copies meeting in one call fail the typecheck or
# the load. A range lets `bun install` resolve a package to the newest zod and nest it beside the
# hoisted one; that is how a second zod 4 appeared once already. So each manifest names one exact
# version, the same one, and this check fails until they agree. Raising zod is every one of those
# edits together, and `bun install` rewrites the lockfile from them.
#
# The set is every manifest that declares zod — the root `package.json` and each workspace it
# lists — in any dependency section, so a new workspace joins it by declaring zod. One is left
# out by name: `packages/daemon`, the TypeScript daemon, which stays on zod 3 until Stage 7 of
# LEGION-208 deletes it. The exclusion asserts that reason: once that manifest names a zod that
# is not 3.x, this check fails and asks for the exclusion to go.
#
# It checks manifests, not `bun.lock`: an exact pin in every manifest leaves the lockfile nothing
# to nest, and a failure here names the manifest that drifted. Packages outside the workspace set
# (`@assistant-ui/react`, `@opencode-ai/plugin`) nest their own zod, which no Legion schema crosses.
#
# Run from anywhere: .github/scripts/check-zod-version.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import re
import sys
from collections import defaultdict
from pathlib import Path

SECTIONS = (
    "dependencies",
    "devDependencies",
    "peerDependencies",
    "optionalDependencies",
    "overrides",
    "resolutions",
)
EXACT = re.compile(r"\d+\.\d+\.\d+")
# The manifest left out of the set, and the major version that justifies leaving it out.
EXCLUDED = {"packages/daemon/package.json": "3"}


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines, or it reaches the Actions log as a
    line of its own."""
    return " ".join(str(value).split())


def fatal(path: str, message: str) -> None:
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def load(path: Path) -> tuple[dict, str]:
    try:
        text = path.read_bytes().decode("utf-8")
    except FileNotFoundError:
        fatal(str(path), f"{path} is missing")
    except UnicodeDecodeError as error:
        fatal(str(path), f"{path} is not readable as text: {one_line(error)}")
    try:
        manifest = json.loads(text)
    except json.JSONDecodeError as error:
        fatal(str(path), f"{path} is not valid JSON: {error.msg} at line {error.lineno}")
    if not isinstance(manifest, dict):
        fatal(str(path), f"{path} is not a JSON object")
    return manifest, text


def line_of(text: str, value: str) -> int:
    """The line declaring zod as `value`, so a problem names where to edit."""
    for number, line in enumerate(text.splitlines(), start=1):
        if re.search(r'"zod"\s*:', line) and f'"{value}"' in line:
            return number
    return 1


root, _ = load(Path("package.json"))
workspaces = root.get("workspaces")
if not isinstance(workspaces, list) or not all(
    isinstance(workspace, str) and workspace for workspace in workspaces
):
    fatal("package.json", "package.json must contain a non-empty array of workspace paths")

manifests = ["package.json"] + [f"{workspace.removesuffix('/')}/package.json" for workspace in workspaces]
problems: list[str] = []
# version -> the "path:line" of every declaration naming it
declared: dict[str, list[str]] = defaultdict(list)
for manifest in manifests:
    content, text = load(Path(manifest))
    for section in SECTIONS:
        entries = content.get(section)
        if not isinstance(entries, dict) or "zod" not in entries:
            continue
        value = entries["zod"]
        site = f"{manifest}:{line_of(text, str(value))}"
        if manifest in EXCLUDED:
            major = EXCLUDED[manifest]
            if not re.fullmatch(rf"[\^~]?{major}(\.\d+){{0,2}}", str(value)):
                problems.append(
                    f"{site}: {manifest} is left out of this check because it is on zod {major}, "
                    f"but its {section} name zod {one_line(value)} — remove it from EXCLUDED in "
                    f".github/scripts/check-zod-version.sh"
                )
            continue
        if not isinstance(value, str) or not EXACT.fullmatch(value):
            problems.append(
                f"{site}: {section} name zod {one_line(value)}, which is a range, not a release: "
                f"bun install can resolve it to a newer zod and nest it beside the hoisted one. "
                f"Write an exact X.Y.Z, the one every manifest names"
            )
            continue
        declared[value].append(site)

if not declared and not problems:
    fatal(
        "package.json",
        f"no manifest outside {', '.join(EXCLUDED)} declares zod: this check covered 0 of "
        f"{len(manifests)} manifests, so it proves nothing",
    )
if len(declared) > 1:
    most = max(len(sites) for sites in declared.values())
    common = [version for version, sites in declared.items() if len(sites) == most]
    summary = "; ".join(
        f"{version} in {', '.join(sites)}" for version, sites in sorted(declared.items())
    )
    for version, sites in sorted(declared.items()):
        if len(common) == 1 and version == common[0]:
            continue
        for site in sites:
            problems.append(
                f"{site}: names zod {version}, where the manifests disagree ({summary}). "
                f"Every manifest names one exact zod"
            )

for problem in problems:
    path = problem.split(":", 1)[0]
    print(f"::error file={path}::{problem}", file=sys.stderr)
if problems:
    print(f"::error::{len(problems)} zod declaration(s) do not name the one exact zod", file=sys.stderr)
    raise SystemExit(1)
(version, sites), = declared.items()
print(
    f"every zod declaration names {version} ({len(sites)} across {len(manifests)} manifests; "
    f"{', '.join(EXCLUDED)} excluded, on zod {', '.join(EXCLUDED.values())})"
)
PY
