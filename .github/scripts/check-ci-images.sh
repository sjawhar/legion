#!/usr/bin/env bash
# A GitHub-hosted runner shares one anonymous pull quota with every other runner on its address,
# so a CI job that fetches an image from a third-party registry fails for a reason the change
# under test did not cause. Every image a CI job of this repository fetches is therefore one copy
# in one package this repository publishes, ghcr.io/sjawhar/legion/ci, and every reference names
# that copy by digest. .github/ci-images.yaml holds one row per distinct upstream image, and
# .github/workflows/mirror-images.yaml copies the rows the package does not hold yet with
# `crane copy`, which keeps the manifest digest. This check holds the two sides to each other.
#
# It fails, naming the file, the line and the remedy, on:
#
#   1. a row this check cannot read: a row that is not a mapping, a missing or empty `upstream`,
#      `tag` or `digest`, or a digest that is not `sha256:` and 64 hex characters. A tag is
#      required because an untagged manifest is a package version nothing names, and a retention
#      policy may delete a digest a job pins.
#   2. two rows with the same digest, or two rows with the same tag. The row is what says which
#      image a digest is — which upstream it copies, and which version that upstream carries — so
#      one digest names one row and one row names one tag.
#   3. a row whose upstream carries a version this repository pins elsewhere and disagrees with
#      that pin: an `oven/bun` row against `.bun-version` (two such rows, one of them with the
#      `-slim` suffix), and a `library/golang` row against `ARG GO_VERSION` of
#      packages/daemon/docker/worker.Dockerfile. Raising Bun or Go is then one edit per place,
#      made together.
#   4. a reference in CI's reach that names a registry other than ghcr.io/sjawhar/legion/ci,
#   5. a reference in CI's reach that is not pinned by digest (`@sha256:` and 64 hex characters):
#      a tag is a name the registry can move, so only a digest names the bytes a job ran,
#   6. a reference in CI's reach whose digest no row of .github/ci-images.yaml holds,
#   7. a row whose digest the package does not hold: the manifest is read with an anonymous
#      `HEAD` against ghcr.io. Its message names .github/ci-images.yaml and says that
#      .github/workflows/mirror-images.yaml publishes a row when that file changes, so the usual
#      remedy is to merge the row and let the mirror run.
#
# CI'S REACH is every .github/workflows/*.yaml, every .github/actions/*/action.yml, and each
# other file a CI job runs that names an image. The first two kinds come from those two globs;
# REACH below is the explicit list of the other files, each entry a file and never a directory.
# A REACH entry that is gone fails this check rather than silently checking nothing. A
# .github/workflows/*.yml, or a .github/actions/*/action.yaml, fails too: this check reads the
# two spellings the repository uses, and a third would be in no list.
#
# A REFERENCE in those files is one of:
#   * a job's `services:` or `container:` image, read from the parsed YAML, never grepped;
#   * a buildx `driver-opts` `image=` value;
#   * a `FROM` argument, continuations joined. A `FROM` naming an earlier stage of the same
#     Dockerfile, or an `ARG` the Dockerfile declares, is not a reference;
#   * the image a `docker pull`, `docker run` or `docker create` fetches, in a `run:` script or a
#     shell file. Every literal reference the command holds is read. A command holding none must
#     name a variable this check can follow: one the same file assigns a literal reference to, or
#     one IMAGE_FROM lists with where its value comes from and why that source is already checked.
#     A command naming neither is refused, because an image this check cannot read is an image no
#     row has to hold;
#   * a string of a Go or a TypeScript source, or a shell assignment, whose value reads as a
#     reference. Every string, not only a `const`: nats-stream.test.ts writes its image as one
#     argument of a `docker("run", …)` call. reads_as_reference says which values are read as
#     one and which written-alike things (a Go import path, a TypeScript module specifier, a
#     host and port) are not.
# A line this check cannot read is a refusal that names it, never a silent pass.
#
# THE EXCEPTION LIST. Besides the reach, this check sweeps every text file of the repository and
# fails on any file that names mirror.gcr.io, the registry CI used before, unless EXCEPTIONS
# names that file. Each entry carries exactly one ground: `no CI job runs it`, or `a CI job runs
# it, and it fetches nothing`. An entry on a file that no longer names mirror.gcr.io fails too:
# a ground that holds nothing any more is a ground nobody reads again. An entry naming a file
# the tree does not hold admits nothing, and is left alone. The sweep skips .git, .jj,
# node_modules, .venv, vendor, .legion (Legion's per-issue run data, which a retro removes) and
# this check's own two files, whose text names the registry to find it.
#
# Files are discovered by walking the tree, not by asking git, so an unsnapshotted change cannot
# read green locally.
#
# CHECK 7 AND THE NETWORK. The read of the package is one anonymous token request
# (`GET https://ghcr.io/token?scope=repository:sjawhar/legion/ci:pull&service=ghcr.io`) and one
# `HEAD https://ghcr.io/v2/sjawhar/legion/ci/manifests/<digest>` per row, with an `Accept` naming
# the four index and manifest media types. CI_IMAGES_MANIFEST_HEAD is the seam that read hangs
# on: when it is set, this check runs `"$CI_IMAGES_MANIFEST_HEAD" <digest>` once per row instead
# and reads the HTTP status code the command prints on stdout (200 = the package holds the
# digest, 404 = it does not). check-ci-images.test.sh sets it to a small script of its own, so no
# case of that test reaches the network.
#
# Run from anywhere: .github/scripts/check-ci-images.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# PYTHONDONTWRITEBYTECODE keeps the yaml import from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 python3 <<'PY'
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

