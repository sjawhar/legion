#!/usr/bin/env bash
# Bun is a toolchain, and CI must run the one this repository locks. The pin lives in
# `.bun-version`; the two Dockerfile `ARG BUN_VERSION` defaults must equal it (nothing passes
# `--build-arg`, so those defaults are what every image build resolves), and this check fails
# until all three agree. Raising Bun is therefore three edits, made together.
#
# What this gate is for is accidental drift: an unpinned setup-bun step, a floating Docker tag, a
# Bun install that never reads `.bun-version`. It does not try to stop someone who is deliberately
# overriding the version, since that person can edit this file too. One such shape is known and
# left uncovered on purpose: `container.options: --env INPUT_BUN-VERSION=…`, which the runner
# splices into its `docker create` argv, so a step's nested setup-bun reads it. Review catches
# that, not this script.
#
# `oven-sh/setup-bun` cannot enforce its own pin: a missing version file is a `warning()`, and
# `bun-version` and `bun-download-url` both win over the file when a step sets them. So every
# job installs Bun through `.github/actions/setup-bun`, and this check fails, in code order:
#
#   1. `.bun-version` missing, empty, or not an exact `X.Y.Z` — `1` or `1.3` is a floating tag,
#      which is the original defect wearing a pinned-looking file,
#   2. a workflow or action file that cannot be parsed as YAML, or a Dockerfile or manifest that
#      cannot be decoded as text,
#   3. a `run:` step that installs or runs an unpinned Bun — the install script, a global
#      npm/pnpm/yarn install, a `mise use`, a release archive, or a `docker run oven/bun:<tag>`,
#   4. a `run:` step that writes an `INPUT_*` variable into `$GITHUB_ENV`, which every later step
#      inherits — by redirect, heredoc, `tee -a`, or the `{name}<<{delimiter}` form,
#   5. `oven-sh/setup-bun` (matched case-insensitively) named anywhere but
#      `.github/actions/setup-bun/action.yml`,
#   6. an `env:` naming an `INPUT_*` variable at workflow, job, `container:` or step level — an
#      action reads its inputs from `INPUT_*`, either way,
#   7. a job `container:` or `services:` image on `oven/bun` whose tag is not the pinned one,
#   8. that wrapper missing, or not holding exactly one setup-bun step whose only input is
#      `bun-version-file: .bun-version` — it is checked for what it is, not only for what other
#      files are not,
#   9. a `FROM oven/bun` (case-insensitive) whose tag does not come from `${BUN_VERSION}`, an
#      `ARG BUN_VERSION` default that disagrees with the pin, or a Dockerfile `RUN` (backslash
#      continuations joined) that installs Bun,
#  10. a tool-manager manifest (any mise file, `.tool-versions`) naming bun.
#
# Files are discovered by walking the tree, not by asking git, so an unsnapshotted change cannot
# read green locally. Workflows and actions are parsed as YAML, never grepped. Every problem is
# printed with its file and line; the only non-zero exit is a named problem.
#
# Run from anywhere: .github/scripts/check-bun-version.sh
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
    """A YAML mapping that remembers where it was written. The lines are attributes, not keys:
    a document is never searched for a key the loader invented, so a job or an input named
    `__line__` is judged like any other."""

    line: int
    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    # Also each key's own line, so a problem in a plain scalar value — `container: oven/bun:1`,
    # which carries no mapping of its own — is reported where the reader has to edit.
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


def line_of(value: object) -> int:
    return getattr(value, "line", 0)


def key_line(mapping: object, name: str) -> int:
    return getattr(mapping, "lines", {}).get(name, line_of(mapping))


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

PIN_FILE = ".bun-version"
SETUP_ACTION = Path(".github/actions/setup-bun/action.yml")
SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
MISE_FILES = {
    "mise.toml",
    ".mise.toml",
    "mise.local.toml",
    ".mise.local.toml",
    ".tool-versions",
}
MISE_PATHS = {
    Path(".config/mise/config.toml"),
    Path(".mise/config.toml"),
}
BUN_INSTALLERS = (
    "bun.sh/install",
    "bun.com/install",
    "install.bun.sh",
    "releases/download/bun-v",
)
PACKAGE_MANAGER_BUN = re.compile(
    r"\b(?:npm|pnpm|yarn)\s[^\n|&;]*(?:\s-g\b|--global\b)[^\n|&;]*\bbun\b"
    r"|\b(?:npm|pnpm|yarn)\s[^\n|&;]*\bbun\b[^\n|&;]*(?:\s-g\b|--global\b)"
)
MISE_BUN = re.compile(r"\bmise\b[^\n|&;]*\b(?:use|install)\b[^\n|&;]*\bbun\b")
DOCKER_RUN_BUN = re.compile(r"\bdocker\s[^\n|&;]*\brun\b[^\n|&;]*\b(oven/bun(?::\S+)?)")
INPUT_ASSIGNMENT = re.compile(r"INPUT_[A-Za-z0-9_-]+(?:=|<<)")
MISE_ENTRY = re.compile(r'^"?(?:tools\.)?bun"?\s*[=:]|^bun\s|^\[tools\.bun\]')
VERSION = re.compile(r"\d+\.\d+\.\d+")

