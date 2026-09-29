#!/usr/bin/env bash
# An image workflow that filters its triggers by path publishes nothing for a change to an input
# its filter leaves out, and no pin can then name that commit. So every trigger that builds an
# image must cover every file the build reads: each context source a `COPY`, `ADD` or
# `RUN --mount=type=bind` names, the Dockerfile, the `.dockerignore` that shapes the context, and
# the workflow file that runs the build.
#
# The one build form this check READS is a `docker/build-push-action` step, whose `context` and
# `file` name the tree and the Dockerfile. Everything else is an allowlist or a net:
#
#   * `uses:` is an ALLOWLIST. A build action cannot be spotted by name — `depot/build-push-
#     action` and `mr-smithers-excellent/docker-build-push` are both real — so an action that
#     is not `docker/build-push-action` and not in NON_BUILDING_ACTIONS is refused. A local
#     `./` action is READ, never trusted for being local: a composite's `runs.steps` get these
#     same rules recursively, and a local action that is not composite is as opaque as a remote
#     one — `docker/build-push-action` is itself a JavaScript action — so `using: docker`,
#     `using: node*` and a missing `using` are all refused unless NON_BUILDING_ACTIONS names
#     it. A job-level `uses:` must be a `./.github/workflows/` call, which triggers_for traces.
#   * The build action's `with:` keys are an ALLOWLIST (BUILD_ACTION_INPUTS). A key this check
#     has not read may name another tree the build receives — `build-contexts`, `secret-files`
#     — which the trigger would then owe, so an unlisted key is refused. `cache-from` is
#     listed but also read, since `type=local,src=…` is a local tree.
#   * GATES are an ALLOWLIST: unknown means refused, never ungated. A gate resolves to
#     `dorny/paths-filter` outputs tested `== 'true'` and joined by `||`, or to terms that
#     cannot narrow by path, or it is refused. GitHub compares case-insensitively, so this
#     check does too.
#   * A build in a `run:` step is DETECTED, and EVERY detection is refused - there is no
#     pattern that can excuse one. The detector judges ONE COMMAND at a time and is drawn
#     broad: a container-CLI word followed later on the command by `build`, `bake` or `b`, the
#     other builders, and `compose up|run|create|watch` without `--no-build`. A broad detector
#     needs an escape hatch for its false positives, and that hatch is DATA, not a shape:
#     NOT_A_BUILD_STEPS lists steps by (workflow, job, step `id:`) - ids, because a step's name
#     is neither required nor unique - pinning the exact hits a reviewer saw and why they build
#     no image. A hit on a listed step is excused and printed while its hits still match; a hit
#     anywhere else, or one the entry did not describe, is refused. An entry matching no step
#     or several fails the check rather than rotting. So a build is admitted only when the
#     detector misses it (e.g. a builder it does not name, a variable-named engine, a script
#     file, text piped to a shell, a non-shell `shell:` - the paragraph above RUN_STEP_BUILD
#     is the list), or when its step is listed in NOT_A_BUILD_STEPS. Measured over 117 build
#     shapes: all 117 detected, 0 of this repository's 122 run steps matched, and the list is
#     empty - nothing is excused today.
#
# A trigger that builds an image is, in its workflow and in every workflow that calls that one
# through `workflow_call`, a `push`, `pull_request`, `pull_request_target` or `merge_group`
# event. An event with no `paths` covers everything; `paths-ignore` covers whatever it does not
# name. A job whose `if:` tests `needs.<job>.outputs.<name> == 'true'` for several outputs
# joined by `||` runs only when one of them is set, so an input is covered only if the event
# covers it AND one of those filters does. That holds for the build step's own `if:`, for the
# job that builds, for a job that calls a workflow which builds, and for every job either of
# those transitively `needs:` — GitHub skips a job whose needed job was skipped — so separate
# gates AND while the filters inside one `if:` OR. `always()` and `!cancelled()` run past a
# skipped need, but a `needs.<job>.result` the same `if:` demands re-imposes that job's gate
# unless the comparison still admits `skipped`.
#
# A directory source is covered when every file under it that the context keeps (the
# `.dockerignore` drops the rest) matches, so a new file outside a partial filter fails here on
# the pull request that adds it. Fails, naming the trigger, the input and an uncovered file, on:
#   1. an input a trigger does not cover,
#   2. a source this check cannot resolve — a variable, a heredoc, a path that matches nothing —
#      since reading zero files from it would pass while checking nothing,
#   3. anything outside an allowlist above — an unlisted action, an unlisted `with:` key, a
#      context that is not a plain repository path, or a gate in any other shape — since
#      reading one wrongly asserts coverage the build does not have; every such refusal names
#      the remedy,
#   4. a run step the net catches, since skipping one leaves an image whose inputs no trigger
#      owes,
#   5. no image build found at all, since that checks nothing.
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
# sit beside the filters clause without being refused. GitHub's comparisons are
# case-insensitive (Expressions → Operators), so these are too: `needs.X.result == 'Success'`
# must read exactly like `== 'success'`, or a capital letter walks a narrowing gate past the
# allowlist.
NEUTRAL = re.compile(
    r"always\(\)|success\(\)|!\s*cancelled\(\)|needs\.[\w-]+\.result(\s*[=!]=\s*'[A-Za-z_]+')?",
    re.I,
)
RESULT_TERM = re.compile(r"needs\.([\w-]+)\.result\s*(==|!=)\s*'([A-Za-z_]+)'", re.I)