import yaml


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where it was written. The lines are attributes, not keys: a
    document is never searched for a key the loader invented."""

    line: int
    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)


def line_of(value: object) -> int:
    return getattr(value, "line", 0)


def key_line(mapping: object, name: str) -> int:
    return getattr(mapping, "lines", {}).get(name, line_of(mapping))


# ---------------------------------------------------------------- data
REGISTRY = "ghcr.io/sjawhar/legion/ci"
REPOSITORY = "sjawhar/legion/ci"
CI_IMAGES = Path(".github/ci-images.yaml")
MIRROR_WORKFLOW = ".github/workflows/mirror-images.yaml"
BUN_PIN = Path(".bun-version")
WORKER_DOCKERFILE = Path("packages/daemon/docker/worker.Dockerfile")
MIRROR = "mirror.gcr.io"
SKIP_DIRS = {".git", ".jj", ".legion", "node_modules", ".venv", "vendor"}
SELF = {".github/scripts/check-ci-images.sh", ".github/scripts/check-ci-images.test.sh"}
ACCEPT = ",".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)

# Every file a CI job runs that names a container image. Each entry is a file: a directory would
# grow a file this check never read.
REACH = (
    "packages/envoy/docker/Dockerfile",
    "packages/envoy/infra/host/heldportwrapper/Dockerfile",
    "packages/daemon/docker/worker.Dockerfile",
    "packages/daemon/internal/testnats/nats.go",
    "packages/envoy/internal/testnats/nats.go",
    "packages/envoy/internal/testpg/testpg.go",
    "packages/envoy/internal/tests3/seaweedfs.go",
    "packages/envoy-client/src/__tests__/nats-server.ts",
    "scripts/e2e/lib/nats-stream.test.ts",
    "packages/pi-legion/scripts/skill-scenarios/rig.sh",
    "scripts/e2e/stage1-skeleton.sh",
)

# A docker command whose image is a variable, and where that variable's value comes from. The
# key is (file, variable name); the value is why the image it carries is checked elsewhere.
IMAGE_FROM = {
    (".github/actions/pull-test-images/action.yml", "image"): (
        "each image is read from a Go string constant the caller's `consts:` input names "
        "(packages/daemon/internal/testnats/nats.go, packages/envoy/internal/testnats/nats.go, "
        "packages/envoy/internal/testpg/testpg.go, packages/envoy/internal/tests3/seaweedfs.go), "
        "or comes from the `images:` input, which ci.yaml's envoy-smoke job fills with the FROM "
        "arguments of packages/envoy/docker/Dockerfile; every one of those files is in the reach "
        "above, and this check reads the references in them"
    ),
    (".github/workflows/ci.yaml", "image"): (
        "the dispatch job's step reads it from `const Image` of "
        "packages/envoy/internal/tests3/seaweedfs.go, which is in the reach above"
    ),
    (".github/workflows/ci.yaml", "ENVOY_SMOKE_IMAGE"): (
        "the local tag the envoy-smoke job's own build loaded (ci.yaml sets it to "
        "legion-envoy-smoke:<sha>); no registry holds it, so the run fetches nothing"
    ),
}

NO_CI_JOB = "no CI job runs it"
FETCHES_NOTHING = "a CI job runs it, and it fetches nothing"

# Every file outside the reach that may name mirror.gcr.io, and the one ground that admits it.
# `no CI job runs it`: the devbox live proofs and their libraries, the compose files, the
# walkthrough recorder and documentation. `a CI job runs it, and it fetches nothing`: a script a
# CI job runs whose mirror.gcr.io text no job ever hands to a registry — dev-postgres.test.sh,
# which the envoy-go job runs, installs a fake `docker` on PATH, and check-bun-version.test.sh
# writes its references into fixture files under a temporary directory.
EXCEPTIONS = {
    "AGENTS.md": NO_CI_JOB,
    "docs/kubernetes.md": NO_CI_JOB,
    "docs/plans/2026-10-14-dispatch-gcp-plan.md": NO_CI_JOB,
    "docs/plans/2026-10-14-envoy-infra-plan.md": NO_CI_JOB,
    "docs/solutions/github/an-image-pull-that-fails-in-ci-is-this-repositorys-own-package-read-the-failure-do-not-re-run-INFRA-567.md": NO_CI_JOB,
    "docs/site/media/walkthroughs/legion-operator/record-casts.sh": NO_CI_JOB,
    "docs/solutions/testing/a-docker-run-line-is-proven-from-a-pod-with-no-container-runtime-as-a-two-container-job-whose-client-ships-in-the-image-pr2498.md": NO_CI_JOB,
    "packages/envoy/deploy/compose/dispatch.acceptance.compose.yml": NO_CI_JOB,
    "packages/envoy/deploy/compose/single-node.compose.yml": NO_CI_JOB,
    "scripts/e2e/README.md": NO_CI_JOB,
    "scripts/e2e/burstable-kind.sh": NO_CI_JOB,
    "scripts/e2e/claude-backend-sandbox.sh": NO_CI_JOB,
    "scripts/e2e/claude-backend-tmux.sh": NO_CI_JOB,
    "scripts/e2e/controller-start-tmux.sh": NO_CI_JOB,
    "scripts/e2e/controller-sweep-kind.sh": NO_CI_JOB,
    "scripts/e2e/dispatch-user-turns.sh": NO_CI_JOB,
    "scripts/e2e/lib/bridge-tunnel.sh": NO_CI_JOB,
    "scripts/e2e/lib/kind-sandbox.sh": NO_CI_JOB,
    "scripts/e2e/share-volumes-kind.sh": NO_CI_JOB,
    "scripts/e2e/stage2-tmux-supervision.sh": NO_CI_JOB,
    "scripts/e2e/stage3-devbox-workflow.sh": NO_CI_JOB,
    "scripts/e2e/stage4a-sandbox-runtime.sh": NO_CI_JOB,
    "scripts/e2e/stage4b-sandbox-tree.sh": NO_CI_JOB,
    "scripts/e2e/verifiers-staging-token.sh": NO_CI_JOB,
    ".github/scripts/check-bun-version.test.sh": FETCHES_NOTHING,
    "packages/daemon/internal/testpg/testpg.go": FETCHES_NOTHING,
    "packages/envoy/scripts/corpus-copy.sh": FETCHES_NOTHING,
    "packages/envoy/scripts/dev-postgres.sh": FETCHES_NOTHING,
    "packages/envoy/scripts/dev-postgres.test.sh": FETCHES_NOTHING,
}

# ---------------------------------------------------------------- reference grammar
HOST = re.compile(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?")
COMPONENT = re.compile(r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*")
TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9._-]{0,127}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
VARIABLE = re.compile(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?")
ASSIGNMENT = re.compile(r"^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(\S*)")
STRING = re.compile(r"\"([^\"\n\\]*)\"|'([^'\n\\]*)'|`([^`\n\\]*)`")
GO_VERSION_ARG = re.compile(r"^ARG\s+GO_VERSION=(\S+)\s*$")
FETCH_VERBS = {"pull", "run", "create"}

problems: list[str] = []
used_image_from: set[tuple[str, str]] = set()


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: a block scalar in a workflow would
    otherwise reach the Actions log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def fatal(message: str) -> None:
    """Nothing downstream can be judged without the rows, so stop here."""
    print(f"::error file={CI_IMAGES}::{message}", file=sys.stderr)
    raise SystemExit(1)


def split_reference(text: str) -> tuple[str, str, str, str] | None:
    """(host, path, tag, digest) for a string shaped like an image reference, else None. `host`
    and `tag` and `digest` are "" where the reference carries none."""
    if not text or any(character.isspace() for character in text) or "//" in text:
        return None
    body, at, digest = text.partition("@")
    if at and not DIGEST.fullmatch(digest):
        return None
    head, slash, last = body.rpartition("/")
    name, tag = body, ""
    if ":" in last:
        base, _, tag = last.partition(":")
        name = head + slash + base
        if not TAG.fullmatch(tag):
            return None
    parts = name.split("/")
    host = ""
    if len(parts) > 1 and ("." in parts[0] or ":" in parts[0] or parts[0] == "localhost"):
        host, parts = parts[0], parts[1:]
        if not HOST.fullmatch(host):
            return None
    if not parts or not all(COMPONENT.fullmatch(part) for part in parts):
        return None
    return host, "/".join(parts), tag, digest


def reads_as_reference(text: str) -> bool:
    """A value this check reads as an image reference where its position does not already say it
    is one: a Go or TypeScript string, a shell assignment, a word of a docker command. Three
    shapes are read as one, and the reason each needs a rule of its own is that other things are
    written the same way:
      * anything carrying a digest. Nothing else is;
      * a registry or a namespace and a tag (`mirror.gcr.io/library/nats:2.10`, `oven/bun:1.4.4`).
        A Go import path (`github.com/owner/module`) is shaped like the untagged form, so a tag
        or a digest is required here;
      * one name and a tag (`nats:2.10`), where the tag starts with a digit or is `latest` and
        the name is not an address. A TypeScript module specifier (`node:crypto`, `bun:test`)
        and a host and port (`127.0.0.1:4222`) are written the same way, and the tag, which
        names a release, is what tells them apart."""
    parts = split_reference(text)
    if not parts:
        return False
    host, path, tag, digest = parts
    if digest:
        return True
    if not tag:
        return False
    if host or "/" in path:
        return True
    return (tag[0].isdigit() or tag == "latest") and not re.fullmatch(r"[0-9.]+", path)


def unquote(word: str) -> str:
    for quote in ('"', "'"):
        if len(word) > 1 and word.startswith(quote) and word.endswith(quote):
            return word[1:-1]
    return word


def read_text(path: Path) -> str | None:
    """A file's text, or None where it is not decodable as UTF-8."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError:
        return None


