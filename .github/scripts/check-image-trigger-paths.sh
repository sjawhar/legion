#!/usr/bin/env bash
# An image workflow that filters its triggers by path publishes nothing for a change to an input
# its filter leaves out, and no pin can then name that commit. So every trigger that builds an
# image must cover every file the build reads: each context source a `COPY`, `ADD` or
# `RUN --mount=type=bind` names, the Dockerfile, the `.dockerignore` that shapes the context, and
# the workflow file that runs the build.
#
# An image build is a `docker/build-push-action` step; its `file` and `context` name the
# Dockerfile and the context. A trigger that builds it is, in its workflow and in every workflow
# that calls that one through `workflow_call`, a `push`, `pull_request`, `pull_request_target` or
# `merge_group` event. An event with no `paths` covers everything; `paths-ignore` covers whatever
# it does not name. A calling job whose `if:` tests `needs.<job>.outputs.<name> == 'true'` for
# several outputs joined by `||` runs only when one of them is set, so an input is covered only if
# the caller's event covers it AND one of those `dorny/paths-filter` filters does.
#
# A directory source is covered when every file under it that the context keeps (the
# `.dockerignore` drops the rest) matches, so a new file outside a partial filter fails here on
# the pull request that adds it. Fails, naming the trigger, the input and an uncovered file, on:
#   1. an input a trigger does not cover,
#   2. a source this check cannot resolve — a variable, a heredoc, a path that matches nothing —
#      since reading zero files from it would pass while checking nothing,
#   3. a gate it cannot evaluate — a calling job's `if:` in any other shape, an output that is
#      not a `dorny/paths-filter` filter, a `predicate-quantifier` other than `some`, a GitHub
#      path pattern using `?`, `+` or `\` — since misreading one could pass wrongly,
#   4. no image build found at all, since that checks nothing.
#
# Files are discovered by walking the tree, not by asking git, so an unsnapshotted change cannot
# read green locally.
#
# Run from anywhere: .github/scripts/check-image-trigger-paths.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import json
import posixpath
import re
import shlex
import sys
from pathlib import Path

import yaml

SKIP_DIRS = {".git", ".jj", "node_modules", ".venv", "vendor"}
WORKFLOWS = Path(".github/workflows")
PATH_EVENTS = ("push", "pull_request", "pull_request_target", "merge_group")
NEEDS_OUTPUT = re.compile(r"needs\.([\w-]+)\.outputs\.([\w-]+)")
STEP_OUTPUT = re.compile(r"steps\.([\w-]+)\.outputs\.([\w-]+)")
EQUALS_TRUE = re.compile(r"needs\.([\w-]+)\.outputs\.([\w-]+)\s*==\s*'true'")

problems: list[str] = []


def walk() -> list[str]:
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
                found.append(entry.as_posix())
    return found


FILES = walk()


def glob_regex(pattern: str) -> re.Pattern:
    """A path glob as a regex over a repository-relative path, with picomatch's meaning — the
    matcher dorny/paths-filter runs (https://github.com/micromatch/picomatch#globbing-features):
    `**` spans directories, `*` and `?` stay inside one. GitHub's own `paths` agree except for
    `?`, `+` and `\\`, which event_filters refuses; a .dockerignore line reads the same."""
    out, index = "", 0
    while index < len(pattern):
        if pattern.startswith("**/", index):
            out += "(?:.*/)?"
            index += 3
        elif pattern.startswith("**", index):
            out += ".*"
            index += 2
        elif pattern[index] == "*":
            out += "[^/]*"
            index += 1
        elif pattern[index] == "?":
            out += "[^/]"
            index += 1
        elif pattern[index] == "[" and "]" in pattern[index + 1 :]:
            end = pattern.index("]", index + 1)
            out += pattern[index : end + 1]
            index = end + 1
        else:
            out += re.escape(pattern[index])
            index += 1
    return re.compile(out)


class Filter:
    """An ordered pattern list where the last match wins and `!` negates, as GitHub reads
    `paths`. `everything` is an event with no path filter."""

    def __init__(self, patterns: list[str] | None, ignore: bool = False):
        self.everything = patterns is None
        self.ignore = ignore
        self.rules = [
            (p.startswith("!"), glob_regex(p.removeprefix("!"))) for p in (patterns or [])
        ]

    def covers(self, path: str) -> bool:
        if self.everything:
            return True
        matched = False
        for negated, regex in self.rules:
            if regex.fullmatch(path):
                matched = not negated
        return not matched if self.ignore else matched


class AnyOf:
    """Covers what any one of its filters covers: a job gated on `a == 'true' || b == 'true'`."""

    def __init__(self, filters: list[Filter]):
        self.filters = filters

    def covers(self, path: str) -> bool:
        return any(f.covers(path) for f in self.filters)