def admits_skipped(term: str) -> bool:
    """True when a `needs.<job>.result` comparison is still satisfied by `skipped`.

    `result == 'success'` is not path-neutral: it re-imposes the named job's own gate on this
    one, which is how a build can be skipped on a change its filters cover. `!= 'failure'` and
    `== 'skipped'` do admit it, and so leave this job free of that gate."""
    match = RESULT_TERM.fullmatch(term.strip())
    if not match:
        return False
    _, operator, raw = match.groups()
    value = raw.lower()
    return value == "skipped" if operator == "==" else value != "skipped"


def required_results(condition: str) -> set[str]:
    """Jobs whose gate this `if:` re-imposes, because it demands a result that `skipped` fails.

    A conjunct is a disjunction: if any of its terms admits `skipped`, or is unconditionally
    true, the conjunct demands nothing of the jobs it names."""
    required: set[str] = set()
    if not condition.strip():
        return required
    for part in [unwrap(p) for p in split_top(unwrap(condition), "&&")]:
        terms = [unwrap(term).strip() for term in split_top(part, "||")]
        if any(admits_skipped(term) or term == "always()" for term in terms):
            continue
        required.update(match.group(1) for term in terms for match in [RESULT_TERM.fullmatch(term)] if match)
    return required


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


# A run step's build is DETECTED, and every detection is refused unless the step is named in
# NOT_A_BUILD_STEPS below. There is no pattern that can excuse one. Ten rounds of review each
# found a spelling some recogniser missed - a quoted subcommand, `${BUILD_FLAGS}`, a global
# option before `compose`, a pipe inside `$( )`, `--call build`, `podman-remote`, `docker.exe` -
# and a pattern that must be COMPLETE to be safe cannot be made safe by adding more patterns.
# The escape hatch a broad detector needs is therefore DATA: a finite list of steps, each named
# exactly and reviewed, rather than a shape anyone can spell their way into.
#
# The detector runs on ONE COMMAND at a time: a step is stripped of comments, its backslash
# continuations joined, its command substitutions taken as commands of their own, and then
# split on newlines, `;`, `&` and `|`. It is deliberately broad, and reads no flag table: a
# container-CLI word followed later on the command by `build`, `bake` or `b`, the other
# builders, and `compose up|run|create|watch` without `--no-build`, since a service carrying
# `build:` builds implicitly - as both compose files in this repository do. ANCHORED_BUILD
# recognises the shapes that are unmistakably a build; it is part of the detector, not a veto.
# `docker commit`, `docker import` and `buildah commit` are in: they always make an image, from
# a container rather than a Dockerfile.
#
# A build word counts anywhere in its command EXCEPT inside the quoted argument of `echo` or
# `printf`, whose text is not a command.
#
# What the detector cannot see, so what it admits: a build inside a SCRIPT FILE a step calls;
# TEXT PIPED TO A SHELL (`echo "docker build ." | sh`, `printf … | bash`), since an echo's
# quoted argument is blanked as text; a build ASSEMBLED FROM A VARIABLE or reached through
# `eval` where the words never appear literally; a RENAMED BINARY, an alias or a symlink whose name
# says nothing (`ctr build`, `alias d=docker`); a NESTED substitution inside a quoted echo; a
# build behind a
# TASK RUNNER (`make image`, `npm run build:docker`, `just build`, `bazel run //:push`, `nix
# build .#dockerImage`), which is not netted because that would refuse most steps in most
# repositories; a BUILDER THIS DOES NOT NAME (`packer build`, `s2i build`, `nixpacks build`,
# `werf build`, `apko build`); an ENGINE NAMED BY A VARIABLE that is not DOCKER-ish
# (`$CONTAINER_ENGINE build`); and a step whose `shell:` IS NOT ONE THIS READS AS A SHELL
# (`shell: python` runs a program whose text this check does not interpret).
#
# So, exactly: a build is admitted only when the detector misses it (e.g. the list above), or
# when its step is listed in NOT_A_BUILD_STEPS.

