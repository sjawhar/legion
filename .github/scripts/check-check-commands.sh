#!/usr/bin/env bash
# A prescribed check command must reach this repository's own pinned tool, not whatever the name
# happens to answer. `bunx biome check .` with no `node_modules` installs a squatted npm package
# (`biome` 0.3.3, unrelated to `@biomejs/biome`) that checks nothing and exits 0; a bare `tsc` in a
# recipe is answered by the worker image's own `/usr/local/bin/tsc`, a compiler this repository
# does not pin; `bunx playwright` fetches whatever the registry serves rather than the
# `@playwright/test` the lockfile holds. What this gate is for is that shape exactly: a prescribed
# command that silently runs the wrong tool and passes, which is worse than one that fails.
#
# `scripts/repo-tool.sh <tool>` resolves `./node_modules/.bin/<tool>` then the root's, never PATH
# and never the registry, so it either runs the version the lockfile installed or names the
# missing tool and exits 1. The rule here is therefore "a prescribed command invokes a pinned
# check tool through `scripts/repo-tool.sh`", not "no `bunx`": the pinned check tools are `biome`,
# `tsc`, `tsserver` and `playwright`, and the gate judges commands, never prose.
#
# `vite` and `astro` are deliberately not on that list: they are build recipes rather than the
# checks this gate is about, no binary on the image shadows them, and a bare one in a `bun run`
# recipe is already fail-closed, since Bun puts `node_modules/.bin` on the recipe's PATH and
# nothing else answers the name. A `bunx vite` is still caught, by the `bunx` half of rule 1.
#
# One tokenizer serves every rule, so a command is read the same way wherever it is written: a
# command string is split into segments on `&&`, `||`, `;` and `|`; each segment's leading
# brackets, braces, quotes, a prompt's `$` and any `FOO=bar` assignments are stepped over; and the
# first word that remains is the command the shell would run. Quotes are stepped over rather than
# obeyed, so a command nested in `bash -c '… && bunx playwright …'` — how
# `docs/site/media/broker/screenshots.sh` used to call Playwright — is judged like any other. A
# segment whose first word is `bunx` is a problem whatever it names; a segment whose first word's
# basename is one of the four tools is a problem; a segment whose first word ends in
# `scripts/repo-tool.sh`, with any relative prefix, is correct whatever tool it names.
#
# The gate fails, in code order:
#
#   1. a `scripts` value in any `package.json` (parsed as JSON, never grepped) whose segments
#      invoke a pinned check tool outside the wrapper, or invoke `bunx` at all, reported at the
#      line the recipe's own key sits on,
#   2. a `run:` in any `.github/workflows/*.yml` or `*.yaml` (parsed as YAML with a line-recording
#      loader) doing the same, each line of a multi-line `run:` judged on its own and reported at
#      its own line where the loader's span gives it, otherwise at the step's,
#   3. a command line in any `*.sh` outside `.github/scripts/`, or inside a fenced code block of
#      the prescriptive markdown set — root `AGENTS.md` and `README.md`, `packages/*/AGENTS.md`
#      and `packages/*/README.md`, `docs/site/README.md`, `docs/site/media/README.md`,
#      `skills/**/*.md`, `packages/claude-envoy/legion/skills/**/*.md`, `docs/solutions/**/*.md`,
#      `docs/kubernetes.md`, `docs/deployment.md` — doing the same. Prose and inline code are
#      never judged: a note may name `bunx biome` in a sentence or in backticks, because only a
#      line inside a fenced block is a command someone runs,
#   4. an exemption that gives no reason. A fenced block whose immediately preceding non-blank
#      line is `<!-- check-check-commands: allow <reason> -->` is not judged, and neither is a
#      line ending `# check-check-commands: allow <reason>`; the reason is what makes the
#      exemption reviewable, so an empty one is itself a problem.
#
# Excluded, each for its own reason:
#
#   * `docs/plans/**`, `docs/superpowers/**` and `.superpowers/**` — human-authored history of
#     what was done at the time, not commands anyone is told to run now,
#   * `.github/scripts/**` — these guards quote the very command names they forbid, as data
#     (`check-release-credentials.sh`'s `"bunx"` in a Python set, and this file's own rules),
#   * Dockerfiles, which are not judged at all: an image's own build is where a tool legitimately
#     comes from PATH, which is why `packages/daemon/docker/worker.Dockerfile`'s
#     `ln -s bun /usr/local/bin/bunx` — the line that creates the `bunx` every agent then misuses
#     — passes,
#   * `.git`, `.jj`, `node_modules`, `.venv` and `vendor`, which hold nothing this repository
#     prescribes.
#
# Files are discovered by walking the tree, not by asking jj or git, so an unsnapshotted change
# cannot read green locally. Every problem is printed with its file and line; the only non-zero
# exit is a named problem, and a file that cannot be parsed or decoded is a named problem too.
#
# Run from anywhere: .github/scripts/check-check-commands.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import re
import sys
from pathlib import Path