def dockerignore_filter(path: str | None):
    """True for a context path the .dockerignore drops: a pattern matches the path or a
    directory above it, and a later `!` line takes it back."""
    if path is None:
        return lambda _: False
    rules = []
    for line in Path(path).read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        negated = line.startswith("!")
        pattern = posixpath.normpath(line.removeprefix("!").lstrip("/"))
        rules.append((negated, glob_regex(pattern)))

    def ignored(relative: str) -> bool:
        parts = relative.split("/")
        prefixes = ["/".join(parts[: n + 1]) for n in range(len(parts))]
        dropped = False
        for negated, regex in rules:
            if any(regex.fullmatch(prefix) for prefix in prefixes):
                dropped = not negated
        return dropped

    return ignored


def joined_lines(text: str):
    """(line number, instruction) with Dockerfile continuations joined and comments dropped."""
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


class Unreadable(ValueError):
    """An instruction whose context sources this check cannot name."""


def copy_sources(instruction: str) -> list[str]:
    """Context sources of one COPY or ADD; none for `--from` (a stage or an image) or a URL."""
    arguments = instruction.split(maxsplit=1)[1] if " " in instruction else ""
    if arguments.startswith("["):
        try:
            entries = json.loads(arguments)
        except json.JSONDecodeError as error:
            raise Unreadable(f"malformed JSON-array form: {error.msg}") from error
        flags = []
    else:
        try:
            words = shlex.split(arguments)
        except ValueError as error:
            raise Unreadable(f"unparseable: {error}") from error
        flags = [word for word in words if word.startswith("--")]
        entries = [word for word in words if not word.startswith("--")]
    if any(flag.startswith("--from=") for flag in flags):
        return []
    if not isinstance(entries, list) or len(entries) < 2:
        raise Unreadable("no source and destination")
    sources = []
    for source in entries[:-1]:
        if "://" in source or source.startswith("git@"):
            continue
        if source.startswith("<<"):
            raise Unreadable("a heredoc source, which this check cannot read")
        sources.append(source)
    return sources


def mount_sources(instruction: str) -> list[str]:
    """Context sources of a RUN's bind mounts: a bind mount without `from=` reads the context,
    all of it when it names no source."""
    sources = []
    for word in shlex.split(instruction.split(maxsplit=1)[1] if " " in instruction else ""):
        if not word.startswith("--mount="):
            continue
        options = dict(
            item.split("=", 1) if "=" in item else (item, "")
            for item in word[len("--mount=") :].split(",")
        )
        if options.get("type", "bind") == "bind" and "from" not in options:
            sources.append(options.get("source", options.get("src", ".")))
    return sources


def context_files(context: str, source: str, ignored) -> list[str]:
    """Repository paths of the files a context source brings in, after the .dockerignore."""
    if "$" in source:
        raise Unreadable(f"{source} names a variable, which this check cannot resolve")
    relative = posixpath.normpath(source.lstrip("/"))
    root = "" if context == "." else context.rstrip("/") + "/"

    def keep(path: str) -> bool:
        return path.startswith(root) and not ignored(path[len(root) :])

    if relative == ".":
        found = [path for path in FILES if keep(path)]
    else:
        regex = glob_regex(root + relative)
        found = [
            path
            for path in FILES
            if keep(path)
            and (regex.fullmatch(path) or any(regex.fullmatch(p) for p in parents(path)))
        ]
    if not found:
        raise Unreadable(f"{source} matches no file in the context")
    return found


def parents(path: str) -> list[str]:
    parts = path.split("/")[:-1]
    return ["/".join(parts[: n + 1]) for n in range(len(parts))]


def load(path: Path):
    try:
        return yaml.safe_load(path.read_text())
    except yaml.YAMLError as error:
        problems.append(f"::error file={path}::{path} is not valid YAML: {' '.join(str(error).split())}")
        return None


def events(document: dict) -> dict:
    """The workflow's `on:` as a mapping; YAML 1.1 reads a bare `on` key as True."""
    triggers = document.get("on", document.get(True))
    if isinstance(triggers, str):
        return {triggers: None}
    if isinstance(triggers, list):
        return {name: None for name in triggers}
    return triggers if isinstance(triggers, dict) else {}


def event_filters(workflow: Path, document: dict) -> list[tuple[str, Filter]]:
    found = []
    for name, spec in events(document).items():
        if name not in PATH_EVENTS:
            continue
        spec = spec if isinstance(spec, dict) else {}
        key = next((k for k in ("paths", "paths-ignore") if k in spec), None)
        if key is None:
            found.append((f"on.{name}", Filter(None)))
            continue
        # GitHub's filter patterns give `?` and `+` regex meanings (zero or one, one or more of
        # the preceding character) and read `\` as an escape
        # (https://docs.github.com/en/actions/writing-workflows/workflow-syntax-for-github-actions#filter-pattern-cheat-sheet).
        # glob_regex reads patterns as picomatch does, so it refuses those rather than guess.
        unread = [p for p in spec[key] if any(char in str(p) for char in "?+\\")]
        if unread:
            problems.append(
                f"::error file={workflow}::{workflow} on.{name}.{key} has {', '.join(unread)}: "
                f"this check does not read GitHub's ?, + or \\ in a path filter"
            )
            continue
        found.append((f"on.{name}.{key}", Filter(spec[key], ignore=key == "paths-ignore")))
    return found


