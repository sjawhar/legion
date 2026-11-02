#!/usr/bin/env bash
# A GitHub-hosted runner shares one anonymous pull quota with every other runner on its address,
# so a CI job that fetches an image from a third-party registry fails for a reason the change
# under test did not cause. Every image a CI job of this repository fetches is therefore one copy
# in one package this repository publishes, ghcr.io/sjawhar/legion/ci, and every reference names
# that copy by digest. .github/ci-images.yaml holds one row per distinct upstream image, and
# .github/scripts/mirror-ci-images.sh copies the rows the package does not hold yet with
# `crane copy`, which keeps the manifest digest. ci.yaml's ci-images job runs that script and
# then the package mode of this check, in that order. That job is the one copy path, and it runs
# on a pull request, on a merge group and on a push to main. This check holds the two sides to
# each other.
#
# THE TWO MODES. This check takes one mode argument, and runs that half alone. A call that names
# no mode, or that names more than one, fails naming both.
#
#   tree     every refusal that reads the files of this tree alone. It makes no network read, so
#            ci.yaml's lint job runs it, and lint runs on every non-draft run. A change that
#            names a third-party registry again can touch any file of the tree, and an image
#            filter names few of those files, so this half must read every change.
#   package  the one refusal that reads the package, over the network. ci.yaml's ci-images job
#            runs it, after the copy of that same job. The ci_images filter selects that job and
#            names every file that decides what the copy and this read do: .github/ci-images.yaml,
#            the two scripts, .github/scripts/ci_images.py (the reference grammar both read a row
#            with), .github/actions/install-crane and ci.yaml itself. The package can
#            also change under a run that touches none of them, and a re-run reads it again.
#
# Both modes read .github/ci-images.yaml, because the package mode asks ghcr.io about each row's
# digest. The package mode leaves the shape of a row to the tree mode, which runs on every change.
#
# It fails, naming the file, the line and the remedy, on:
#
# IN TREE MODE:
#
#   1. a row this check cannot read: a row that is not a mapping, a missing or empty `upstream`,
#      `tag` or `digest`, or a digest that is not `sha256:` and 64 hex characters. A tag is
#      required because an untagged manifest is a package version nothing names, and a retention
#      policy may delete a digest a job pins.
#   2. two rows with the same digest, two rows with the same tag, or a tag that is not the one
#      its own row's upstream and digest give it. The row is what says which image a digest is
#      — which upstream it copies, and which version that upstream carries — so one digest
#      names one row and one row names one tag. The digest in the tag is what makes one tag
#      name one digest: a row that moves to a new digest takes a new tag with it, the digest it
#      leaves keeps the tag it had, and no copy moves a tag off a digest a consumer pins. The
#      tag a row must carry is the upstream repository's last path element, then the upstream
#      tag when the row names one, then the first eight hex characters of the digest. This
#      check reads the whole string, not the eight characters alone.
#   3. a version this repository pins elsewhere that no row carries: no `oven/bun` row at the
#      version `.bun-version` locks, with or without the `-slim` suffix, or no `library/golang`
#      row at `ARG GO_VERSION` of packages/daemon/docker/worker.Dockerfile. A row for another
#      version is admitted, so raising Bun or Go is two commits: the new version's row first,
#      which ci.yaml's ci-images job copies, then the pin and every FROM. One commit holding
#      both would let a consumer job pull the new digest while that same run's copy is still
#      landing it. Rule 7 below is what keeps a Go FROM on the row the ARG names. This rule and
#      rule 7 read a row's repository with Docker Hub's implicit `library/` written out, so
#      `docker.io/golang:1.27.1-alpine` and `docker.io/library/golang:1.27.1-alpine` are one
#      repository to both of them.
#   4. a reference in CI's reach that names a registry other than ghcr.io/sjawhar/legion/ci,
#   5. a reference in CI's reach that is not pinned by digest (`@sha256:` and 64 hex characters):
#      a tag is a name the registry can move, so only a digest names the bytes a job ran,
#   6. a reference in CI's reach whose digest no row of .github/ci-images.yaml holds,
#   7. a reference in CI's reach whose row copies library/golang at a version other than
#      ARG GO_VERSION of packages/daemon/docker/worker.Dockerfile. Rule 3 asks for one row at
#      that version and admits a row beside it, so without this rule a FROM could name the
#      other row: that stage would build with the other Go, and the worker's toolchain stage
#      downloads the ARG's. A bump's second commit moves the ARG and every FROM together, so it
#      passes. .github/scripts/check-bun-version.sh holds a Bun FROM to .bun-version this way.
#
# IN PACKAGE MODE:
#
#   8. a row whose digest the package does not hold: the manifest is read with an anonymous
#      `HEAD` against ghcr.io. Its message names .github/ci-images.yaml and says that ci.yaml's
#      ci-images job copies a row before this mode reads the package, so the usual remedy is to
#      merge the row and let that job run.
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
#   * a `FROM` argument, continuations joined, with each `ARG`'s default substituted into it.
#     Docker builds `FROM` the default when no `--build-arg` overrides it, so the default is the
#     image the build fetches. A `FROM` naming an earlier stage of the same Dockerfile is not a
#     reference. A `FROM` on a variable the Dockerfile declares with no default anywhere is not
#     one either: whoever runs the build passes that value, so no image can be read here;
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
# the tree does not hold admits nothing, and is left alone. The sweep walks the tree with
# workflow_paths.walk, which skips .git, .jj, node_modules, .venv and vendor. This sweep skips
# two more things itself: .legion (Legion's per-issue run data, which a retro removes) and this
# check's own two files, whose text names the registry to find it.
#
# Files are discovered by walking the tree, not by asking git, so an unsnapshotted change cannot
# read green locally.
#
# CHECK 8 AND THE NETWORK. The package mode's read of the package is one anonymous token request
# (`GET https://ghcr.io/token?scope=repository:sjawhar/legion/ci:pull&service=ghcr.io`) and one
# `HEAD https://ghcr.io/v2/sjawhar/legion/ci/manifests/<digest>` per row, with an `Accept` naming
# the four index and manifest media types. CI_IMAGES_MANIFEST_HEAD is the seam that read hangs
# on: when it is set, this check runs `"$CI_IMAGES_MANIFEST_HEAD" <digest>` once per row instead
# and reads the HTTP status code the command prints on stdout (200 = the package holds the
# digest, 404 = it does not). check-ci-images.test.sh sets it to a small script of its own, and
# mirror-ci-images.test.sh drives this rule with it over the repository's own rows, so no case of
# either test reaches the network.
#
# Run from anywhere: .github/scripts/check-ci-images.sh tree
#                    .github/scripts/check-ci-images.sh package
# CI runs the tree mode in ci.yaml's lint job, and the package mode in ci.yaml's ci-images job,
# after that job's copy. Its tests run in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The mode is required, and names what it runs. A call with no mode, or with more than one word,
# fails here: a caller that says neither cannot be given one half of this check by default.
case "$#:${1:-}" in
  1:tree | 1:package) ;;
  *)
    echo "::error file=.github/scripts/check-ci-images.sh::this check takes one mode: \`tree\` reads the files of this tree and makes no network read, and \`package\` reads ghcr.io/sjawhar/legion/ci. Run \`.github/scripts/check-ci-images.sh tree\` or \`.github/scripts/check-ci-images.sh package\`." >&2
    exit 1
    ;;