problems = []
wrapper_setup_steps: list[tuple[int, dict]] = []
wrapper_total_steps = 0


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: a block scalar in a workflow would
    otherwise reach the Actions log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def bun_tag_is_pinned(image: str, expected: str) -> bool:
    """An `oven/bun` reference is pinned when its tag is the pinned release, suffix and all;
    an untagged reference is `latest`."""
    return image.partition(":")[2].split("-")[0] == expected


def installs_bun(script: str, pinned: str = "") -> bool:
    """A command that fetches Bun itself, or runs a Bun image that is not the pinned one: none
    of these ways reads the pinned file."""
    if any(marker in script for marker in BUN_INSTALLERS):
        return True
    if PACKAGE_MANAGER_BUN.search(script) or MISE_BUN.search(script):
        return True
    ran = DOCKER_RUN_BUN.search(script)
    return bool(ran and not bun_tag_is_pinned(ran.group(1), pinned))


def is_input_key(key: object) -> bool:
    """`getInput` reads `INPUT_<NAME>`, so a name with that prefix is an action input under
    another name. Matched case-insensitively: a spelling no runner reads is still someone
    writing one by hand."""
    return str(key).upper().startswith("INPUT_")


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
workflows = [
    path
    for path in files
    if (path.parent == Path(".github/workflows") and path.suffix in {".yml", ".yaml"})
    or path.name in {"action.yml", "action.yaml"}
]
dockerfiles = [
    path
    for path in files
    if path.name == "Dockerfile"
    or path.name.startswith("Dockerfile.")
    or path.name.endswith(".Dockerfile")
]
manifests = [path for path in files if path.name in MISE_FILES or path in MISE_PATHS]

def fatal(message: str) -> None:
    """The pin itself is unreadable: nothing downstream can be judged, so stop here."""
    print(f"::error file={PIN_FILE}::{message}", file=sys.stderr)
    raise SystemExit(1)


pin_path = Path(PIN_FILE)
if not pin_path.is_file():
    fatal(f"{PIN_FILE} is missing: every Bun install reads it")
pinned = pin_path.read_text().strip()
if not pinned:
    fatal(f"{PIN_FILE} is empty: every Bun install reads it")
if not VERSION.fullmatch(pinned):
    fatal(
        f"{PIN_FILE} is {one_line(pinned)}, which is a range, not a release: setup-bun resolves "
        f"it to the newest match and oven/bun:{one_line(pinned)} is a floating tag. Write an "
        f"exact X.Y.Z."
    )


def step_holders(document: object):
    """(is_step, mapping) for every mapping a workflow or composite action hangs behaviour on,
    outermost first: the document, each job, each job's `container`, and every step. One walk, so
    a rule added to it reaches the same places as every other. The tag is the walk's own
    knowledge of where it is: a job carrying `uses:` is a reusable-workflow call, not a step, and
    a step is written under `steps:` whether or not it carries a key this gate reads."""
    if not isinstance(document, dict):
        return
    yield False, document
    jobs = document.get("jobs")
    holders = (
        [job for job in jobs.values() if isinstance(job, dict)] if isinstance(jobs, dict) else []
    )
    runs = document.get("runs")
    if isinstance(runs, dict):
        holders.append(runs)
    for holder in holders:
        yield False, holder
        container = holder.get("container")
        if isinstance(container, dict):
            yield False, container
        for step in holder.get("steps", []) or []:
            if isinstance(step, dict):
                yield True, step


def steps(document: object):
    """Every step of a workflow (`jobs.<id>.steps`) or a composite action (`runs.steps`)."""
    for is_step, holder in step_holders(document):
        if is_step:
            yield holder