def locate(lines: list[str], start: int, needle: str) -> int:
    """The first line at or after `start` (1-based) holding `needle`, else `start`. A problem in
    a `run:` script is reported where the reader has to edit, not at the step's first line."""
    for number in range(max(start, 1), len(lines) + 1):
        if needle in lines[number - 1]:
            return number
    return start


# ---------------------------------------------------------------- the rows
if not CI_IMAGES.is_file():
    fatal(f"{CI_IMAGES} is missing: it is the one place that says which image a digest is")
try:
    document = yaml.load(CI_IMAGES.read_text(), Loader=LineLoader)
except yaml.YAMLError as error:
    fatal(f"{CI_IMAGES} is not valid YAML: {one_line(error)}")
if not isinstance(document, dict):
    fatal(f"{CI_IMAGES} is not a mapping of `registry:` and `images:`")
if document.get("registry") != REGISTRY:
    fatal(
        f"{CI_IMAGES}:{key_line(document, 'registry')}: registry is "
        f"{one_line(document.get('registry'))}, and every consumer names {REGISTRY}. Rewrite the "
        f"row, or change this check and every reference with it."
    )
rows = document.get("images")
if not isinstance(rows, list) or not rows:
    fatal(f"{CI_IMAGES}:{key_line(document, 'images')}: images is empty, so nothing is checked")