esac
export CI_IMAGES_MODE="$1"

# The sweep's walk is workflow_paths.py's and the reference grammar is ci_images.py's, both
# beside this script in the repository it checks, which PYTHONPATH names.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

import yaml

from ci_images import DIGEST, split_reference, upstream_parts


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
# The half this run checks. The wrapper above admits `tree` and `package` alone, and the rules
# below ask for the package half by name.
PACKAGE = "package"
MODE = os.environ["CI_IMAGES_MODE"]

REGISTRY = "ghcr.io/sjawhar/legion/ci"
REPOSITORY = "sjawhar/legion/ci"
CI_IMAGES = Path(".github/ci-images.yaml")
MIRROR_SCRIPT = ".github/scripts/mirror-ci-images.sh"
MIRROR_JOB = "ci.yaml's ci-images job"
BUN_PIN = Path(".bun-version")
WORKER_DOCKERFILE = Path("packages/daemon/docker/worker.Dockerfile")
# The two upstream repositories a pin of this repository names: rule 3 asks for a row at each
# pin, and rule 7 holds every reference on a GOLANG_REPOSITORY row to ARG GO_VERSION. Both are
# written as upstream_parts gives a repository, with Docker Hub's implicit `library/` spelled
# out.
BUN_REPOSITORY = "oven/bun"
GOLANG_REPOSITORY = "library/golang"
MIRROR = "mirror.gcr.io"
# The directories workflow_paths.walk skips (.git, .jj, node_modules, .venv, vendor) are every
# directory this sweep skips but one, and this set holds that one.
SWEEP_SKIP_DIRS = {".legion"}
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
# walkthrough recorder, the corpus copy a developer restores on a devbox, and documentation.
# `a CI job runs it, and it fetches nothing`: a script a CI job runs whose mirror.gcr.io text no
# job ever hands to a registry — dev-postgres.test.sh, which the envoy-go job runs, installs a
# fake `docker` on PATH.
EXCEPTIONS = {
    "AGENTS.md": NO_CI_JOB,
    "docs/kubernetes.md": NO_CI_JOB,
    "docs/plans/2026-10-14-dispatch-gcp-plan.md": NO_CI_JOB,
    "docs/plans/2026-10-14-envoy-infra-plan.md": NO_CI_JOB,
    "docs/solutions/github/an-image-pull-that-fails-in-ci-is-this-repositorys-own-package-read-the-failure-do-not-re-run-pr2512.md": NO_CI_JOB,
    "docs/site/media/walkthroughs/legion-operator/record-casts.sh": NO_CI_JOB,
    "docs/solutions/testing/a-docker-run-line-is-proven-from-a-pod-with-no-container-runtime-as-a-two-container-job-whose-client-ships-in-the-image-pr2498.md": NO_CI_JOB,
    "packages/envoy/deploy/compose/dispatch.acceptance.compose.yml": NO_CI_JOB,
    "packages/envoy/deploy/compose/single-node.compose.yml": NO_CI_JOB,
    "packages/envoy/scripts/corpus-copy.sh": NO_CI_JOB,
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
    "packages/daemon/internal/testpg/testpg.go": FETCHES_NOTHING,
    "packages/envoy/scripts/dev-postgres.sh": FETCHES_NOTHING,
    "packages/envoy/scripts/dev-postgres.test.sh": FETCHES_NOTHING,
}

