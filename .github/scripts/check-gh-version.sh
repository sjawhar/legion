#!/usr/bin/env bash
# `legion gh` reads every command from the real gh's own help, and its tests hold its tables to the
# help of the gh the worker image ships, saved under `packages/daemon/cmd/legion/testdata/gh-help`
# with that gh's version in its `VERSION` file. The image's gh is `worker.Dockerfile`'s `ARG
# GH_TOOL=gh@X.Y.Z` default (nothing passes `--build-arg`, so the default is what every build
# resolves). If the pin moves and the saved help does not, the tests keep passing on the old help,
# and a command the new gh's help spells otherwise is first refused in a pane. So this check fails
# until the two name one gh: raising gh is the pin's edit and `testdata/regenerate-gh-help.sh`, run
# with the new gh, together.
#
# Run from anywhere: .github/scripts/check-gh-version.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

DOCKERFILE = Path("packages/daemon/docker/worker.Dockerfile")
VERSION = Path("packages/daemon/cmd/legion/testdata/gh-help/VERSION")


def fatal(path: Path, message: str) -> None:
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def read(path: Path) -> str:
    try:
        return path.read_bytes().decode("utf-8")
    except FileNotFoundError:
        fatal(path, f"{path} is missing")
    except UnicodeDecodeError as error:
        fatal(path, f"{path} is not readable as text: {error.reason}")


pins = [
    (number, match.group(1))
    for number, line in enumerate(read(DOCKERFILE).splitlines(), start=1)
    if (match := re.fullmatch(r"\s*ARG\s+GH_TOOL=(\S*)\s*", line))
]
if len(pins) != 1:
    fatal(DOCKERFILE, f"{DOCKERFILE} must default ARG GH_TOOL exactly once, as gh@X.Y.Z; it does {len(pins)} times")
line, pin = pins[0]
pinned = re.fullmatch(r"gh@(\d+\.\d+\.\d+)", pin)
if not pinned:
    fatal(DOCKERFILE, f"{DOCKERFILE}:{line}: ARG GH_TOOL={pin} is no exact gh release; write gh@X.Y.Z")

text = read(VERSION).strip()
saved = re.fullmatch(r"gh version (\d+\.\d+\.\d+)( \(\d{4}-\d{2}-\d{2}\))?", text)
if not saved:
    fatal(VERSION, f"{VERSION} holds {text!r}, not the first line of `gh --version`")
if saved.group(1) != pinned.group(1):
    fatal(
        VERSION,
        f"{VERSION} saves gh {saved.group(1)}'s help, and {DOCKERFILE}:{line} pins gh "
        f"{pinned.group(1)}: run packages/daemon/cmd/legion/testdata/regenerate-gh-help.sh with gh "
        f"{pinned.group(1)}",
    )
print(f"the saved gh help is gh {saved.group(1)}'s, the worker image's pin")
PY
