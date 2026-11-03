#!/usr/bin/env bash
# `legion gh` reads every command from the real gh's own help, and its tests hold its tables to the
# help of the gh the worker image ships, saved under `packages/daemon/cmd/legion/testdata/gh-help`
# with that gh's version in its `VERSION` file. The image's gh is `worker.Dockerfile`'s `ARG
# GH_TOOL=gh@X.Y.Z` default (nothing passes `--build-arg`, so the default is what every build
# resolves). If the pin moves and the saved help does not, the tests keep passing on the old help,
# and a command the new gh's help spells otherwise is first refused in a pane. So this check fails
# until the two name one gh: raising gh is the pin's edit and `testdata/regenerate-gh-help.sh`, run
# with the new gh, together. The self-hosted pool installs gh in
# `.github/actions/self-hosted-prereqs`, whose `gh_version=X.Y.Z` must name the same gh, so the
# daemon's gh-backed tests run against it on the pool as in the image; that edit carries the new
# release's `gh_sha256` too, which the action checks before it unpacks anything.
#
# Run from anywhere: .github/scripts/check-gh-version.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

DOCKERFILE = Path("packages/daemon/docker/worker.Dockerfile")
VERSION = Path("packages/daemon/cmd/legion/testdata/gh-help/VERSION")
PREREQS = Path(".github/actions/self-hosted-prereqs/action.yml")


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

action_pins = [
    (number, match.group(1))
    for number, line in enumerate(read(PREREQS).splitlines(), start=1)
    if (match := re.fullmatch(r"\s*gh_version=(\S*)\s*", line))
]
if len(action_pins) != 1:
    fatal(PREREQS, f"{PREREQS} must set gh_version exactly once, as gh_version=X.Y.Z; it does {len(action_pins)} times")
action_line, action_pin = action_pins[0]
if action_pin != pinned.group(1):
    fatal(
        PREREQS,
        f"{PREREQS}:{action_line}: gh_version={action_pin}, and {DOCKERFILE}:{line} pins gh "
        f"{pinned.group(1)}: set gh_version={pinned.group(1)} and gh_sha256 to the SHA-256 "
        f"gh_{pinned.group(1)}_checksums.txt lists for gh_{pinned.group(1)}_linux_amd64.tar.gz",
    )
print(f"the saved gh help and the self-hosted pool's gh are gh {saved.group(1)}, the worker image's pin")
PY