# ---------------------------------------------------------------- reference grammar
# The grammar of a reference, and the two readers of one, are ci_images.py's, imported above:
# mirror-ci-images.sh reads a row's upstream with the same two.
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
            f"{MIRROR_SCRIPT} copies from it"
        )
        continue
    if not isinstance(tag, str) or not tag.strip():
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'tag')}: the row for {one_line(upstream)} has no tag. "
            f"An untagged manifest is a package version nothing names, and a retention policy "
            f"may delete a digest a job pins. Give it one: the upstream repository's last path "
            f"element, then the upstream tag when this row names one, then the first eight hex "
            f"characters of the digest."
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


def verdict(what: str, summary: str) -> None:
    """Prints the problems this mode found, and its own summary line. Each mode ends here."""
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(f"::error::{len(problems)} problem(s) {what}", file=sys.stderr)
        raise SystemExit(1)
    print(summary)
    raise SystemExit(0)


# ---------------------------------------------------------------- rule 8: the package
def manifest_status(digests: list[str]) -> dict[str, str]:
    """The HTTP status of an anonymous manifest HEAD, per digest. CI_IMAGES_MANIFEST_HEAD stands
    in for the read, so a test drives this check without the network."""
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


if MODE == PACKAGE:
    # Rule 1, a row's own shape, is the tree mode's, and that mode reads every change. A row
    # this mode cannot read is one it asks ghcr.io nothing about, so this mode names the rows
    # the package lacks and nothing else.
    problems = []
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
                    f"{status}. {MIRROR_JOB} copies a row of {CI_IMAGES} before this mode reads "
                    f"the package, so merge this row, or re-run that job, before a job pins the "
                    f"digest."
                )
    verdict(
        f"between {CI_IMAGES} and the package",
        f"{CI_IMAGES} holds {len(readable)} row(s), {held} of them in the package",
    )


