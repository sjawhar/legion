#!/usr/bin/env bash
# An image workflow that filters its triggers by path publishes nothing for a change to an input
# its filter leaves out, and no pin can then name that commit. So every trigger that builds an
# image must cover every file the build reads: each context source a `COPY`, `ADD` or
# `RUN --mount=type=bind` names, the Dockerfile, the `.dockerignore` that shapes the context, and
# the workflow file that runs the build.
#
# An image build is a `docker/build-push-action` step, whose `file` and `context` name the
# Dockerfile and the context, or a `run:` step invoking `docker build` / `docker buildx build`,
# whose `--file` and one positional argument name them, relative to the step's
# `working-directory`. A trigger that builds it is, in its workflow and in every workflow
# that calls that one through `workflow_call`, a `push`, `pull_request`, `pull_request_target` or
# `merge_group` event. An event with no `paths` covers everything; `paths-ignore` covers whatever
# it does not name. A job whose `if:` tests `needs.<job>.outputs.<name> == 'true'` for several
# outputs joined by `||` runs only when one of them is set, so an input is covered only if the
# event covers it AND one of those `dorny/paths-filter` filters does. That holds for the build
# step's own `if:`, for the job that builds, for a job that calls a workflow which builds, and
# for every job either of those transitively `needs:` — GitHub skips a job whose needed job was
# skipped — so separate gates AND while the filters inside one `if:` OR.
#
# A directory source is covered when every file under it that the context keeps (the
# `.dockerignore` drops the rest) matches, so a new file outside a partial filter fails here on
# the pull request that adds it. Fails, naming the trigger, the input and an uncovered file, on:
#   1. an input a trigger does not cover,
#   2. a source this check cannot resolve — a variable, a heredoc, a path that matches nothing —
#      since reading zero files from it would pass while checking nothing,
#   2a. a `docker … build` in a run step it cannot read — a subshell, a wrapper, `docker compose
#      build`, an unknown flag, a `working-directory` holding a variable — since skipping one
#      leaves an image whose inputs no trigger owes, which is what this check exists to catch,
#   3. a gate it cannot evaluate — an `if:` in any other shape, one narrowed by a top-level
#      `&&` term naming a `github.` context, an output that is not a `dorny/paths-filter`
#      filter, a `predicate-quantifier` other than `some`, a GitHub path pattern using `?`, `+`
#      or `\` — since misreading one could pass wrongly,
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
# The only `if:` terms that do not narrow which CHANGES reach a job, so the only ones that can
# sit beside the filters clause without being refused.
NEUTRAL = re.compile(
    r"always\(\)|success\(\)|!\s*cancelled\(\)|needs\.[\w-]+\.result(\s*[=!]=\s*'[^']*')?"
)

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


# A run step that builds an image is refused rather than parsed. Three rounds of review each
# found another invocation shape the parser could not see — a comment swallowing the command, a
# command substitution, `eval`, `docker buildx bake`, `docker compose up --build` — and a shape
# it cannot see is an image whose inputs no trigger owes. The build form this check CAN read is
# a `docker/build-push-action` step, so a run step matching any of these words fails and says
# so. This catches the words, not every build: a script file, a composite action, or a build
# behind a variable still passes unseen.
RUN_STEP_BUILD = re.compile(
    r"docker\s+build(?![\w-])"
    r"|docker\s+buildx\s+(build|bake)(?![\w-])"
    r"|docker(\s+compose|-compose)\b[^\n]*--build"
    r"|\bbuildah\b"
    r"|\bkaniko\b|gcr\.io/kaniko-project",
)


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
    """(job, output) for each paths-filter output an `if:` is gated on.

    An allowlist, because unknown must mean refused and never ungated: a condition this check
    does not model can narrow the gate on anything at all — `if: false`, `vars.X == 'true'`,
    `&& github.event_name != 'merge_group'` — and reading it as ungated asserts coverage the
    job does not have. Each top-level `&&` conjunct is therefore either THE filters clause
    (`needs.<job>.outputs.<name> == 'true'` tests joined by `||`, at most one such conjunct;
    other `||` terms there only widen, so they are allowed), or made entirely of terms that do
    not narrow by path — `always()`, `success()`, `!cancelled()`, `needs.<job>.result` — or it
    is refused, quoting the term."""
    if not condition.strip():
        return []
    conjuncts = [unwrap(part) for part in split_top(unwrap(condition), "&&")]
    clauses = [part for part in conjuncts if NEEDS_OUTPUT.search(part)]
    for part in conjuncts:
        if part in clauses:
            continue
        if all(NEUTRAL.fullmatch(unwrap(term).strip()) for term in split_top(part, "||")):
            continue
        raise Unreadable(
            f"is narrowed by `{' '.join(part.split())}`, which this check cannot "
            f"evaluate against a path filter"
        )
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