def split_top(expression: str, operator: str) -> list[str]:
    """`expression` split at each `operator` outside parentheses and quoted strings."""
    parts, depth, quoted, start, index = [], 0, False, 0, 0
    while index < len(expression):
        char = expression[index]
        if char == "'":
            quoted = not quoted
        elif not quoted and char == "(":
            depth += 1
        elif not quoted and char == ")":
            depth -= 1
        elif not quoted and depth == 0 and expression.startswith(operator, index):
            parts.append(expression[start:index].strip())
            index += len(operator)
            start = index
            continue
        index += 1
    parts.append(expression[start:].strip())
    return parts


def unwrap(expression: str) -> str:
    """`expression` without the `${{ }}` and the parentheses that enclose all of it."""
    expression = expression.strip()
    if expression.startswith("${{") and expression.endswith("}}"):
        expression = expression[3:-2].strip()
    while expression.startswith("(") and expression.endswith(")"):
        inner = expression[1:-1]
        # The first parenthesis encloses everything only if the inner text never closes more
        # than it opens.
        depth, quoted = 0, False
        for char in inner:
            if char == "'":
                quoted = not quoted
            elif not quoted:
                depth += {"(": 1, ")": -1}.get(char, 0)
                if depth < 0:
                    return expression
        expression = inner.strip()
    return expression


def gate_outputs(condition: str) -> list[tuple[str, str]]:
    """(job, output) for each paths-filter output a calling job's `if:` is gated on. Read only in
    the one shape whose meaning is "runs when any of these filters matched": every output tested
    as `needs.<job>.outputs.<name> == 'true'`, those tests joined by `||` in a single clause, and
    that clause joined to the rest of the condition by `&&`. Other `||` terms in the clause can
    only widen when the job runs, so they are allowed. Anything else — an output in two `&&`
    clauses, a negation, a comparison with another value — is refused as Unreadable."""
    conjuncts = [unwrap(part) for part in split_top(unwrap(condition), "&&")]
    clauses = [part for part in conjuncts if NEEDS_OUTPUT.search(part)]
    if not clauses:
        return []
    if len(clauses) > 1:
        raise Unreadable(
            "tests needs.*.outputs in more than one &&-joined clause, so no single filter's "
            "match is enough for the job to run"
        )
    outputs = []
    for term in split_top(clauses[0], "||"):
        term = unwrap(term)
        tested = EQUALS_TRUE.fullmatch(term)
        if tested:
            outputs.append(tested.groups())
        elif NEEDS_OUTPUT.search(term):
            raise Unreadable(
                f"tests an output as `{' '.join(term.split())}`, not as "
                f"`needs.<job>.outputs.<name> == 'true'` joined by ||"
            )
    return outputs


def gating_filters(document: dict, job_name: str, job: dict) -> list[tuple[str, Filter]]:
    """The dorny/paths-filter filters a job's `if:` is gated on. A gate this check cannot
    evaluate is reported, never ignored, since ignoring it would treat a gated call as one that
    runs on every change."""
    try:
        outputs = gate_outputs(str(job.get("if", "")))
    except Unreadable as error:
        problems.append(f"::error::jobs.{job_name} calls an image workflow, and its if: {error}")
        return []
    jobs = document.get("jobs") or {}
    found = []
    for needed, output in outputs:
        upstream = jobs.get(needed) or {}
        expression = str((upstream.get("outputs") or {}).get(output, ""))
        reference = STEP_OUTPUT.search(expression)
        step = None
        if reference:
            step = next(
                (
                    s
                    for s in upstream.get("steps") or []
                    if s.get("id") == reference.group(1)
                    and "dorny/paths-filter" in str(s.get("uses", ""))
                ),
                None,
            )
        if step is None:
            problems.append(
                f"::error::a job calling an image workflow is gated on needs.{needed}.outputs."
                f"{output}, which is not a dorny/paths-filter filter this check can read"
            )
            continue
        options = step.get("with") or {}
        # `every` makes a filter match only a change that every one of its patterns matches
        # (https://github.com/dorny/paths-filter#advanced-options); only the default, `some`,
        # means "any pattern matches".
        quantifier = options.get("predicate-quantifier", "some")
        if quantifier != "some":
            problems.append(
                f"::error::jobs.{needed} runs dorny/paths-filter with predicate-quantifier: "
                f"{quantifier}, which this check cannot evaluate; it reads only the default, some"
            )
            continue
        name = reference.group(2)
        filters = yaml.safe_load(str(options.get("filters", ""))) or {}
        patterns = filters.get(name)
        if not isinstance(patterns, list) or not all(isinstance(p, str) for p in patterns):
            problems.append(
                f"::error::jobs.{needed} filter {name} is not a list of globs, which this "
                f"check cannot read"
            )
            continue
        found.append((f"jobs.{needed} filter {name}", Filter(patterns)))
    return found