readable: list[tuple[int, str, str, str]] = []  # line, upstream, tag, digest
for row in rows:
    line = line_of(row) or key_line(document, "images")
    if not isinstance(row, dict):
        problems.append(f"{CI_IMAGES}:{line}: this row is not a mapping of upstream, tag, digest")
        continue
    upstream, tag, digest = (row.get(key) for key in ("upstream", "tag", "digest"))
    if not isinstance(upstream, str) or not upstream.strip():
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'upstream')}: this row has no upstream, and "
            f"{MIRROR_WORKFLOW} copies from it"
        )
        continue
    if not isinstance(tag, str) or not tag.strip():
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'tag')}: the row for {one_line(upstream)} has no tag. "
            f"An untagged manifest is a package version nothing names, and a retention policy "
            f"may delete a digest a job pins. Give it one: the upstream repository's last path "
            f"element, then the upstream tag."
        )
        continue
    if not isinstance(digest, str) or not DIGEST.fullmatch(digest.strip()):
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'digest')}: the row for {one_line(upstream)} has digest "
            f"{one_line(digest)}, which is not sha256: and 64 hex characters. Read it with "
            f"`crane digest {one_line(upstream)}`."
        )
        continue
    readable.append((line, upstream.strip(), tag.strip(), digest.strip()))