def tag_rule(upstream: str, digest: str) -> str:
    """The tag the rule gives a row: the upstream repository's last path element, then the
    upstream tag when the row names one, then the first eight hex characters of the digest."""
    _, path, carried, _ = upstream_parts(upstream)
    short = digest.partition(":")[2][:8]
    parts = [part for part in (path.rpartition("/")[2], carried, short) if part]
    return "-".join(parts)


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
    wanted = tag_rule(upstream, digest)
    if tag != wanted:
        problems.append(
            f"{CI_IMAGES}:{line}: tag {tag} is not {wanted}, the tag this row's upstream and "
            f"digest give it. The last eight characters are what keeps one tag on one digest. A "
            f"tag without them is a tag another digest of the same image can take, and the copy "
            f"that takes it moves the tag off the digest consumers pin. The rest names the image "
            f"a reader of the package sees. Name this row {wanted}."
        )


# Rule 3: a version this repository pins elsewhere that no row carries. One row carrying the pin
# is enough, and a row for another version is admitted, because a bump lands its row in the
# commit before the pin: ci.yaml's ci-images job copies the row, and the commit that moves the
# pin and the FROMs then pulls a digest the package already holds. Nothing loosens by admitting
# the extra row. Rule 6 refuses a FROM whose digest no row holds, rule 7 refuses a FROM on a
# library/golang row of any Go but the pinned one, and .github/scripts/check-bun-version.sh
# refuses a FROM on a row of any Bun but the pinned one.
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

carried_by: dict[str, list[str]] = {BUN_REPOSITORY: [], GOLANG_REPOSITORY: []}
for _, upstream, _, _ in readable:
    _, repository, carried, _ = upstream_parts(upstream)
    if repository in carried_by:
        carried_by[repository].append(carried)
if not any(carried in {bun_pin, f"{bun_pin}-slim"} for carried in carried_by[BUN_REPOSITORY]):
    problems.append(
        f"{CI_IMAGES}:{key_line(document, 'images')}: no row copies oven/bun:{bun_pin}, and "
        f"{BUN_PIN} locks {bun_pin}. Add that row (the `-slim` suffix is a row of its own) and "
        f"name its digest, and {MIRROR_JOB} publishes it. Raising Bun is two commits: this row "
        f"first, then {BUN_PIN}, the ARG and every FROM."
    )