# Steps this check flags that build no image. A step is named by its `id:`, which GitHub
# requires to be unique within a job, because a step's NAME is neither required nor unique: two
# steps can share one, and an unnamed step has none at all, so a name-keyed entry could excuse
# a build it was never written for. An inner step of a local composite action is keyed by the
# CALLING step's id and its own, joined with `/`, which is unique in the calling job even when
# the action is used twice.
#
# Each entry also pins HITS, the exact detector matches the reviewer saw. The step is excused
# only while its current matches equal that list, so a step that later grows a real build is
# refused rather than riding its entry. An entry matching no step, or more than one, fails, as
# does one whose hits have drifted: the list cannot outlive what it described.
#
# Empty: this repository has no false positive today. Adding an entry is a reviewed edit here.
NOT_A_BUILD_STEPS: list[tuple[str, str, str, tuple[str, ...], str]] = [
    # (workflow file, job id, step id, the step's exact detector hits, why it builds no image)
]

# Before the subcommand, an option may take a value. Only a flag whose value can itself read
# as a subcommand has to be named: `docker --context build pull` is a context called build, and
# VALUED is that list - a container CLI's own documented globals, few and stable. Any other
# flag may take a value too, but never one of the subcommand words, so `docker --debug build .`
# and `pack --quiet build` are seen as the builds they are while `buildctl --addr tcp://h build`
# still reads its address. Options are atomic and never cross a line.
# A value is refused only when it IS a subcommand word, not when it merely starts with one:
# `-f build-compose.yml`, `-p run-tests` and `--profile up-stack` are values, and reading them
# as the subcommand made their commands vanish from the detector.
VERB = r"(build|bake|bud|b|commit|import|publish|resolve|apply|up|run|create|watch)(?![\w.:/-])"
VALUED = r"(--config|--context|-c|--host|-H|--log-level|-l|--tlscacert|--tlscert|--tlskey|--builder)"
QUOTED_VALUE = r"""("[^"\n]*"|'[^'\n]*'|\$\{\{[^}\n]*\}\})"""
OPT = (
    r"(?>([ \t]+(%s([ \t]+(%s|\S+)|=(%s|\S+))?"
    r"|-{1,2}[\w-]+(=(%s|\S+))?([ \t]+(%s|(?!%s)[^\s-]\S*))?))*)"
) % (VALUED, QUOTED_VALUE, QUOTED_VALUE, QUOTED_VALUE, QUOTED_VALUE, VERB)
NAME = r"(docker(-buildx)?|podman|nerdctl|depot|finch)"
CLI = (
    r"""["']?(\b""" + NAME + r"\b"
    r"|\$[({]?\w*DOCKER\w*[-:}\w)]*"
    r"|\$\([^)\n]*\b" + NAME + r"\b[^)\n]*\))[\"']?"
)
SUB = r"(?![\w.:/-])"
Q = r"""["']?"""  # a subcommand may be quoted: `docker "build" .`
# A global option may sit between the CLI and `compose` - `docker --context ci compose build`.
# The verb is matched at compose's own subcommand position, so `docker compose exec app go run`
# is not read as `compose run`, which would refuse it with a `--no-build` it cannot use.
COMPOSE_CLI = r"(\b(docker|podman|nerdctl)\b%s[ \t]+compose|\b(docker|podman|nerdctl)-compose)" % OPT
# `up|run|create|watch` without `--no-build`: a service carrying `build:` builds implicitly, so
# the remedy named for these is `--no-build`, which reads no compose file.
COMPOSE = COMPOSE_CLI + OPT + r"[ \t]+(up|run|create|watch)(?![\w-])(?![^\n]*--no-build)"
ANCHORED_BUILD = re.compile(
    rf"{CLI}{OPT}[ \t]+((image|builder)[ \t]+)?{Q}(build|bake){Q}{SUB}"
    rf"|({CLI}{OPT}[ \t]+)?buildx{OPT}[ \t]+b[a-z]*{SUB}"
    rf"|{COMPOSE_CLI}{OPT}[ \t]+build{SUB}"
    rf"|{COMPOSE}"
    rf"|\b(buildah|buildctl[\w.-]*|pack|skaffold|img){OPT}[ \t]+(build|bud){SUB}"
    rf"|\bko{OPT}[ \t]+(build|publish|resolve|apply)\b|\bskaffold{OPT}[ \t]+run\b"
    rf"|\bcrane[ \t]+append\b|\bjib:?[a-zA-Z]*[Bb]uild\b|/kaniko/executor"
)
RUN_STEP_BUILD = re.compile(
    r"\b(docker|podman|nerdctl|depot|finch)\b[^\n]*[\s-](?<!no-)[\"']?(build|bake|b)(?![\w-])"
    r"|\b(docker|podman|nerdctl|depot|finch)\b[^\n]*\s(commit|import)\b"
    r"|\$[({]?\w*DOCKER\w*[-:}\w)]*[\"']?[^\n]*[\s-](?<!no-)(build|bake|b)(?![\w-])"
    rf"|{COMPOSE}"
    r"|\bbuildx\s+b[a-z]*(?![\w-])"
    r"|\bbuildah\b"
    r"|\bbuildctl[\w.-]*\b[^\n]*\bbuild\b"
    r"|\bko\b[^\n]*\s(build|publish|resolve|apply)\b"
    r"|\bpack\b[^\n]*\sbuild\b|\bskaffold\b[^\n]*\s(build|run)\b"
    r"|\bimg\b[^\n]*\sbuild\b"
    r"|\bearthly\b[^\n]*\s\+\w"
    r"|\bcrane\s+append\b|\b[\w./]*jib:?[a-zA-Z]*[Bb]uild\b|/kaniko/executor"
)
# A compose hit has a remedy of its own: `--no-build` says the step starts services only.
COMPOSE_REMEDY = re.compile(r"compose\b.*[ \t](up|run|create|watch)$")
excused: list[str] = []
matches: dict[tuple, list[str]] = {}