def input_env_keys(document: object):
    """(line, keys) for every `env:` naming an INPUT_* variable, at any level that sets one:
    an action reads its inputs from INPUT_*, and a workflow-, job- or container-level entry is
    inherited by the steps under it. `getInput` keeps a hyphen, so the real spelling is
    `INPUT_BUN-VERSION`; nothing legitimate writes one by hand."""
    for _, holder in step_holders(document):
        environment = holder.get("env")
        if not isinstance(environment, dict):
            continue
        named = sorted(key for key in environment if is_input_key(key))
        if named:
            yield key_line(holder, "env"), named


def job_images(document: object):
    """(line, image) for every job runner and service image: a job whose runner is a Bun image
    installs nothing, so no step rule would fire on it."""
    if not isinstance(document, dict):
        return
    jobs = document.get("jobs")
    if not isinstance(jobs, dict):
        return
    for job in jobs.values():
        if not isinstance(job, dict):
            continue
        # `container:` may be the image string itself; a mapping carries its own line, and the
        # string form is reported at the line its key sits on, found by name.
        holders = [("container", job.get("container"))]
        services = job.get("services")
        if isinstance(services, dict):
            holders.extend(services.items())
        for name, holder in holders:
            if isinstance(holder, str):
                yield key_line(job, name), holder
            elif isinstance(holder, dict) and isinstance(holder.get("image"), str):
                yield line_of(holder), holder["image"]


def read_text(path: Path) -> str | None:
    """None when the file cannot be decoded — named like an unparseable workflow, not raised.
    UTF-8 explicitly: the gate must not depend on the runner's or a developer's locale."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as error:
        problems.append(f"::error file={path}::{path} is not readable as text: {one_line(error)}")
        return None


for workflow in workflows:
    content = read_text(workflow)
    if content is None:
        continue
    try:
        document = yaml.load(content, Loader=LineLoader)
    except yaml.YAMLError as error:
        # One named line, never a traceback: this gate runs on every pull request, and a file
        # it cannot parse is that file's problem to report, not a crash for everyone.
        problems.append(f"::error file={workflow}::{workflow} is not valid YAML: {one_line(error)}")
        continue
    if workflow == SETUP_ACTION:
        wrapper_total_steps = sum(1 for _ in steps(document))
    for step in steps(document):
        line = line_of(step)
        script = step.get("run")
        if isinstance(script, str) and installs_bun(script, pinned):
            problems.append(
                f"{workflow}:{line}: this step installs Bun itself, which does not read "
                f"{PIN_FILE} — use ./.github/actions/setup-bun"
            )
        if isinstance(script, str) and "GITHUB_ENV" in script and INPUT_ASSIGNMENT.search(script):
            problems.append(
                f"{workflow}:{line}: this step exports an INPUT_* variable into $GITHUB_ENV, "
                f"which every step after it inherits — an action input, written sideways"
            )
        uses = step.get("uses")
        # Mis-cased is still the same action: GitHub serves `Oven-Sh/setup-bun@v2`.
        if not isinstance(uses, str) or "oven-sh/setup-bun" not in uses.lower():
            continue
        if workflow == SETUP_ACTION:
            wrapper_setup_steps.append((line, step))
        else:
            problems.append(
                f"{workflow}:{line}: {one_line(uses)} is called directly — use "
                f"./.github/actions/setup-bun, which is the one place the pin is read"
            )

    for line, keys in input_env_keys(document):
        problems.append(
            f"{workflow}:{line}: sets {', '.join(keys)} — an action reads its inputs from "
            f"INPUT_*, so this is an action input written as an environment variable"
        )
    for line, image in job_images(document):
        if "oven/bun" in image and not bun_tag_is_pinned(image, pinned):
            problems.append(
                f"{workflow}:{line}: {one_line(image)} runs an unpinned Bun — use "
                f"oven/bun:{pinned}"
            )

# The wrapper is judged for what it is, not only for what other files are not: a wrapper that is
# deleted, repointed at a fork, duplicated, or given a second input would otherwise leave every
# other rule satisfied while CI installed something else.
if not SETUP_ACTION.is_file():
    problems.append(
        f"::error file={SETUP_ACTION}::{SETUP_ACTION} is missing: it is the one place Bun is "
        f"installed, and every job uses it"
    )
elif wrapper_total_steps != 1:
    problems.append(
        f"{SETUP_ACTION}:1: holds {wrapper_total_steps} steps, not one — a second step can write "
        f"INPUT_* into $GITHUB_ENV or prepend $GITHUB_PATH, which later steps then take"
    )
elif len(wrapper_setup_steps) != 1:
    problems.append(
        f"{SETUP_ACTION}:1: holds {len(wrapper_setup_steps)} oven-sh/setup-bun steps, not "
        f"one — the wrapper must install Bun exactly once, from {PIN_FILE}"
    )
else:
    line, step = wrapper_setup_steps[0]
    uses = str(step.get("uses"))
    options = step.get("with") if isinstance(step.get("with"), dict) else {}
    extra = sorted(key for key in options if key != "bun-version-file")
    if not uses.lower().startswith("oven-sh/setup-bun@"):
        problems.append(
            f"{SETUP_ACTION}:{line}: the wrapper runs {one_line(uses)}, not oven-sh/setup-bun@<ref>"
        )
    if options.get("bun-version-file") != PIN_FILE:
        problems.append(
            f"{SETUP_ACTION}:{line}: the wrapper must pass bun-version-file: {PIN_FILE}, not "
            f"{one_line(options.get('bun-version-file'))}"
        )
    if extra:
        problems.append(
            f"{SETUP_ACTION}:{line}: the wrapper passes {', '.join(extra)} beside the version "
            f"file, and setup-bun prefers those over it — remove them"
        )
    environment = step.get("env") if isinstance(step.get("env"), dict) else {}
    # An INPUT_* entry is reported once, by the rule that owns that spelling everywhere.
    named = sorted(key for key in environment if not is_input_key(key))
    if named:
        problems.append(
            f"{SETUP_ACTION}:{line}: the wrapper sets {', '.join(named)} on the step — an action "
            f"reads its inputs from INPUT_*, so an env entry is an input by another name"
        )

def joined_lines(text: str):
    """(line number, logical line) with Dockerfile backslash continuations joined, so a RUN
    split across lines — which this repository's own Dockerfiles write — is judged whole."""
    pending, start = "", 0
    for number, raw in enumerate(text.splitlines(), start=1):
        stripped = raw.strip()
        if not pending:
            start = number
        # Docker removes a comment line inside a continued instruction and carries on, and a
        # comment's own trailing backslash continues nothing. Either way the line is dropped
        # without flushing what is pending.
        if stripped.startswith("#"):
            continue
        if stripped.endswith("\\"):
            pending += stripped[:-1] + " "
            continue
        yield start, (pending + stripped).strip()
        pending = ""
    if pending:
        yield start, pending.strip()