by_digest: dict[str, tuple[int, str, str, str]] = {}
by_tag: dict[str, tuple[int, str, str, str]] = {}
for row in readable:
    line, upstream, tag, digest = row
    earlier = by_digest.get(digest)
    if earlier is not None:
        problems.append(
            f"{CI_IMAGES}:{line}: {digest} is also the digest of the row for "
            f"{one_line(earlier[1])} on line {earlier[0]}. The row is what says which image a "
            f"digest is, so one digest names one row: delete one of them."
        )
    else:
        by_digest[digest] = row
    earlier = by_tag.get(tag)
    if earlier is not None:
        problems.append(
            f"{CI_IMAGES}:{line}: tag {tag} is also the tag of the row for {one_line(earlier[1])} "
            f"on line {earlier[0]}, and the second copy would move the tag off the first row's "
            f"digest. Give one of them another tag."
        )
    else:
        by_tag[tag] = row


def upstream_parts(upstream: str) -> tuple[str, str, str, str]:
    """(host, path, tag, digest) of a row's upstream, each "" where the reference carries none."""
    return split_reference(upstream) or ("", "", "", "")


# Rule 3: a row whose upstream carries a version this repository pins elsewhere.
if not BUN_PIN.is_file():
    fatal(f"{BUN_PIN} is missing: the oven/bun rows of {CI_IMAGES} carry the version it pins")
bun_pin = BUN_PIN.read_text().strip()
worker_text = read_text(WORKER_DOCKERFILE)
if worker_text is None:
    fatal(
        f"{WORKER_DOCKERFILE} is missing or is not readable as text: the library/golang row of "
        f"{CI_IMAGES} carries the version its ARG GO_VERSION pins"
    )
go_pin = ""
for text in worker_text.splitlines():
    found = GO_VERSION_ARG.match(text)
    if found:
        go_pin = found.group(1)
if not go_pin:
    fatal(f"{WORKER_DOCKERFILE} declares no ARG GO_VERSION=<version>, which a golang row copies")

bun_rows: list[tuple[int, str, str]] = []  # line, upstream, the tag that upstream carries
for line, upstream, _, _ in readable:
    _, repository, carried, _ = upstream_parts(upstream)
    if repository == "oven/bun":
        bun_rows.append((line, upstream, carried))
wanted = {bun_pin, f"{bun_pin}-slim"}
for line, upstream, carried in bun_rows:
    if carried not in wanted:
        problems.append(
            f"{CI_IMAGES}:{line}: this row copies {one_line(upstream)}, and {BUN_PIN} locks "
            f"{bun_pin}. Raise both together: edit this row to oven/bun:{bun_pin} (or "
            f"oven/bun:{bun_pin}-slim), name its new digest, and repoint every FROM on it."
        )
for tag in sorted(wanted):
    if not any(carried == tag for _, _, carried in bun_rows):
        problems.append(
            f"{CI_IMAGES}:{key_line(document, 'images')}: no row copies oven/bun:{tag}, and this "
            f"repository builds FROM it ({BUN_PIN} locks {bun_pin}). Add the row, and "
            f"{MIRROR_WORKFLOW} publishes it."
        )
for line, upstream, _, _ in readable:
    _, repository, carried, _ = upstream_parts(upstream)
    if repository != "library/golang":
        continue
    if carried.partition("-")[0] != go_pin:
        problems.append(
            f"{CI_IMAGES}:{line}: this row copies {one_line(upstream)}, and "
            f"{WORKER_DOCKERFILE} pins ARG GO_VERSION={go_pin}. Raise both together: edit this "
            f"row to golang:{go_pin}-alpine, name its new digest, and repoint every FROM on it."
        )

# ---------------------------------------------------------------- CI's reach
workflows = sorted(Path(".github/workflows").glob("*.yaml"))
actions = sorted(Path(".github/actions").glob("*/action.yml"))
for stray in sorted(Path(".github/workflows").glob("*.yml")) + sorted(
    Path(".github/actions").glob("*/action.yaml")
):
    problems.append(
        f"{stray}:1: CI's reach is .github/workflows/*.yaml and .github/actions/*/action.yml, so "
        f"this file is in no list and its images are checked by nothing. Rename it, or widen the "
        f"two globs in this check."
    )
reach: list[Path] = list(workflows) + list(actions)
for name in REACH:
    path = Path(name)
    if not path.is_file():
        problems.append(
            f"{name}:1: CI's reach names this file, and it is gone. Delete its line from REACH in "
            f"this check, or restore the file."
        )
        continue
    reach.append(path)
reach_names = {str(path) for path in reach}

references: list[tuple[str, int, str, str]] = []  # file, line, text, how


def note(path: Path, line: int, text: str, how: str) -> None:
    references.append((str(path), line, text, how))