SUBSTITUTION = re.compile(r"\$\(([^()\n]*)\)|`([^`\n]*)`")
QUOTED = re.compile(r"\"[^\"\n]*\"|'[^'\n]*'")
SAYS = re.compile(r"^\s*(\w+=\S*\s+)*(echo|printf)\b")


def strip_comments(script: str) -> str:
    """Drop `#` comments outside quotes. A comment saying `--no-build` must not excuse the

    build on the line above it, and a commented-out build must not be refused."""
    out, quote = [], ""
    for line in script.splitlines():
        kept = []
        for index, char in enumerate(line):
            if quote:
                if char == quote:
                    quote = ""
            elif char in "\"'":
                quote = char
            elif char == "#" and (index == 0 or line[index - 1].isspace()):
                break
            kept.append(char)
        quote = ""
        out.append("".join(kept))
    return "\n".join(out)


def commands(script: str) -> list[str]:
    """One command per entry, with nothing inside a substitution or a quoted string left to

    change how the command around it reads. A `$(…)` body and a quoted string are scanned as
    commands of their own, then replaced in the text they sat in - otherwise a `|` inside
    `--build-arg V=$(cmd | filter)` splits the build in half and the detector misses it. A
    quoted WORD is that word, so `docker "build" .` still reads as a build, and an `echo` or
    `printf` loses its quoted argument, which is text rather than a command."""
    text = re.sub(r"\\\s*\n", " ", strip_comments(script))
    inner: list[str] = []

    def blank_substitution(match: "re.Match[str]") -> str:
        body = match.group(1) if match.group(1) is not None else match.group(2)
        inner.append(body)
        named = re.search(NAME, body)
        return "$(%s)" % (named.group(0) if named else "arg")

    text = SUBSTITUTION.sub(blank_substitution, text)
    source = text

    def blank_quote(match: "re.Match[str]") -> str:
        body = match.group(0)[1:-1]
        if body and not re.search(r"\s", body):
            return body
        start = max((source.rfind(c, 0, match.start()) for c in "\n;&|("), default=-1) + 1
        if not SAYS.search(source[start : match.start()]):
            inner.append(body)
        return " arg "

    text = QUOTED.sub(blank_quote, text)
    found = re.split(r"[\n;&|]+", text)
    for body in inner:
        found.extend(re.split(r"[\n;&|]+", body))
    return found