documents = {
    path: load(path)
    for path in sorted(WORKFLOWS.glob("*.y*ml"))
    if path.suffix in {".yml", ".yaml"}
}
documents = {path: doc for path, doc in documents.items() if isinstance(doc, dict)}


def triggers_for(workflow: Path) -> list[tuple[str, list]]:
    """(label, filters that must all cover an input) for every trigger that runs `workflow`."""
    document = documents[workflow]
    found = [(f"{workflow} {label}", [f]) for label, f in event_filters(workflow, document)]
    if "workflow_call" not in events(document):
        return found
    for caller, caller_document in documents.items():
        for job_name, job in (caller_document.get("jobs") or {}).items():
            if not isinstance(job, dict) or job.get("uses") != f"./{workflow}":
                continue
            gates = gating_filters(caller_document, job_name, job)
            for label, caller_filter in event_filters(caller, caller_document):
                if gates:
                    names = " or ".join(name for name, _ in gates)
                    either = AnyOf([gate for _, gate in gates])
                    found.append(
                        (f"{caller} {label} + jobs.{job_name}.if ({names})", [caller_filter, either])
                    )
                else:
                    found.append((f"{caller} {label} (jobs.{job_name})", [caller_filter]))
    return found


builds = []
for workflow, document in documents.items():
    for job in (document.get("jobs") or {}).values():
        if not isinstance(job, dict):
            continue
        for step in job.get("steps") or []:
            if isinstance(step, dict) and "docker/build-push-action" in str(step.get("uses", "")):
                options = step.get("with") or {}
                context = posixpath.normpath(str(options.get("context", ".")))
                dockerfile = str(options.get("file", posixpath.join(context, "Dockerfile")))
                builds.append((workflow, context, posixpath.normpath(dockerfile)))

if not builds:
    problems.append(
        "::error::no workflow runs docker/build-push-action: this check covered 0 image builds, "
        "so it proves nothing"
    )

for workflow, context, dockerfile in builds:
    if not Path(dockerfile).is_file():
        problems.append(f"::error file={workflow}::{workflow} builds {dockerfile}, which is missing")
        continue
    specific = f"{dockerfile}.dockerignore"
    general = posixpath.normpath(posixpath.join(context, ".dockerignore"))
    ignore_file = next((p for p in (specific, general) if Path(p).is_file()), None)
    ignored = dockerignore_filter(ignore_file)

    # (what the build reads, the files it stands for)
    inputs: list[tuple[str, list[str]]] = [
        (dockerfile, [dockerfile]),
        (str(workflow), [str(workflow)]),
    ]
    if ignore_file:
        inputs.append((ignore_file, [ignore_file]))
    for line, instruction in joined_lines(Path(dockerfile).read_text()):
        command = instruction.split(maxsplit=1)[0].upper() if instruction else ""
        try:
            if command in {"COPY", "ADD"}:
                sources = copy_sources(instruction)
            elif command == "RUN":
                sources = mount_sources(instruction)
            else:
                continue
            for source in sources:
                inputs.append(
                    (f"{source} ({dockerfile}:{line})", context_files(context, source, ignored))
                )
        except Unreadable as error:
            problems.append(f"::error file={dockerfile},line={line}::{dockerfile}:{line}: {error}")

    triggers = triggers_for(workflow)
    for label, filters in triggers:
        for name, files in inputs:
            uncovered = [path for path in files if not all(f.covers(path) for f in filters)]
            if uncovered:
                more = f" and {len(uncovered) - 1} more are" if len(uncovered) > 1 else " is"
                problems.append(
                    f"::error file={label.split()[0]}::{label} does not cover {name}: "
                    f"{uncovered[0]}{more} built into the image, so a commit touching only "
                    f"that builds no image"
                )
        print(
            f"{dockerfile}: checked {len(inputs)} inputs "
            f"({sum(len(files) for _, files in inputs)} files) against {label}"
        )
    if not triggers:
        print(f"{dockerfile}: {workflow} has no path-triggered event; nothing to check")

problems = list(dict.fromkeys(problems))
for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(f"::error::{len(problems)} image trigger problem(s)", file=sys.stderr)
    raise SystemExit(1)
print(f"every image trigger covers every input its build reads ({len(builds)} image build(s))")
PY