if not any(carried.partition("-")[0] == go_pin for carried in carried_by[GOLANG_REPOSITORY]):
    problems.append(
        f"{CI_IMAGES}:{key_line(document, 'images')}: no row copies library/golang at the "
        f"version {WORKER_DOCKERFILE} pins ARG GO_VERSION={go_pin}. Add the row "
        f"golang:{go_pin}-alpine and name its digest, and {MIRROR_JOB} publishes it. Raising Go "
        f"is two commits: this row first, then the ARG and every FROM."
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
    """Rules 4, 5, 6 and 7: one reference, read against the rows."""
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
            f"a copy in {REGISTRY}. Add a row for it to {CI_IMAGES}, which {MIRROR_JOB} "
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
    row = by_digest.get(digest)
    if row is None:
        problems.append(
            f"{path}:{line}: {how} names a digest no row of {CI_IMAGES} holds, so nothing copies "
            f"it into the package. Add its row to that file, and {MIRROR_JOB} publishes it."
        )
        return
    # Rule 7. Rule 3 admits a library/golang row beside the one the ARG names, which is what a
    # bump's first commit lands, so the row a Go FROM names is read here against that ARG.
    _, row_repository, carried, _ = upstream_parts(row[1])
    if row_repository == GOLANG_REPOSITORY and carried.partition("-")[0] != go_pin:
        version = one_line(carried) if carried else "no version this check can read"
        problems.append(
            f"{path}:{line}: {how} names the row for {one_line(row[1])} on line {row[0]} of "
            f"{CI_IMAGES}, which copies Go {version}, and {WORKER_DOCKERFILE} pins "
            f"ARG GO_VERSION={go_pin}. A stage that builds with another Go is not the Go the "
            f"worker's toolchain stage downloads, so name the row at {go_pin} here. Raising Go "
            f"is two commits: that row first, then the ARG and every FROM together."
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


def expand(text: str, defaults: dict[str, str]) -> str:
    """`text` with each ARG default substituted for its variable. The substitution repeats,
    because a default can name another ARG. The bound stops a default that names itself."""
    for _ in range(10):
        grown = VARIABLE.sub(lambda found: defaults.get(found.group(1), found.group(0)), text)
        if grown == text:
            break
        text = grown
    return text


def scan_dockerfile(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a Dockerfile: every FROM, read with the ARG defaults substituted
    into it. A FROM naming an earlier stage of the same Dockerfile is not a reference, and
    neither is a FROM on a variable the Dockerfile declares with no default anywhere."""
    defaults: dict[str, str] = {}
    declared: set[str] = set()
    stages: set[str] = set()
    for line, command in shell_lines("\n".join(lines), 1):
        words = command.split()
        if not words:
            continue
        verb = words[0].upper()
        if verb == "ARG" and len(words) > 1:
            name, equals, value = words[1].partition("=")
            declared.add(name)
            # A stage re-declares an ARG by name to carry the global default into that stage, so
            # a re-declaration with no `=` keeps the default the first declaration gave.
            if equals:
                defaults[name] = unquote(value)
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
        if VARIABLE.search(image):
            resolved = expand(image, defaults)
            left = sorted(set(VARIABLE.findall(resolved)))
            unknown = [name for name in left if name not in declared]
            if unknown:
                problems.append(
                    f"{path}:{line}: this FROM reads {one_line(image)}, whose "
                    f"{', '.join(unknown)} this Dockerfile does not declare as an ARG, "
                    f"so the image it builds on cannot be read here."
                )
                continue
            if left:
                # Each variable left is one the Dockerfile declares with no default anywhere.
                # Whoever runs the build passes it with --build-arg, so this FROM names no image
                # to read (packages/envoy/infra/host/heldportwrapper/Dockerfile is that case).
                continue
            if resolved.lower() in stages:
                continue
            note(path, line, resolved, f"this FROM, which the ARG defaults make {resolved},")
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
# The walk is workflow_paths.walk, the one check-image-trigger-paths.sh reads the same tree
# with. It is imported here rather than beside this check's other imports, because the package
# mode ends above and this sweep is the one half that walks the tree.
from workflow_paths import walk  # noqa: E402

swept = 0
naming_mirror: set[str] = set()
for name in walk():
    path = Path(name)
    if SWEEP_SKIP_DIRS.intersection(path.parts[:-1]) or name in SELF:
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

verdict(
    f"between {CI_IMAGES} and the images CI fetches",
    f"{CI_IMAGES} holds {len(readable)} row(s); {len(references)} reference(s) in {len(reach)} "
    f"file(s) of CI's reach name one of those rows; of {swept} text file(s) swept, the "
    f"{len(naming_mirror)} that name {MIRROR} are all in the exception list",
)
PY