def classify_run_step(step: dict) -> list[str]:
    """Every command in this step that reads as building an image, in order."""
    script = step.get("run")
    if not isinstance(script, str):
        return []
    found = []
    for command in commands(script):
        hit = RUN_STEP_BUILD.search(command) or ANCHORED_BUILD.search(command)
        if hit:
            found.append(hit.group(0).strip())
    return list(dict.fromkeys(found))

NON_BUILDING_ACTIONS = {
    "actions/checkout",
    "actions/download-artifact",
    "actions/setup-go",
    "actions/setup-node",
    "actions/upload-artifact",
    "docker/login-action",
    "docker/metadata-action",
    "docker/setup-buildx-action",
    "docker/setup-qemu-action",
    "dorny/paths-filter",
    "oven-sh/setup-bun",
}
# `docker/build-push-action` inputs this check has read and found to add no build input, so a
# build using only these is fully described by its `context` and `file`. `build-contexts` is
# deliberately absent: a named context is another tree the build reads, and the trigger would
# owe it, and so is `secret-files`, which names a path. `secrets` IS on the list below: it
# takes `id=value` pairs, which are values, not paths.
BUILD_ACTION_INPUTS = {
    "annotations",  # metadata on the result
    "build-args",  # values, not paths
    "builder",  # which builder runs it
    "cache-from",  # where layers are reused from
    "cache-to",  # where layers are written
    "context",  # READ: the tree the build receives
    "file",  # READ: the Dockerfile
    "github-token",  # auth
    "labels",  # metadata on the result
    "load",  # where the result goes
    "no-cache",  # cache behaviour
    "no-cache-filters",  # cache behaviour
    "secrets",  # `id=value` pairs; values, not paths (`secret-files` IS a path, so it is not here)
    "platforms",  # target architectures
    "provenance",  # attestation toggle
    "pull",  # base-image freshness
    "push",  # where the result goes
    "sbom",  # attestation toggle
    "tags",  # names on the result
    "target",  # which stage to stop at; narrows what is read, never widens
}
# A context this check can resolve to a repository path: no expression, no URL, no named
# context.
PLAIN_CONTEXT = re.compile(r"[\w.][\w./-]*\Z")

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
    skipped.

    A job whose own `if:` says `always()` or `!cancelled()` runs past a skipped need, so the
    walk does not descend into its `needs:` — except into a job whose result that same `if:`
    demands. `always() && needs.X.result == 'success'` is exempt from the skip rule and then
    re-imposes X's gate by hand, so X still decides whether this job runs."""
    gates, seen, queue = [], set(), [job_name]
    while queue:
        name = queue.pop()
        if name in seen:
            continue
        seen.add(name)
        job = (document.get("jobs") or {}).get(name) or {}
        condition = str(job.get("if", ""))
        found = gating_filters(document, f"jobs.{name}", job)
        if found:
            gates.append((f"jobs.{name}.if", found))
        demanded = required_results(condition)
        queue += sorted(demanded)
        if "always()" in condition or "cancelled()" in condition:
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