def judge(path: str, line: int, text: str, how: str) -> None:
    """Rules 4, 5 and 6: one reference, read against the rows."""
    parts = split_reference(text)
    if parts is None:
        problems.append(
            f"{path}:{line}: {how} is {one_line(text)}, which this check cannot read as an image "
            f"reference. Write {REGISTRY}@<digest>, naming a digest {CI_IMAGES} holds."
        )
        return
    host, repository, tag, digest = parts
    named = f"{host}/{repository}" if host else repository
    if named != REGISTRY:
        problems.append(
            f"{path}:{line}: {how} names {one_line(named)}, and every image a CI job fetches is "
            f"a copy in {REGISTRY}. Add a row for it to {CI_IMAGES}, which {MIRROR_WORKFLOW} "
            f"copies, and name that row's digest here."
        )
        return
    if not digest:
        problems.append(
            f"{path}:{line}: {how} is {one_line(text)}, which names "
            f"{'tag ' + tag if tag else 'no digest'}. A tag is a name the registry can move, so "
            f"name the digest instead: {REGISTRY}@<digest> from {CI_IMAGES}."
        )
        return
    if digest not in by_digest:
        problems.append(
            f"{path}:{line}: {how} names a digest no row of {CI_IMAGES} holds, so nothing copies "
            f"it into the package. Add its row to that file, and {MIRROR_WORKFLOW} publishes it."
        )


def shell_lines(text: str, first: int) -> list[tuple[int, str]]:
    """(line, command) for each logical line of a shell script: backslash continuations joined,
    a comment that starts a word dropped. `first` is the file line the text starts on."""
    out: list[tuple[int, str]] = []
    number = first
    pending = ""
    start = first
    for raw in text.splitlines():
        if not pending:
            start = number
        body = raw
        continued = False
        if body.endswith("\\") and not body.endswith("\\\\"):
            body, continued = body[:-1], True
        pending = f"{pending} {body}" if pending else body
        if not continued:
            out.append((start, pending))
            pending = ""
        number += 1
    if pending:
        out.append((start, pending))
    return [(line, strip_comment(command)) for line, command in out]


def strip_comment(command: str) -> str:
    quote = ""
    for index, character in enumerate(command):
        if quote:
            if character == quote:
                quote = ""
        elif character in "\"'":
            quote = character
        elif character == "#" and (index == 0 or command[index - 1].isspace()):
            return command[:index]
    return command


def fetches(words: list[str]) -> bool:
    """A command that fetches an image from a registry: a `docker` whose subcommand, the first
    word after it that is not an option, is `pull`, `run` or `create`. `docker buildx
    imagetools create` writes an index and `docker image inspect` reads the local daemon, so
    neither is one; what `docker buildx build` pulls is the FROM of its Dockerfile, which this
    check reads where that Dockerfile is in the reach. A global option that takes a separate
    value (`docker --context two run …`) would shift the subcommand, and no command in the reach
    has one."""
    for index, word in enumerate(words):
        if word != "docker" and not word.endswith("/docker"):
            continue
        for following in words[index + 1 :]:
            if following.startswith("-"):
                continue
            if following in FETCH_VERBS:
                return True
            break
    return False