import yaml


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records where each mapping and each value was written, so a problem names
    the line the reader has to edit."""


class Mapping(dict):
    """A YAML mapping that remembers its lines. They are attributes, not keys: a document is never
    searched for a key the loader invented, so a job or a step named `__line__` is judged like any
    other."""

    line: int
    lines: dict[str, int]
    spans: dict[str, tuple[int, int]]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    pairs = [(key, value) for key, value in node.value if hasattr(key, "value")]
    mapping.lines = {key.value: key.start_mark.line + 1 for key, _ in pairs}
    # The value's own span, so each line of a block scalar is reported where it is written rather
    # than where its key is.
    mapping.spans = {
        key.value: (value.start_mark.line + 1, value.end_mark.line + 1) for key, value in pairs
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

WRAPPER = "scripts/repo-tool.sh"
MARKER = "check-check-commands: allow"
PINNED_TOOLS = ("biome", "tsc", "tsserver", "playwright")
SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
EXCLUDED_DIRS = ("docs/plans", "docs/superpowers", ".superpowers", ".github/scripts")
GUIDES = (
    "AGENTS.md",
    "README.md",
    "packages/*/AGENTS.md",
    "packages/*/README.md",
    "docs/site/README.md",
    "docs/site/media/README.md",
    "skills/**/*.md",
    "packages/claude-envoy/legion/skills/**/*.md",
    "docs/solutions/**/*.md",
    "docs/kubernetes.md",
    "docs/deployment.md",
)
SEGMENT = re.compile(r"&&|\|\||[;|]")
ENV_PREFIX = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
# Stepped over before the first word is read: a subshell's or group's opener, a quote, a prompt's
# `$`, and a `${` that a variable path starts with.
LEADING = " \t({}$\"'`"
FENCE = re.compile(r"^\s*(?P<fence>`{3,}|~{3,})(?P<info>.*)$")
ALLOW_COMMENT = re.compile(rf"^<!--\s*{MARKER}(?P<reason>.*?)-->$")
ALLOW_TRAILING = re.compile(rf"#\s*{MARKER}(?P<reason>[^#]*)$")

problems = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: a block scalar in a workflow would
    otherwise reach the Actions log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def first_word(segment: str) -> str:
    """The command a segment would run: brackets, quotes, a prompt's `$` and any `FOO=bar`
    assignments are stepped over. Quotes are stepped over rather than obeyed, so the inner command
    of `bash -c '… && bunx playwright …'` is read as a command of its own."""
    text = segment.strip().lstrip(LEADING)
    while True:
        words = text.split(None, 1)
        if not words:
            return ""
        word = words[0].strip("\"'`")
        if not ENV_PREFIX.match(word):
            return word
        text = (words[1] if len(words) > 1 else "").lstrip(LEADING)


def offenders(command: str):
    """Every segment of a command string that reaches a pinned check tool outside the wrapper,
    as the sentence naming what is wrong with it (the segment's first word is quoted inside it).
    One reading of what the shell would run serves a recipe, a workflow `run:` line, a shell line
    and a fenced command line alike."""
    for segment in SEGMENT.split(command):
        word = first_word(segment)
        if not word or word.endswith(WRAPPER):
            continue
        tool = word.rsplit("/", 1)[-1]
        if tool == "bunx":
            yield (
                f"runs `{one_line(word)}`, which resolves from the npm registry rather than this "
                f"repository's lockfile (`bunx biome` there is a squatted package that checks "
                f"nothing and exits 0) — run `{WRAPPER} <tool>` instead"
            )
        elif tool in PINNED_TOOLS:
            yield (
                f"runs `{one_line(word)}`, which is whatever PATH answers with (the worker image "
                f"ships its own tsc), not the {tool} this repository pins — run "
                f"`{WRAPPER} {tool}` instead"
            )


def judge(path: Path, number: int, text: str, context: str) -> None:
    """Judges one command line, honouring a trailing exemption. An exemption with no reason is
    reported instead: the reason is the whole of what makes it reviewable."""
    exemption = ALLOW_TRAILING.search(text.rstrip())
    if exemption:
        if not exemption.group("reason").strip():
            problems.append(
                f"{path}:{number}: this exemption gives no reason — write "
                f"`# {MARKER} <reason>`"
            )
        return
    for wrong in offenders(text):
        problems.append(f"{path}:{number}: {context} {wrong}")


def glob_to_regex(pattern: str) -> re.Pattern[str]:
    """`**/` spans directories, `*` stops at one: `packages/*/AGENTS.md` is a package's own guide,
    not one nested three levels under it."""
    parts = []
    for piece in re.split(r"(\*\*/|\*)", pattern):
        if piece == "**/":
            parts.append(r"(?:[^/]+/)*")
        elif piece == "*":
            parts.append(r"[^/]*")
        else:
            parts.append(re.escape(piece))
    return re.compile("".join(parts) + r"\Z")


GUIDE_PATTERNS = [glob_to_regex(pattern) for pattern in GUIDES]


def walk() -> list[Path]:
    """Every file in the tree, the directories that hold nothing prescriptive skipped. A symlinked
    directory is not descended into: it would be walked twice or leave the tree entirely."""
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


def excluded(path: Path) -> bool:
    text = path.as_posix()
    return any(text.startswith(f"{directory}/") for directory in EXCLUDED_DIRS)


def read_text(path: Path) -> str | None:
    """None when the file cannot be decoded — named like an unparseable workflow, not raised.
    UTF-8 explicitly: the gate must not depend on the runner's or a developer's locale."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as error:
        problems.append(f"::error file={path}::{path} is not readable as text: {one_line(error)}")
        return None


files = [path for path in walk() if not excluded(path)]
manifests = [path for path in files if path.name == "package.json"]
workflows = [
    path
    for path in files
    if path.parent == Path(".github/workflows") and path.suffix in {".yml", ".yaml"}
]
shell_scripts = [path for path in files if path.suffix == ".sh"]
guides = [
    path for path in files if any(pattern.match(path.as_posix()) for pattern in GUIDE_PATTERNS)
]


def recipe_line(lines: list[str], name: str) -> int:
    """The line a recipe's own key sits on, found in the file's text from the `scripts` key down:
    the parsed document carries no lines of its own, and a recipe named like a dependency must be
    reported where it is written, not where the name first appears."""
    needle = f'"{name}"'
    start = next(
        (number for number, text in enumerate(lines) if '"scripts"' in text),
        0,
    )
    for number in range(start, len(lines)):
        if needle in lines[number]:
            return number + 1
    return start + 1


for manifest in manifests:
    content = read_text(manifest)
    if content is None:
        continue
    try:
        document = json.loads(content)
    except json.JSONDecodeError as error:
        # One named line, never a traceback: this gate runs on every pull request, and a file it
        # cannot parse is that file's problem to report, not a crash for everyone.
        problems.append(f"::error file={manifest}::{manifest} is not valid JSON: {one_line(error)}")
        continue
    scripts = document.get("scripts") if isinstance(document, dict) else None
    if not isinstance(scripts, dict):
        continue
    lines = content.splitlines()
    for name, command in scripts.items():
        if isinstance(command, str):
            judge(manifest, recipe_line(lines, name), command, f'the "{name}" recipe')


def run_lines(raw: list[str], span: tuple[int, int], value: str):
    """(line number, command line) for a `run:` value. Each line of a block scalar is matched to
    its own line inside the span the loader gives the value, a cursor forward so two identical
    commands are reported separately; a folded or quoted scalar, whose text is not in the file
    line for line, falls back to the line the value starts on."""
    start, end = span
    cursor = start
    for text in value.splitlines():
        stripped = text.strip()
        if not stripped or stripped.startswith("#"):
            continue
        number = start
        for candidate in range(cursor, min(end, len(raw)) + 1):
            if raw[candidate - 1].strip() == stripped:
                number = candidate
                cursor = candidate + 1
                break
        yield number, stripped


for workflow in workflows:
    content = read_text(workflow)
    if content is None:
        continue
    try:
        document = yaml.load(content, Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"::error file={workflow}::{workflow} is not valid YAML: {one_line(error)}")
        continue
    raw = content.splitlines()
    jobs = document.get("jobs") if isinstance(document, dict) else None
    for job in jobs.values() if isinstance(jobs, dict) else []:
        if not isinstance(job, dict):
            continue
        for step in job.get("steps") or []:
            if not isinstance(step, dict) or not isinstance(step.get("run"), str):
                continue
            step_line = getattr(step, "lines", {}).get("run", getattr(step, "line", 0))
            span = getattr(step, "spans", {}).get("run", (step_line, step_line))
            for number, command in run_lines(raw, span, step["run"]):
                judge(workflow, number, command, "this run: line")

for script in shell_scripts:
    content = read_text(script)
    if content is None:
        continue
    for number, text in enumerate(content.splitlines(), start=1):
        stripped = text.strip()
        if not stripped or stripped.startswith("#"):
            continue
        judge(script, number, stripped, "this line")

for guide in guides:
    content = read_text(guide)
    if content is None:
        continue
    fence = ""
    exempt = False
    previous, previous_number = "", 0
    for number, text in enumerate(content.splitlines(), start=1):
        match = FENCE.match(text)
        if not fence:
            if match:
                fence = match.group("fence")
                exemption = ALLOW_COMMENT.match(previous.strip())
                exempt = bool(exemption and exemption.group("reason").strip())
                if exemption and not exempt:
                    problems.append(
                        f"{guide}:{previous_number}: this exemption gives no reason — write "
                        f"`<!-- {MARKER} <reason> -->`"
                    )
            elif text.strip():
                previous, previous_number = text, number
            continue
        # A fence closes on the same character, at least as long, and carrying no info string.
        closing = match and match.group("fence")[0] == fence[0] and not match.group("info").strip()
        if closing and len(match.group("fence")) >= len(fence):
            fence = ""
            # The closing fence is the block's last non-blank line, so the marker above this
            # block cannot exempt the next one too.
            previous, previous_number = text, number
            continue
        stripped = text.strip()
        if exempt or not stripped or stripped.startswith("#"):
            continue
        judge(guide, number, stripped, "this fenced command")

for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} place(s) prescribe a check command that does not run this "
        f"repository's pinned tool through {WRAPPER}",
        file=sys.stderr,
    )
    raise SystemExit(1)
print(
    f"every prescribed biome, tsc, tsserver and playwright command runs through {WRAPPER} "
    f"({len(manifests)} manifest(s), {len(workflows)} workflow(s), {len(shell_scripts)} shell "
    f"script(s), {len(guides)} guide(s))"
)
PY