for dockerfile in dockerfiles:
    content = read_text(dockerfile)
    if content is None:
        continue
    for number, stripped in joined_lines(content):
        upper = stripped.upper()
        if stripped.startswith("ARG BUN_VERSION="):
            default = stripped[len("ARG BUN_VERSION=") :].strip().strip('"')
            if default != pinned:
                problems.append(
                    f"{dockerfile}:{number}: ARG BUN_VERSION={one_line(default)}, but {PIN_FILE} "
                    f"locks {pinned} — raise both together"
                )
        elif upper.startswith("FROM ") and "oven/bun" in stripped.lower():
            image = stripped.split()[1]
            if "${BUN_VERSION}" not in image.partition(":")[2]:
                problems.append(
                    f"{dockerfile}:{number}: {one_line(image)} hardcodes its Bun tag — use "
                    f"oven/bun:${{BUN_VERSION}} and ARG BUN_VERSION={pinned}"
                )
        elif upper.startswith("RUN ") and installs_bun(stripped, pinned):
            problems.append(
                f"{dockerfile}:{number}: this RUN installs Bun itself, which does not read "
                f"{PIN_FILE} — build FROM oven/bun:${{BUN_VERSION}} instead"
            )

for manifest in manifests:
    content = read_text(manifest)
    if content is None:
        continue
    for number, text in enumerate(content.splitlines(), start=1):
        stripped = text.strip()
        if stripped.startswith("#") or "bun" not in stripped:
            continue
        if MISE_ENTRY.match(stripped):
            problems.append(
                f"{manifest}:{number}: {one_line(stripped)} — a tool manager installing Bun does "
                f"not read {PIN_FILE}; install Bun with ./.github/actions/setup-bun"
            )

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} place(s) do not install the Bun {PIN_FILE} locks ({pinned})",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(f"every Bun install reads {PIN_FILE} ({pinned})")
PY