def scan_step(workflow, job_name, step: dict, gate_step: dict, origin: str, depth: int, prefix: str = "") -> None:
    """Classify one step: a build this check reads, an action it trusts, or a refusal.

    `gate_step` is the step whose `if:` gates the build — the step itself at job level, and the
    CALLING step when this one came out of a local composite action, since that is what decides
    whether the composite runs at all. `origin` names the file the step lives in."""
    uses = str(step.get("uses", ""))
    action = uses.split("@", 1)[0]
    where = step.get("name") or "an unnamed step"
    # A step is addressed by its `id:`, joined under the calling step's id when it came out of
    # a local composite action. A step without one cannot be listed, and its refusal says so.
    own = str(step.get("id", "")).strip()
    step_id = f"{prefix}/{own}" if prefix and own else (own if not prefix else "")
    place = f"{workflow}: {where}" if origin == str(workflow) else f"{workflow}: {where} (in {origin})"
    if action == "docker/build-push-action":
        options = step.get("with") or {}
        unknown = sorted(set(map(str, options)) - BUILD_ACTION_INPUTS)
        if unknown:
            problems.append(
                f"::error file={workflow}::{place} passes {unknown[0]} to "
                f"docker/build-push-action, an input this check has not read; it may name "
                f"another tree the build reads (build-contexts, secret-files), so the trigger "
                f"would owe those files too. Add it to BUILD_ACTION_INPUTS in this script once "
                f"you have checked what it reads"
            )
            return
        cache = str(options.get("cache-from", ""))
        if re.search(r"type=local\b", cache):
            problems.append(
                f"::error file={workflow}::{place} reads cache-from {cache.strip()}, a local "
                f"cache directory this build takes as an input; use a registry or gha cache so "
                f"this check can read what the build receives"
            )
            return
        raw = str(options.get("context", "."))
        if not PLAIN_CONTEXT.fullmatch(raw):
            problems.append(
                f"::error file={workflow}::{place} builds from context {raw}, which is not a "
                f"plain repository path; give it a path so this check can read what the build "
                f"receives"
            )
            return
        context = posixpath.normpath(raw)
        dockerfile = str(options.get("file", posixpath.join(context, "Dockerfile")))
        builds.append((workflow, job_name, gate_step, context, posixpath.normpath(dockerfile)))
        return
    if uses:
        if action in NON_BUILDING_ACTIONS:
            return
        if action.startswith("./"):
            scan_local_action(workflow, job_name, action, gate_step, place, depth, step_id)
            return
        problems.append(
            f"::error file={workflow}::{place} uses {uses}, which this check cannot tell apart "
            f"from an image builder; build images with docker/build-push-action, and add this "
            f"action to NON_BUILDING_ACTIONS in this script if it builds none"
        )
        return
    hits = classify_run_step(step)
    if not hits:
        return
    shown = ", ".join(repr(hit) for hit in hits)
    entry = next(
        (e for e in NOT_A_BUILD_STEPS if (e[0], e[1], e[2]) == (str(workflow), job_name, step_id)),
        None,
    ) if step_id else None
    if entry is not None:
        matches.setdefault(entry, []).append(place)
        if tuple(hits) == tuple(entry[3]):
            excused.append(f"{workflow}: job {job_name}: step {step_id}: {shown} - {entry[4]}")
            return
        was = ", ".join(repr(hit) for hit in entry[3])
        problems.append(
            f"::error file={workflow}::{place} is listed in NOT_A_BUILD_STEPS, but the entry no "
            f"longer describes it: it was reviewed as building {was or 'nothing'} and now "
            f"builds {shown}. Re-review the step and update its entry"
        )
        return
    remedy = (
        "add --no-build if the step should not build"
        if any(COMPOSE_REMEDY.search(hit) for hit in hits)
        else "reword the command so it does not read as a build (`--flag=value`)"
    )
    problems.append(
        f"::error file={workflow}::{place} builds an image in a run step ({shown}); build it "
        f"with docker/build-push-action so this check can read it, {remedy}, or give the step "
        f"an `id:` and add ({str(workflow)!r}, {job_name!r}, its id, its hits, why it builds "
        f"nothing) to NOT_A_BUILD_STEPS in this script in a reviewed PR"
    )