def gating_filters(document: dict, describe: str, holder: dict) -> list[tuple[str, Filter]]:
    """The dorny/paths-filter filters an `if:` is gated on. The holder is a job that builds an
    image, a job that calls a workflow which does, a job either of those needs, or the build
    step itself. A gate this check cannot evaluate is reported, never ignored, since ignoring it
    would treat a gated build as one that runs on every change."""
    try:
        outputs = gate_outputs(str(holder.get("if", "")))
    except Unreadable as error:
        problems.append(f"::error::{describe} builds or calls an image, and its if: {error}")
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
                f"::error::{describe} builds or calls an image and is gated on "
                f"needs.{needed}.outputs.{output}, which is not a dorny/paths-filter filter "
                f"this check can read"
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


def inherited_gates(document: dict, job_name: str) -> list[tuple[str, list[tuple[str, Filter]]]]:
    """(label, filters) for every gate that decides whether `job_name` runs: its own `if:` and
    that of each job it transitively `needs:`, since GitHub skips a job whose needed job was
    skipped. A job whose own `if:` says `always()` or `!cancelled()` runs anyway, so the walk
    does not descend past it."""
    gates, seen, queue = [], set(), [job_name]
    while queue:
        name = queue.pop()
        if name in seen:
            continue
        seen.add(name)
        job = (document.get("jobs") or {}).get(name) or {}
        found = gating_filters(document, f"jobs.{name}", job)
        if found:
            gates.append((f"jobs.{name}.if", found))
        if "always()" in str(job.get("if", "")) or "cancelled()" in str(job.get("if", "")):
            continue
        needs = job.get("needs") or []
        queue += [needs] if isinstance(needs, str) else list(needs)
    return gates


def apply_gates(triggers: list, gates: list) -> list:
    """AND each gate into every trigger. Filters inside one `if:` are ||-joined and widen, but
    separate gates must all hold."""
    for gate_label, found in gates:
        names = " or ".join(label for label, _ in found)
        either = AnyOf([f for _, f in found])
        triggers = [
            (f"{label} + {gate_label} ({names})", filters + [either]) for label, filters in triggers
        ]
    return triggers


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
            # The calling job is gated by its own `if:` and by every job it transitively needs,
            # exactly as a building job is.
            gates = inherited_gates(caller_document, job_name)
            base = [(f"{caller} {label}", [caller_filter]) for label, caller_filter in event_filters(caller, caller_document)]
            if gates:
                found += apply_gates(base, gates)
            else:
                found += [(f"{label} (jobs.{job_name})", filters) for label, filters in base]
    return found


builds = []
for workflow, document in documents.items():
    for job_name, job in (document.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        for step in job.get("steps") or []:
            if not isinstance(step, dict):
                continue
            if "docker/build-push-action" in str(step.get("uses", "")):
                options = step.get("with") or {}
                context = posixpath.normpath(str(options.get("context", ".")))
                dockerfile = str(options.get("file", posixpath.join(context, "Dockerfile")))
                builds.append((workflow, job_name, step, context, posixpath.normpath(dockerfile)))
                continue
            script = step.get("run")
            if not isinstance(script, str) or not RUN_STEP_BUILD.search(script):
                continue
            where = step.get("name") or "an unnamed run step"
            problems.append(
                f"::error file={workflow}::{workflow}: {where} builds an image in a run step; "
                f"build images with docker/build-push-action so this check can read the build"
            )

if not builds:
    problems.append(
        "::error::no workflow builds an image with docker/build-push-action: this check covered "
        "0 image builds, so it proves nothing"
    )

for workflow, job_name, step, context, dockerfile in builds:
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

    # A trigger that starts the workflow is not enough: a gate can skip the build on a change
    # the trigger let through, and the image is then not built for that change either. The
    # gates are the build step's own `if:`, its job's, and every job that job transitively
    # needs.
    document = documents[workflow]
    gates = inherited_gates(document, job_name)
    if str(step.get("if", "")):
        where = step.get("name") or "an unnamed step"
        describe = f"jobs.{job_name} step '{where}'"
        found = gating_filters(document, describe, step)
        if found:
            gates.append((f"{describe} if", found))
    triggers = apply_gates(triggers_for(workflow), gates)
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