def scan_shell(path: Path, text: str, first: int, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over shell text: every assignment that reads as a reference, and the
    image of every docker fetch."""
    commands = shell_lines(text, first)
    assigned: dict[str, str] = {}
    for _, command in commands:
        found = ASSIGNMENT.match(command.strip())
        if found:
            assigned[found.group(1)] = unquote(found.group(2))
    for line, command in commands:
        words = command.split()
        found = ASSIGNMENT.match(command.strip())
        if found and reads_as_reference(unquote(found.group(2))):
            value = unquote(found.group(2))
            note(path, locate(lines, line, value), value, f"{found.group(1)}=")
        if not fetches(words):
            continue
        literals = [
            unquote(word) for word in words if reads_as_reference(unquote(word))
        ]
        for value in literals:
            note(path, locate(lines, line, value), value, "the image this docker command fetches")
        if literals:
            continue
        names = set(VARIABLE.findall(command))
        followed = any(reads_as_reference(assigned.get(name, "")) for name in names)
        for name in sorted(names):
            if (str(path), name) in IMAGE_FROM:
                used_image_from.add((str(path), name))
                followed = True
        if not followed:
            problems.append(
                f"{path}:{line}: this docker command fetches an image this check cannot read "
                f"({one_line(command)}). Name the image, or add the variable that holds it to "
                f"IMAGE_FROM in this check, saying where its value comes from."
            )


def scan_dockerfile(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a Dockerfile: every FROM that is not an earlier stage or an ARG."""
    args: set[str] = set()
    stages: set[str] = set()
    for line, command in shell_lines("\n".join(lines), 1):
        words = command.split()
        if not words:
            continue
        verb = words[0].upper()
        if verb == "ARG" and len(words) > 1:
            args.add(words[1].partition("=")[0])
            continue
        if verb != "FROM":
            continue
        rest = [word for word in words[1:] if not word.startswith("--")]
        if not rest:
            problems.append(
                f"{path}:{line}: this FROM names no image. Write {REGISTRY}@<digest>, naming a "
                f"digest {CI_IMAGES} holds."
            )
            continue
        image = rest[0]
        if len(rest) > 2 and rest[1].upper() == "AS":
            stages.add(rest[2].lower())
        if image.lower() in stages:
            continue
        names = VARIABLE.findall(image)
        if names:
            unknown = [name for name in names if name not in args]
            if unknown:
                problems.append(
                    f"{path}:{line}: this FROM reads {one_line(image)}, whose "
                    f"{', '.join(sorted(unknown))} this Dockerfile does not declare as an ARG, "
                    f"so the image it builds on cannot be read here."
                )
            continue
        note(path, line, image, "this FROM")


def scan_source(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a Go or TypeScript source: every string literal whose value reads
    as a reference. Every spelling, not only a `const`: scripts/e2e/lib/nats-stream.test.ts
    writes its image as one argument of a `docker("run", …)` call."""
    for number, text in enumerate(lines, start=1):
        for found in STRING.finditer(text):
            value = next(group for group in found.groups() if group is not None)
            if reads_as_reference(value):
                note(path, number, value, "this string")


def scan_workflow(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a workflow or a composite action: each job's runner and service
    images, each buildx driver-opts image=, and every run: script."""
    try:
        parsed = yaml.load("\n".join(lines), Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"{path}:1: this file is not valid YAML: {one_line(error)}")
        return
    if not isinstance(parsed, dict):
        return
    holders = []
    jobs = parsed.get("jobs")
    if isinstance(jobs, dict):
        holders.extend(job for job in jobs.values() if isinstance(job, dict))
    runs = parsed.get("runs")
    if isinstance(runs, dict):
        holders.append(runs)
    for holder in holders:
        images: list[tuple[int, object, str]] = []
        container = holder.get("container")
        how = "this job's container image"
        if isinstance(container, dict):
            images.append((key_line(container, "image"), container.get("image"), how))
        elif container is not None:
            images.append((key_line(holder, "container"), container, how))
        services = holder.get("services")
        if isinstance(services, dict):
            for name, service in services.items():
                how = f"the {name} service's image"
                if isinstance(service, dict):
                    images.append((key_line(service, "image"), service.get("image"), how))
                else:
                    images.append((key_line(services, name), service, how))
        for line, image, how in images:
            if not isinstance(image, str) or "${{" in image:
                problems.append(
                    f"{path}:{line}: {how} is {one_line(image)}, which this check cannot read. "
                    f"Write {REGISTRY}@<digest>, naming a digest {CI_IMAGES} holds."
                )
                continue
            note(path, line, image, how)
        for step in holder.get("steps", []) or []:
            if not isinstance(step, dict):
                continue
            with_ = step.get("with")
            if isinstance(with_, dict):
                options = with_.get("driver-opts")
                values = options if isinstance(options, list) else str(options or "").splitlines()
                for option in values:
                    option = str(option).strip()
                    if option.startswith("image="):
                        value = option[len("image=") :]
                        note(
                            path,
                            locate(lines, key_line(with_, "driver-opts"), value),
                            value,
                            "this buildx driver's image",
                        )
            script = step.get("run")
            if isinstance(script, str):
                scan_shell(path, script, key_line(step, "run"), lines)


for path in reach:
    text = read_text(path)
    if text is None:
        problems.append(f"{path}:1: this file is in CI's reach and is not readable as text")
        continue
    lines = text.splitlines()
    if path.suffix in {".yaml", ".yml"}:
        scan_workflow(path, lines)
    elif path.name == "Dockerfile" or path.name.endswith(".Dockerfile"):
        scan_dockerfile(path, lines)
    elif path.suffix in {".go", ".ts"}:
        scan_source(path, lines)
    elif path.suffix == ".sh":
        scan_shell(path, text, 1, lines)
    else:
        problems.append(
            f"{path}:1: CI's reach names this file, and this check reads no references out of a "
            f"{path.suffix or 'suffixless'} file. Teach it how, or drop the file from REACH."
        )

for path, line, text, how in references:
    judge(path, line, text, how)

for key, why in sorted(IMAGE_FROM.items()):
    if key not in used_image_from:
        problems.append(
            f"{key[0]}:1: IMAGE_FROM names ${key[1]} here, and no docker command in that file "
            f"fetches it any more ({one_line(why)}). Delete the entry."
        )

# ---------------------------------------------------------------- the mirror.gcr.io sweep
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


swept = 0
naming_mirror: set[str] = set()
for path in walk():
    name = str(path)
    if name in SELF:
        continue
    text = read_text(path)
    if text is None:
        continue
    swept += 1
    if MIRROR not in text:
        continue
    naming_mirror.add(name)
    line = locate(text.splitlines(), 1, MIRROR)
    if name in reach_names:
        problems.append(
            f"{name}:{line}: a CI job runs this file and it names {MIRROR}, the registry CI "
            f"fetched from before. Name {REGISTRY}@<digest> instead, from a row of {CI_IMAGES}."
        )
    elif name not in EXCEPTIONS:
        problems.append(
            f"{name}:{line}: this file names {MIRROR} and is in neither CI's reach nor the "
            f"exception list. Repoint it at a row of {CI_IMAGES}, or add it to EXCEPTIONS in "
            f"this check under `{NO_CI_JOB}` or `{FETCHES_NOTHING}`."
        )

# An entry admits mirror.gcr.io in one file. An entry naming a file the tree does not hold
# admits nothing, so it is left alone; an entry on a file that no longer names mirror.gcr.io is
# a ground nothing holds any more, and fails here.
for name, ground in sorted(EXCEPTIONS.items()):
    if name in reach_names:
        problems.append(
            f"{name}:1: this file is in CI's reach and in the exception list, which admits "
            f"{MIRROR} in it. Delete one of the two entries."
        )
    elif Path(name).is_file() and name not in naming_mirror:
        problems.append(
            f"{name}:1: the exception list admits {MIRROR} here on the ground that {ground}, and "
            f"the file does not name {MIRROR} any more. Delete its line from EXCEPTIONS."
        )

# ---------------------------------------------------------------- rule 7: the package
def manifest_status(digests: list[str]) -> dict[str, str]:
    """The HTTP status of an anonymous manifest HEAD, per digest. CI_IMAGES_MANIFEST_HEAD stands
    in for the read, so a test drives this rule without the network."""
    command = os.environ.get("CI_IMAGES_MANIFEST_HEAD", "")
    if command:
        out = {}
        for digest in digests:
            done = subprocess.run(
                [command, digest], capture_output=True, text=True, check=False
            )
            out[digest] = done.stdout.strip().split()[0] if done.stdout.strip() else "no status"
        return out
    try:
        with urllib.request.urlopen(
            f"https://ghcr.io/token?scope=repository:{REPOSITORY}:pull&service=ghcr.io",
            timeout=30,
        ) as answer:
            token = json.load(answer)["token"]
    except Exception as error:  # noqa: BLE001 - every failure to read the package is a refusal
        problems.append(
            f"{CI_IMAGES}:1: ghcr.io would not issue a pull token for {REPOSITORY} "
            f"({type(error).__name__}: {one_line(error)}), so this check cannot tell which rows "
            f"the package holds."
        )
        return {}
    out = {}
    for digest in digests:
        request = urllib.request.Request(
            f"https://ghcr.io/v2/{REPOSITORY}/manifests/{digest}", method="HEAD"
        )
        request.add_header("Authorization", f"Bearer {token}")
        request.add_header("Accept", ACCEPT)
        try:
            with urllib.request.urlopen(request, timeout=30) as answer:
                out[digest] = str(answer.status)
        except urllib.error.HTTPError as error:
            out[digest] = str(error.code)
        except Exception as error:  # noqa: BLE001
            out[digest] = f"{type(error).__name__}: {one_line(error)}"
    return out


held = 0
if readable:
    statuses = manifest_status([row[3] for row in readable])
    for line, upstream, tag, digest in readable:
        status = statuses.get(digest)
        if status == "200":
            held += 1
        elif status is not None:
            problems.append(
                f"{CI_IMAGES}:{line}: {REGISTRY}@{digest} ({tag}, the copy of "
                f"{one_line(upstream)}) is not in the package: a manifest HEAD answered "
                f"{status}. {MIRROR_WORKFLOW} publishes a row when {CI_IMAGES} changes, so merge "
                f"this row, or run that workflow, before a job pins the digest."
            )

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} problem(s) between {CI_IMAGES} and the images CI fetches",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(
    f"{CI_IMAGES} holds {len(readable)} row(s), {held} of them in the package; "
    f"{len(references)} reference(s) in {len(reach)} file(s) of CI's reach name one of those "
    f"rows; of {swept} text file(s) swept, the {len(naming_mirror)} that name {MIRROR} are all "
    f"in the exception list"
)
PY