def scan_local_action(workflow, job_name, action: str, gate_step: dict, place: str, depth: int, prefix: str) -> None:
    """A `./` action is read, not trusted: a composite's steps get the same rules recursively.

    Anything else is refused. A container action's image comes from a Dockerfile this check
    never sees, and a JavaScript action is as opaque as a remote one — `docker/build-push-
    action` is itself JavaScript — so a local `using: node*`, an unknown `using`, and a missing
    `runs.using` are all refused unless NON_BUILDING_ACTIONS names the action."""
    if depth > 5:
        problems.append(f"::error file={workflow}::{place} nests local actions too deeply to read")
        return
    definition = next(
        (p for p in (Path(action[2:]) / "action.yml", Path(action[2:]) / "action.yaml") if p.is_file()),
        None,
    )
    if definition is None:
        problems.append(
            f"::error file={workflow}::{place} uses {action}, which has no action.yml this "
            f"check can read"
        )
        return
    document = load(definition) or {}
    runs = document.get("runs") if isinstance(document, dict) else None
    using = str((runs or {}).get("using", ""))
    if using == "composite":
        for inner in (runs or {}).get("steps") or []:
            if isinstance(inner, dict):
                scan_step(workflow, job_name, inner, gate_step, str(definition), depth + 1, prefix)
        return
    problems.append(
        f"::error file={workflow}::{place} uses {action}, a local `using: {using or 'unset'}` "
        f"action whose program this check cannot read; build images with "
        f"docker/build-push-action, and add this action to NON_BUILDING_ACTIONS in this script "
        f"if it builds none"
    )


for workflow, document in documents.items():
    for job_name, job in (document.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        called = str(job.get("uses", ""))
        if called and not called.startswith("./.github/workflows/"):
            problems.append(
                f"::error file={workflow}::{workflow}: jobs.{job_name} calls {called}, a "
                f"workflow outside this repository, whose builds this check cannot read; call "
                f"a ./.github/workflows/ workflow instead"
            )
            continue
        for step in job.get("steps") or []:
            if isinstance(step, dict):
                scan_step(workflow, job_name, step, step, str(workflow), 0)

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

# Every excusal is printed, so a reader can audit the list against what it excused.
for excusal in dict.fromkeys(excused):
    print(f"listed in NOT_A_BUILD_STEPS: {excusal}")

# An entry must name exactly one step that reads as a build. Zero means the step was deleted,
# renamed or no longer builds; more than one means the key does not identify a step. Either
# fails, rather than rotting into a standing exemption nobody reviews again.
for entry in NOT_A_BUILD_STEPS:
    found = matches.get(entry, [])
    if len(found) == 1:
        continue
    problems.append(
        f"::error file={entry[0]}::NOT_A_BUILD_STEPS names {entry[0]} job {entry[1]} step id "
        f"{entry[2]!r}, which matches {len(found)} steps that build; an entry must name exactly "
        f"one, so remove it or correct the id"
    )

problems = list(dict.fromkeys(problems))
for problem in problems:
    print(problem, file=sys.stderr)
if problems:
    print(f"::error::{len(problems)} image trigger problem(s)", file=sys.stderr)
    raise SystemExit(1)
print(f"every image trigger covers every input its build reads ({len(builds)} image build(s))")
PY
