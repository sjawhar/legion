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
#     `dorny/paths-filter` outputs tested `== 'true'` and joined by `||`, to event terms, or to
#     terms that cannot narrow by path, or it is refused. GitHub compares case-insensitively,
#     so this check does too. A filter is read under its step's `predicate-quantifier`: under
#     `some` (the default) a file matches when any pattern does, and a `!` pattern, which
#     there matches every file outside it, is refused; under `some-with-excludes` a file
#     matches when a pattern without `!` does and no `!` pattern does, so a `!` pattern
#     excludes. Any other quantifier (`every` among them), a pattern holding a brace, which
#     picomatch expands, and a filter whose name two paths-filter steps of one job define,
#     are refused. One more term is admitted beside the filters, in a job's `if:` only: a
#     CALL MODE, `inputs.<name> == ''`, exactly the empty literal, on a <name> the workflow's
#     own `on.workflow_call.inputs` declares `type: string` with no `default` or `default:
#     ""`, and that its `on.workflow_dispatch.inputs` leaves out or declares the same way. A
#     call that leaves such an input out gets its default, "", a dispatch that sets no value
#     gets "", and a run of a trigger with no inputs (`push`, `pull_request`) has it null,
#     which GitHub compares equal to '' (both coerce to 0), so the build runs on every run
#     whose starter leaves the input out. worker-image.yaml's build job runs only on
#     `inputs.attest_digest == ''`, so release.yaml's attest-only call, which sets
#     attest_digest, skips it. The caller picks that value, not the change, and every caller's
#     triggers and gates are traced below as triggers of this workflow anyway. That shape is
#     the one this check admits, because it knows it to be '' wherever no starter sets it; the
#     check does not model GitHub's number coercion beyond it, so every other test of an input
#     is refused rather than reasoned about, whether or not it would hold while the input is
#     left out: `== ''` on a non-empty default, on a boolean or number input, or on one
#     `workflow_dispatch` declares with a non-empty default or another type; any `!=`; and
#     `== '<non-empty>'`. The one gap: this check does not read a caller's `with:`, so a
#     caller passing a path-derived value (a paths-filter output, say) into such an input
#     would narrow by path unseen. No caller does that today, and review holds it. Everywhere
#     else an `inputs.` term is refused: in a step's `if:`, in a workflow without
#     `workflow_call` or whose `workflow_call` does not declare that input as such a string,
#     and in any shape but `== ''`.
#   * A build in a `run:` step is DETECTED, and EVERY detection is refused - there is no
#     pattern that can excuse one. The detector judges ONE COMMAND at a time and is drawn
#     broad: a container-CLI word followed later on the command by `build`, `bake` or `b`, the
#     other builders, and `compose up|run|create|watch` without `--no-build`. A broad detector
#     needs an escape hatch for its false positives, and that hatch is DATA, not a shape:
#     NOT_A_BUILD_STEPS lists steps by (workflow, job, step `id:`) - ids, because a step's name
#     is neither required nor unique - pinning the FULL TEXT of each command that hit and why
#     the step builds nothing. A step can be listed when its own text is the whole story - a
#     loose hit, or an anchored one carrying an inspecting flag like `--print` or `--check`.
#     What cannot is a command whose build is not in the step: compose's build lives in its
#     compose file and earthly's in its Earthfile, and a plain build, `docker commit` or
#     `docker import` always makes one. A hit on a listed step is excused
#     and printed while its commands still match exactly; a hit anywhere else, or one the entry
#     did not describe, is refused. An entry matching no step or several fails the check rather
#     than rotting. So a build is admitted only when the detector misses it (e.g. a builder it
#     does not name, a variable-named engine, a script file, text piped to a shell, a
#     non-shell `shell:` - the paragraph above RUN_STEP_BUILD is the list), or when its step is
#     listed in NOT_A_BUILD_STEPS. Measured over 117 build shapes: all 117 detected, 0 of this
#     repository's 122 run steps matched, and the list is empty - nothing is excused today.
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
# Two more kinds of term in those same `if:`s are read. A top-level `&&` conjunct made only of
# `github.event_name == '<event>'` terms joined by `||` admits just those events, and a
# `github.event_name != '<event>'` conjunct excludes that one, so coverage is owed only on the
# triggers whose event is still admitted - for a calling job, the CALLER's triggers, since a
# called workflow sees its caller's event. Any other use of github.event_name is refused.
# `!github.event.pull_request.draft` as a top-level `&&` conjunct of neutral terms skips only a
# draft's run; it is path-neutral while every `pull_request` and `pull_request_target` trigger
# that runs its holder - its own workflow's and each `workflow_call` caller's - lists
# `ready_for_review` in its `types` (no `types` means GitHub's default, which leaves it out),
# because the ready run that follows selects by the same diff. Otherwise it is refused, naming
# the trigger and the type to add. `||`-joined into the filters clause it skips no draft, only
# widens, and so owes nothing.
# `github.event.action != 'closed'` skips only a closed pull request's run, which merges nothing
# more, so it is path-neutral too. The comment above NEUTRAL says why both are.
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
#   5. no image build found at all, since that checks nothing,
#   6. the other direction: a pull request trigger path that starts an image build and nothing
#      else, yet matches a file no build it starts reads, since each push to a pull request
#      touching only such files then builds an image for nothing. That is (a) a `pull_request`
#      or `pull_request_target` trigger of a workflow whose every job builds, calls a workflow
#      that only builds, or needs a job that does, with no paths-filter filter selecting the
#      build; and (b) a path that paths-filter filters select, where each of those filters
#      decides only jobs that only build — a building job with nothing beside its build but
#      checkout, buildx and registry steps, or a call of a workflow that only builds — and the
#      job's gates admit a pull request event. There the event's paths AND the filters must
#      match only files the builds read, or the workflows involved (the building one and the
#      caller). A `push` or `merge_group` trigger, a filter that also decides a job doing more,
#      and a workflow with other jobs are left alone: what they start reads more than an image.
#
# Files are discovered by walking the tree, not by asking git, so an unsnapshotted change cannot
# read green locally.
#
# Run from anywhere: .github/scripts/check-image-trigger-paths.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The path model this check reads workflows with: workflow_paths.py, beside this script in the
# repository it checks. PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__ in the
# tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import posixpath
import re
import shlex
import sys
from pathlib import Path

import yaml
from workflow_paths import Filter, PathsFilter, events, glob_regex, unread_filter_patterns, unread_patterns, walk

WORKFLOWS = Path(".github/workflows")
PATH_EVENTS = ("push", "pull_request", "pull_request_target", "merge_group")
NEEDS_OUTPUT = re.compile(r"needs\.([\w-]+)\.outputs\.([\w-]+)")
STEP_OUTPUT = re.compile(r"steps\.([\w-]+)\.outputs\.([\w-]+)")
EQUALS_TRUE = re.compile(r"needs\.([\w-]+)\.outputs\.([\w-]+)\s*==\s*'true'")
# The only `if:` terms that do not narrow which CHANGES reach a job anywhere, so, with CALL_MODE
# below, the only ones that can sit beside the filters clause without being refused. GitHub's
# comparisons are case-insensitive (Expressions → Operators), so these are too:
# `needs.X.result == 'Success'` must read exactly like `== 'success'`, or a capital letter walks
# a narrowing gate past the allowlist. `!github.event.pull_request.draft` and
# `github.event.action != 'closed'` narrow by event, not by change. Both are true wherever a
# deployment's pinnable image is built: push, merge_group, workflow_call from a push and
# workflow_dispatch carry no pull_request and no
# closed action. On a pull request, `!= 'closed'` skips only a closed pull request, which merges
# nothing more. The draft term skips a draft's head, which is safe only if the trigger builds
# that head when the draft is marked ready: otherwise a draft marked ready with no new push
# merges with no image built at its head. So gating_filters admits the draft term only while
# every `pull_request` and `pull_request_target` trigger that runs its holder (its own
# workflow's, and each `workflow_call` caller's: unready_triggers) lists `ready_for_review` in
# its `types`. A trigger with no `types` gets GitHub's default, opened, synchronize and
# reopened, so it is refused too. The draft term skips a draft only as a top-level `&&`
# conjunct of its own neutral terms; inside the filters clause it is one more `||` term, which
# only widens, so gate_outputs looks for it only among the neutral conjuncts. Their opposites
# (`github.event.pull_request.draft`, `== true`, `github.event.action == 'closed'`) skip every
# build a deployment pins, so they stay refused.
DRAFT = re.compile(r"!\s*github\.event\.pull_request\.draft", re.I)
NEUTRAL = re.compile(
    r"always\(\)|success\(\)|!\s*cancelled\(\)|needs\.[\w-]+\.result(\s*[=!]=\s*'[A-Za-z_]+')?"
    rf"|{DRAFT.pattern}|github\.event\.action\s*!=\s*'closed'",
    re.I,
)
# The activity types a `pull_request` or `pull_request_target` trigger without `types` runs on.
DEFAULT_PULL_REQUEST_TYPES = ("opened", "synchronize", "reopened")
RESULT_TERM = re.compile(r"needs\.([\w-]+)\.result\s*(==|!=)\s*'([A-Za-z_]+)'", re.I)
# An event term: which trigger events reach a job. GitHub compares the value case-insensitively.
EVENT_TERM = re.compile(r"github\.event_name\s*(==|!=)\s*'([\w-]+)'", re.I)
# A CALL MODE (the GATES paragraph above): `inputs.<name> == ''`, exactly the empty literal, so
# `== ''''` (GitHub's escaped quote, the one-quote string) is not one. Neutral only for the input
# names call_inputs admits for the job's own workflow, the `modes` gate_outputs is handed.
CALL_MODE = re.compile(r"inputs\.([\w-]+)\s*==\s*''", re.I)
INPUTS_TERM = re.compile(r"(?<![\w.])inputs\.", re.I)


def call_inputs(document: dict) -> dict[str, str | None]:
    """The inputs a job-level CALL_MODE term may name, by name lowercased (GitHub reads names
    case-insensitively): None for each the term is admitted on, or, for one only its
    `on.workflow_dispatch.inputs` declaration spoils, what that declaration says. The check
    admits one known-safe shape and does not model GitHub's number coercion beyond it: an
    input the workflow's own `on.workflow_call.inputs` declares `type: string` with no
    `default` or `default: ""`, and that `on.workflow_dispatch.inputs` leaves out or declares
    the same way. Such an input is "" on a call that omits it and on a dispatch that sets no
    value, and null, which GitHub compares equal to '', on a trigger with no inputs. A
    workflow_call input declared any other way is left out, so the refusal names the admitted
    shape; a workflow_dispatch declaration with a non-empty default or another type is refused
    with its own reason, since a dispatch that keeps that value need not have the input ''."""
    found = events(document)
    call, dispatch = found.get("workflow_call"), found.get("workflow_dispatch")
    declared = call.get("inputs") if isinstance(call, dict) else None
    if not isinstance(declared, dict):
        return {}
    dispatched = dispatch.get("inputs") if isinstance(dispatch, dict) else None
    dispatched = dispatched if isinstance(dispatched, dict) else {}
    by_key = {str(name).lower(): (name, spec) for name, spec in dispatched.items()}
    modes: dict[str, str | None] = {}
    for name, spec in declared.items():
        if not (isinstance(spec, dict) and spec.get("type") == "string" and spec.get("default", "") == ""):
            continue
        key = str(name).lower()
        modes[key] = None
        if key not in by_key:
            continue
        dispatch_name, dispatch_spec = by_key[key]
        dispatch_spec = dispatch_spec if isinstance(dispatch_spec, dict) else {}
        if dispatch_spec.get("default", "") != "":
            how = f"with default: {json.dumps(dispatch_spec['default'], default=str)}"
        elif dispatch_spec.get("type") != "string":
            how = f"as type: {dispatch_spec['type']}" if "type" in dispatch_spec else "with no type"
        else:
            continue
        modes[key] = f"on.workflow_dispatch.inputs.{dispatch_name} declares it {how}"
    return modes


def neutral(term: str, modes: dict[str, str | None]) -> bool:
    """True for an `if:` term that cannot narrow which CHANGES reach a job: a NEUTRAL term, or
    a CALL_MODE comparison on an input `modes` admits."""
    term = unwrap(term).strip()
    if NEUTRAL.fullmatch(term):
        return True
    mode = CALL_MODE.fullmatch(term)
    return bool(mode) and mode.group(1).lower() in modes and modes[mode.group(1).lower()] is None


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


FILES = walk()


class AnyOf:
    """Covers what any one of its filters covers: a job gated on `a == 'true' || b == 'true'`."""

    def __init__(self, filters: list[PathsFilter]):
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
            raise Unreadable(
                f"malformed JSON-array form: {error.msg}; write it as valid JSON, or as a "
                f"plain argument list"
            ) from error
        flags = []
    else:
        try:
            words = shlex.split(arguments)
        except ValueError as error:
            raise Unreadable(
                f"unparseable: {error}; balance its quotes so a shell could read it"
            ) from error
        flags = [word for word in words if word.startswith("--")]
        entries = [word for word in words if not word.startswith("--")]
    if any(flag.startswith("--from=") for flag in flags):
        return []
    if not isinstance(entries, list) or len(entries) < 2:
        raise Unreadable("no source and destination; give it both")
    sources = []
    for source in entries[:-1]:
        if "://" in source or source.startswith("git@"):
            continue
        if source.startswith("<<"):
            raise Unreadable(
                "a heredoc source, which this check cannot read; write the content into a "
                "file the context carries, and COPY that"
            )
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
# CALLING step's id and its own, joined with `/`, unique in the calling job even when the
# action is used twice - and an inner step whose CALLER has no id has no key at all, or it
# would answer to an entry a job-level step of the same id once owned.
#
# The list holds the false positives whose OWN TEXT is the whole story: a loose-detector hit
# like `docker run --rm golang go build ./...`, and an anchored shape carrying a flag that says
# it only inspects (`buildx bake --print`, `buildx build --check`, `compose build --dry-run`,
# `--help`, `--call=check`). What cannot be listed is a command whose build is NOT in the step:
# `docker compose ... up` builds what its COMPOSE FILE says, which can grow a `build:` with the
# step untouched, and `earthly +target` builds what the EARTHFILE's SAVE IMAGE says - no flag
# on the step can vouch for either. Those get `--no-build` or docker/build-push-action, and an
# entry for one fails. So does one for a plain build, `docker commit` or `docker import`, which
# always make an image.
#
# Every refusal names a remedy this check then accepts, with TWO exceptions it names instead:
# `earthly`, whose image comes from an Earthfile this check cannot read, and `compose watch`,
# which rebuilds by design and whose `--no-up` does not stop it. A step running either has to
# move to docker/build-push-action or out of this check's reach.
#
# Each entry pins COMMANDS: the full text of each command that hit, as the detector reads it -
# not the matched span, which is the same `docker buildx build` in a `--check` lint and a
# `--load` build. The step is excused only while its commands are exactly those, so a step
# whose own text grows a build is refused rather than riding its entry. An entry matching no
# step, or more than one, fails.
#
# Empty: this repository has no false positive today. Adding an entry is a reviewed edit here.
NOT_A_BUILD_STEPS: list[tuple[str, str, str, tuple[str, ...], str]] = [
    # (workflow file, job id, step id, the step's exact hitting commands, why it builds nothing)
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
# A service carrying `build:` builds implicitly under `up`, `run`, `create` and `watch`.
# `--no-build` says otherwise, but Compose takes it on `up` and `create` ONLY, so it can excuse
# just those two - and only as a flag of the command, not as a word inside a value such as
# `-e X=--no-build`.
NO_BUILD = r"(?![^\n]*(^|[ \t])--no-build(?![\w-]))"
COMPOSE = (
    COMPOSE_CLI + OPT + r"[ \t]+((up|create)(?![\w-])" + NO_BUILD + r"|(run|watch)(?![\w-]))"
)
ANCHORED_BUILD = re.compile(
    rf"{CLI}{OPT}[ \t]+((image|builder|container)[ \t]+)?{Q}(build|bake|commit|import){Q}{SUB}"
    rf"|({CLI}{OPT}[ \t]+)?buildx{OPT}[ \t]+b[a-z]*{SUB}"
    rf"|{COMPOSE_CLI}{OPT}[ \t]+build{SUB}"
    rf"|{COMPOSE}"
    rf"|\b(buildah|buildctl[\w.-]*|pack|skaffold|img){OPT}[ \t]+(build|bud){SUB}"
    rf"|\bbuildah{OPT}[ \t]+commit{SUB}"
    rf"|\bko{OPT}[ \t]+(build|publish|resolve|apply)\b|\bskaffold{OPT}[ \t]+run\b"
    rf"|\bearthly\b[^\n]*[ \t]\S*\+\w"
    rf"|\bcrane[ \t]+append\b|\bjib:?[a-zA-Z]*[Bb]uild\b|/kaniko/executor"
)
# A command whose own text says it only inspects: `buildx bake --print`, `buildx build
# --check`, `bake --list=targets`, `compose build --dry-run`, `--help`, and `--call` with a
# frontend method that only reports. This makes an otherwise-certain shape LISTABLE - what it
# does is visible in the step - which P1's full-text pin keeps safe, since swapping `--check`
# for `--load` changes the pinned command and re-opens the review. It does not reach a compose
# verb or `earthly`, whose build lives in a file the step only names.
INSPECTING = re.compile(
    r"(^|\s)(--print|--list|--dry-run|--check|--help)(?!=(false|0)(\s|$))(=|\s|$)"
    r"|(^|\s)--call(=|[ \t]+)(check|outline|targets)(\s|$)"
)
EARTHLY = re.compile(r"\bearthly\b[^\n]*[ \t]\S*\+\w")
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
    r"|\bearthly\b[^\n]*\s\S*\+\w"
    r"|\bcrane\s+append\b|\b[\w./]*jib:?[a-zA-Z]*[Bb]uild\b|/kaniko/executor"
)
COMPOSE_VERB = re.compile(COMPOSE)
# Which compose verb a refusal is about: only `up` and `create` accept `--no-build`, `run`
# has a rewrite that does the same work, and `watch` rebuilds by design and has neither.
COMPOSE_TAKES_NO_BUILD = re.compile(COMPOSE_CLI + OPT + r"[ \t]+(up|create)(?![\w-])")
COMPOSE_RUN = re.compile(COMPOSE_CLI + OPT + r"[ \t]+run(?![\w-])")
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


def classify_run_step(step: dict) -> list[tuple[str, bool]]:
    """Every command in this step that reads as building an image: the command's FULL text,

    normalised as the detector reads it, and whether the anchored recogniser saw a certain
    build in it. The full text, not the matched span, is what an entry pins: the span from the
    CLI word to the build word is identical in `docker buildx build --check .` and `docker
    buildx build --load -t x .`, so pinning it would let a lint's entry excuse a real build.
    Nothing is de-duplicated either, or a second identical build line would vanish."""
    script = step.get("run")
    if not isinstance(script, str):
        return []
    found = []
    for command in commands(script):
        hit = RUN_STEP_BUILD.search(command) or ANCHORED_BUILD.search(command)
        if not hit:
            continue
        certain = bool(ANCHORED_BUILD.search(command))
        if certain and INSPECTING.search(command) and not (COMPOSE_VERB.search(command) or EARTHLY.search(command)):
            certain = False
        found.append((" ".join(command.split()), certain))
    return found

NON_BUILDING_ACTIONS = {
    "actions/attest-build-provenance",
    "actions/cache",
    "actions/cache/restore",
    "actions/cache/save",
    "actions/checkout",
    "actions/configure-pages",
    "actions/deploy-pages",
    "actions/download-artifact",
    "actions/setup-go",
    "actions/setup-node",
    "actions/upload-artifact",
    "actions/upload-pages-artifact",
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
    "call",  # which frontend method runs (build, check, outline, targets); names no path
    "context",  # READ: the tree the build receives
    "file",  # READ: the Dockerfile
    "github-token",  # auth
    "labels",  # metadata on the result
    "load",  # where the result goes
    "no-cache",  # cache behaviour
    "no-cache-filters",  # cache behaviour
    "outputs",  # where the result goes; an exporter writes the result and names no input
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
        raise Unreadable(f"{source} names a variable, which this check cannot resolve; write the path literally")
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
        raise Unreadable(f"{source} matches no file in the context; correct the path, or remove the instruction")
    return found


def parents(path: str) -> list[str]:
    parts = path.split("/")[:-1]
    return ["/".join(parts[: n + 1]) for n in range(len(parts))]


def load(path: Path):
    try:
        return yaml.safe_load(path.read_text())
    except yaml.YAMLError as error:
        problems.append(f"::error file={path}::{path} is not valid YAML: {' '.join(str(error).split())}; fix the "
        f"syntax so this check can read the workflow")
        return None


def event_filters(workflow: Path, document: dict) -> list[tuple[str, str, Filter]]:
    """(event, label, filter) for each path-triggering event of the workflow."""
    found = []
    for name, spec in events(document).items():
        if name not in PATH_EVENTS:
            continue
        spec = spec if isinstance(spec, dict) else {}
        key = next((k for k in ("paths", "paths-ignore") if k in spec), None)
        if key is None:
            found.append((name, f"on.{name}", Filter(None)))
            continue
        # glob_regex reads patterns as picomatch does, so it refuses GitHub's `?`, `+`, `\` and
        # braces rather than guess (workflow_paths.unread_patterns).
        unread = unread_patterns(spec[key])
        if unread:
            problems.append(
                f"::error file={workflow}::{workflow} on.{name}.{key} has {', '.join(unread)}: "
                f"this check does not read GitHub's ?, + or \\ or braces in a path filter; write "
                f"the filter with `*`, `**` and literal characters"
            )
            continue
        found.append((name, f"on.{name}.{key}", Filter(spec[key], ignore=key == "paths-ignore")))
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


def gate_outputs(
    condition: str, modes: dict[str, str | None]
) -> tuple[list[tuple[str, str]], set[str], bool]:
    """(job, output) for each paths-filter output an `if:` is gated on, the trigger events it
    admits, and whether it skips a draft.

    An allowlist, because unknown must mean refused and never ungated: a condition this check
    does not model can narrow the gate on anything at all — `if: false`, `vars.X == 'true'`,
    `github.ref == 'refs/heads/main'` — and reading it as ungated asserts coverage the job does
    not have. Each top-level `&&` conjunct is therefore THE filters clause
    (`needs.<job>.outputs.<name> == 'true'` tests joined by `||`, at most one such conjunct;
    other `||` terms there only widen, so they are allowed); or made entirely of terms that do
    not narrow by path — `always()`, `success()`, `!cancelled()`, `needs.<job>.result`, the
    two event terms `!github.event.pull_request.draft` and `github.event.action != 'closed'`,
    and a CALL_MODE comparison on an input `modes` admits; or an event term —
    `github.event_name == '<event>'` tests joined by `||`, which admits only those events, or
    one `github.event_name != '<event>'`, which excludes that event; or it is refused, quoting
    the term. The `if:` skips a draft when one of those neutral conjuncts holds the draft term;
    in the filters clause the draft term only widens. gating_filters admits that skip only
    under triggers that list `ready_for_review`; the comment above NEUTRAL says why. `modes`
    is empty unless the `if:` is a job's, in a workflow whose `workflow_call` declares inputs
    call_inputs returns."""
    admitted = set(PATH_EVENTS)
    skips_draft = False
    if not condition.strip():
        return [], admitted, skips_draft
    conjuncts = [unwrap(part) for part in split_top(unwrap(condition), "&&")]
    clauses = [part for part in conjuncts if NEEDS_OUTPUT.search(part)]
    for part in conjuncts:
        if part in clauses:
            continue
        terms = [unwrap(term).strip() for term in split_top(part, "||")]
        if all(neutral(term, modes) for term in terms):
            skips_draft = skips_draft or any(DRAFT.fullmatch(term) for term in terms)
            continue
        named = [EVENT_TERM.fullmatch(term) for term in terms]
        if all(match and match.group(1) == "==" for match in named):
            admitted &= {match.group(2).lower() for match in named}
            continue
        if len(terms) == 1 and named[0] and named[0].group(1) == "!=":
            admitted.discard(named[0].group(2).lower())
            continue
        spoiled = [modes[name.lower()] for name in CALL_MODE.findall(part) if modes.get(name.lower())]
        if spoiled:
            hint = (
                f"; {spoiled[0]}, which this check does not read as '' on a dispatch that sets "
                f"no value, so the build may skip there; declare the workflow_dispatch input "
                f'`type: string` with no default or `default: ""`, or leave it out of '
                f"workflow_dispatch"
            )
        elif INPUTS_TERM.search(part):
            hint = (
                "; an `inputs.<name> == ''` comparison is admitted only in a job's if:, on a "
                "`type: string` input the workflow's `on.workflow_call.inputs` declares with no "
                'default or `default: ""` and its `on.workflow_dispatch.inputs` leaves out or '
                "declares the same way"
            )
        else:
            hint = ""
        raise Unreadable(
            f"is narrowed by `{' '.join(part.split())}`, which this check cannot evaluate "
            f"against a path filter; express which CHANGES reach the job as a "
            f"dorny/paths-filter output tested `== 'true'`, and leave anything else "
            f"(`always()`, `success()`, `!cancelled()`, a `needs.<job>.result` comparison that "
            f"still admits `skipped`, `github.event_name == '<event>'` tests joined by ||, one "
            f"`github.event_name != '<event>'`, `!github.event.pull_request.draft`, "
            f"`github.event.action != 'closed'`) beside it{hint}"
        )
    if not clauses:
        return [], admitted, skips_draft
    if len(clauses) > 1:
        raise Unreadable(
            "tests needs.*.outputs in more than one &&-joined clause, so no single filter's "
            "match is enough for the job to run; join the filters this job needs with || in "
            "one clause"
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
                f"`needs.<job>.outputs.<name> == 'true'` joined by ||; write it that way"
            )
    return outputs, admitted, skips_draft


def gating_filters(
    workflow: Path, describe: str, holder: dict, modes: dict[str, str | None] | None = None
) -> tuple[list[tuple[str, PathsFilter]], set[str]]:
    """The dorny/paths-filter filters an `if:` is gated on, and the trigger events it admits.
    The holder is a job that builds an image, a job that calls a workflow which does, a job
    either of those needs, or the build step itself, in `workflow`; `modes` are the CALL_MODE
    inputs its `if:` may test, none for a step. A gate this check cannot evaluate is reported,
    never ignored, since ignoring it would treat a gated build as one that runs on every
    change. So is a draft term under a trigger that does not build the head once the draft is
    marked ready (the comment above NEUTRAL)."""
    document = documents[workflow]
    condition = str(holder.get("if", ""))
    try:
        outputs, admitted, skips_draft = gate_outputs(condition, modes or {})
    except Unreadable as error:
        problems.append(f"::error::{describe} builds or calls an image, and its if: {error}")
        return [], set(PATH_EVENTS)
    if skips_draft:
        for trigger, event in unready_triggers(workflow):
            problems.append(
                f"::error file={trigger}::{workflow}: {describe} builds or calls an image and "
                f"its if: skips a draft with `!github.event.pull_request.draft`, but {trigger} "
                f"on.{event}, which runs it, does not list ready_for_review in its types (no "
                f"types means GitHub's default: {', '.join(DEFAULT_PULL_REQUEST_TYPES)}), so a "
                f"draft marked ready with no new push builds no image at its head; add "
                f"ready_for_review to the {event} types of {trigger}, or drop the draft term"
            )
    jobs = document.get("jobs") or {}
    found = []
    for needed, output in outputs:
        upstream = jobs.get(needed) or {}
        reference = output_filter(document, needed, output)
        step = None
        if reference:
            step = next(
                (s for s in paths_filter_steps(upstream) if s.get("id") == reference[0]),
                None,
            )
        if step is None:
            problems.append(
                f"::error::{describe} builds or calls an image and is gated on "
                f"needs.{needed}.outputs.{output}, which is not a dorny/paths-filter filter "
                f"this check can read; make that output a paths-filter filter, or drop the gate"
            )
            continue
        name = reference[1]
        # A filter of the same name in another paths-filter step of the job reads like the gate,
        # while the output reads only one of them.
        holders = [str(s.get("id", "")) for s in paths_filter_steps(upstream) if name in step_filters(s)]
        if len(holders) > 1:
            problems.append(
                f"::error::jobs.{needed} defines filter {name} in two dorny/paths-filter steps "
                f"({', '.join(holders)}); keep it in the one step its output reads"
            )
            continue
        options = step.get("with") or {}
        # dorny/paths-filter v4.0.3 (src/filter.ts:58-66,133-149): under the default, `some`, a
        # file matches a filter when any of its patterns matches it, and picomatch reads a `!`
        # pattern as the files outside it; under `some-with-excludes`, when a pattern without `!`
        # matches it and no `!` pattern does. `every` (each pattern matches) is not read.
        quantifier = str(options.get("predicate-quantifier", "some"))
        if quantifier not in PathsFilter.QUANTIFIERS:
            problems.append(
                f"::error::jobs.{needed} runs dorny/paths-filter with predicate-quantifier: "
                f"{quantifier}, which this check cannot evaluate; use the default, some, or "
                f"some-with-excludes to exclude with `!` patterns"
            )
            continue
        patterns = step_filters(step).get(name)
        if not isinstance(patterns, list) or not all(isinstance(p, str) for p in patterns):
            problems.append(
                f"::error::jobs.{needed} filter {name} is not a list of globs, which this "
                f"check cannot read; write it as a list of path globs"
            )
            continue
        negated = [p for p in patterns if p.startswith("!")]
        if quantifier == "some" and negated:
            problems.append(
                f"::error::jobs.{needed} filter {name} has {negated[0]}, a `!` pattern under "
                f"predicate-quantifier: some, where it matches every file outside "
                f"{negated[0][1:]}; give the step predicate-quantifier: some-with-excludes, where "
                f"it excludes those files and a filter with no `!` pattern reads as under some"
            )
            continue
        # picomatch expands {a,b} and {a..c}; glob_regex would read the brace literally.
        unread = unread_filter_patterns(patterns)
        if unread:
            problems.append(
                f"::error::jobs.{needed} filter {name} has {unread[0]}, a brace picomatch expands "
                f"and this check does not read; write one pattern per alternative with `*`, `**`, "
                f"`[...]` and literal characters"
            )
            continue
        found.append((filter_label(needed, name), PathsFilter(patterns, quantifier)))
    return found, admitted


def filter_label(needed: str, name: str) -> str:
    return f"jobs.{needed} filter {name}"


def output_filter(document: dict, needed: str, output: str) -> tuple[str, str] | None:
    """(step id, filter name) that `needs.<needed>.outputs.<output>` reads, or None when the
    output is not a step output."""
    upstream = (document.get("jobs") or {}).get(needed) or {}
    reference = STEP_OUTPUT.search(str((upstream.get("outputs") or {}).get(output, "")))
    return (reference.group(1), reference.group(2)) if reference else None


def paths_filter_steps(job: dict) -> list[dict]:
    return [
        s for s in job.get("steps") or []
        if isinstance(s, dict) and "dorny/paths-filter" in str(s.get("uses", ""))
    ]


def step_filters(step: dict) -> dict:
    """A paths-filter step's filters, by name; empty when they are not inline YAML."""
    filters = yaml.safe_load(str((step.get("with") or {}).get("filters", ""))) or {}
    return filters if isinstance(filters, dict) else {}


documents = {
    path: load(path)
    for path in sorted(WORKFLOWS.glob("*.y*ml"))
    if path.suffix in {".yml", ".yaml"}
}
documents = {path: doc for path, doc in documents.items() if isinstance(doc, dict)}


def calling_jobs(workflow: Path):
    """(caller, its document, job id) for each job that calls `workflow` through
    `workflow_call`; none when `workflow` has no such trigger."""
    if "workflow_call" not in events(documents[workflow]):
        return
    for caller, caller_document in documents.items():
        for job_name, job in (caller_document.get("jobs") or {}).items():
            if isinstance(job, dict) and job.get("uses") == f"./{workflow}":
                yield caller, caller_document, job_name


def unready_triggers(workflow: Path) -> list[tuple[Path, str]]:
    """(workflow, event) for each `pull_request` or `pull_request_target` trigger that runs a
    holder in `workflow`, its own or a `workflow_call` caller's, whose `types` leave out
    `ready_for_review`. No `types` means GitHub's default, which leaves it out too."""
    found = []
    for path in dict.fromkeys([workflow] + [caller for caller, _, _ in calling_jobs(workflow)]):
        for event, spec in events(documents[path]).items():
            if event not in ("pull_request", "pull_request_target"):
                continue
            types = spec.get("types") if isinstance(spec, dict) else None
            types = [types] if isinstance(types, str) else types or DEFAULT_PULL_REQUEST_TYPES
            if "ready_for_review" not in [str(kind) for kind in types]:
                found.append((path, event))
    return found


def deciding_jobs(document: dict, job_name: str) -> list[str]:
    """`job_name` and every job whose `if:` decides whether it runs: each job it transitively
    `needs:`, since GitHub skips a job whose needed job was skipped.

    A job whose own `if:` says `always()` or `!cancelled()` runs past a skipped need, so the
    walk does not descend into its `needs:` — except into a job whose result that same `if:`
    demands. `always() && needs.X.result == 'success'` is exempt from the skip rule and then
    re-imposes X's gate by hand, so X still decides whether this job runs."""
    order, seen, queue = [], set(), [job_name]
    while queue:
        name = queue.pop()
        if name in seen:
            continue
        seen.add(name)
        order.append(name)
        job = (document.get("jobs") or {}).get(name) or {}
        condition = str(job.get("if", ""))
        queue += sorted(required_results(condition))
        if "always()" in condition or "cancelled()" in condition:
            continue
        needs = job.get("needs") or []
        queue += [needs] if isinstance(needs, str) else list(needs)
    return order


def inherited_gates(workflow: Path, job_name: str) -> list[tuple[str, list[tuple[str, PathsFilter]], set[str]]]:
    """(label, filters, admitted events) for every gate that decides whether `job_name` runs:
    the `if:` of each of its deciding_jobs, in `workflow`."""
    document = documents[workflow]
    modes = call_inputs(document)
    gates = []
    for name in deciding_jobs(document, job_name):
        job = (document.get("jobs") or {}).get(name) or {}
        found, admitted = gating_filters(workflow, f"jobs.{name}", job, modes)
        gates.append((f"jobs.{name}.if", found, admitted))
    return gates


def apply_gates(triggers: list, gates: list, workflow: Path) -> list:
    """Drop each trigger whose event a gate does not admit, and AND each gate's filters into the
    rest. Filters inside one `if:` are ||-joined and widen, but separate gates must all hold.
    Each trigger's selectors gain the gates' filters, as (workflow, filter label)."""
    for gate_label, found, admitted in gates:
        for label, event, *_ in triggers:
            if event not in admitted:
                print(f"{label}: not checked, since {gate_label} admits no {event} run")
        triggers = [trigger for trigger in triggers if trigger[1] in admitted]
        if not found:
            continue
        names = " or ".join(label for label, _ in found)
        either = AnyOf([f for _, f in found])
        triggers = [
            (
                f"{label} + {gate_label} ({names})",
                event,
                filters + [either],
                origin,
                selectors + tuple((workflow, name) for name, _ in found),
            )
            for label, event, filters, origin, selectors in triggers
        ]
    return triggers


def triggers_for(workflow: Path) -> list[tuple[str, str, list, Path, tuple]]:
    """(label, event, filters that must all cover an input, the workflow whose trigger it is,
    the paths-filter filters that select it) for every trigger that runs `workflow`. A called
    workflow's `github.event_name` is its caller's event, so a caller's trigger carries the
    caller's event."""
    document = documents[workflow]
    found = [
        (f"{workflow} {label}", event, [f], workflow, ())
        for event, label, f in event_filters(workflow, document)
    ]
    for caller, caller_document, job_name in calling_jobs(workflow):
        # The calling job is gated by its own `if:` and by every job it transitively needs,
        # exactly as a building job is.
        gates = inherited_gates(caller, job_name)
        base = [
            (f"{caller} {label}", event, [caller_filter], caller, ())
            for event, label, caller_filter in event_filters(caller, caller_document)
        ]
        filtered = any(gate_filters for _, gate_filters, _ in gates)
        found += [
            (label if filtered else f"{label} (jobs.{job_name})", *rest)
            for label, *rest in apply_gates(base, gates, caller)
        ]
    return found


builds = []


def scan_step(workflow, job_name, step: dict, gate_step: dict, origin: str, depth: int, prefix: str | None = None) -> None:
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
    # `prefix` is None at job level (depth 0) and the CALLING step's key inside a composite -
    # "" when that caller has no id of its own, which leaves the inner step unlistable rather
    # than letting it answer to a bare id a job-level step could later claim.
    step_id = own if prefix is None else (f"{prefix}/{own}" if prefix and own else "")
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
    texts = tuple(text for text, _ in hits)
    certain = any(anchored for _, anchored in hits)
    shown = ", ".join(repr(text) for text in texts)
    entry = (
        next(
            (e for e in NOT_A_BUILD_STEPS if (e[0], e[1], e[2]) == (str(workflow), job_name, step_id)),
            None,
        )
        if step_id
        else None
    )
    if entry is not None:
        matches.setdefault(entry, []).append(place)
    if certain:
        # No pinned text can vouch for these. The remedy is whatever this check would then
        # accept, and nothing else: a reword cannot turn a real build into a non-build, and
        # `--no-build` is a flag of `compose up` and `compose create` only.
        if any(COMPOSE_TAKES_NO_BUILD.search(text) for text in texts):
            remedy = (
                "add --no-build, and build the image with docker/build-push-action if a "
                "service needs one"
            )
        elif any(COMPOSE_RUN.search(text) for text in texts):
            remedy = (
                "`--no-build` is not a flag of `compose run`, so do the same work another way: "
                "`docker compose up -d --no-build <service> && docker compose exec -T <service> "
                "<command>`, or run the image docker/build-push-action built, `docker run --rm "
                "<image> <command>`"
            )
        elif any(COMPOSE_VERB.search(text) for text in texts):
            remedy = (
                "there is none this check accepts: `compose watch` rebuilds by design and "
                "`--no-up` does not stop it, so a step that runs it has to move out of this "
                "check's reach"
            )
        elif any(EARTHLY.search(text) for text in texts):
            remedy = (
                "there is none this check accepts: it cannot read an Earthfile, so a step that "
                "runs earthly has to move to docker/build-push-action or out of this repository"
            )
        else:
            remedy = "build it with docker/build-push-action so this check can read it"
        if entry is not None:
            problems.append(
                f"::error file={workflow}::{place} is listed in NOT_A_BUILD_STEPS, but it "
                f"cannot be: {shown} is certainly a build, or a compose verb that builds from "
                f"a file this step only names. Remove the entry; {remedy}"
            )
            return
        problems.append(
            f"::error file={workflow}::{place} builds an image in a run step ({shown}); "
            f"{remedy}. This shape cannot be listed in NOT_A_BUILD_STEPS"
        )
        return
    if entry is not None:
        if tuple(entry[3]) == texts:
            excused.append(f"{workflow}: job {job_name}: step {step_id}: {shown} - {entry[4]}")
            return
        was = ", ".join(repr(text) for text in entry[3])
        problems.append(
            f"::error file={workflow}::{place} is listed in NOT_A_BUILD_STEPS, but the entry no "
            f"longer describes it: it was reviewed as running {was or 'nothing'} and now runs "
            f"{shown}. Re-review the step and update its entry"
        )
        return
    if prefix is not None and not prefix:
        naming = (
            "give the CALLING step an `id:` (and this step one), since an inner step is keyed "
            "by caller/inner"
        )
    else:
        naming = "give the step an `id:`"
    problems.append(
        f"::error file={workflow}::{place} builds an image in a run step ({shown}); build it "
        f"with docker/build-push-action so this check can read it, reword the command so it "
        f"does not read as a build (`--flag=value`), or {naming} and add "
        f"({str(workflow)!r}, {job_name!r}, its id, its commands, why it builds nothing) to "
        f"NOT_A_BUILD_STEPS in this script in a reviewed PR"
    )

def scan_local_action(workflow, job_name, action: str, gate_step: dict, place: str, depth: int, prefix: str) -> None:
    """A `./` action is read, not trusted: a composite's steps get the same rules recursively.

    Anything else is refused. A container action's image comes from a Dockerfile this check
    never sees, and a JavaScript action is as opaque as a remote one — `docker/build-push-
    action` is itself JavaScript — so a local `using: node*`, an unknown `using`, and a missing
    `runs.using` are all refused unless NON_BUILDING_ACTIONS names the action."""
    if depth > 5:
        problems.append(f"::error file={workflow}::{place} nests local actions too deeply to read; flatten the "
        f"chain to five levels or fewer")
        return
    definition = next(
        (p for p in (Path(action[2:]) / "action.yml", Path(action[2:]) / "action.yaml") if p.is_file()),
        None,
    )
    if definition is None:
        problems.append(
            f"::error file={workflow}::{place} uses {action}, which has no action.yml this "
            f"check can read; give the action an action.yml, or correct the path"
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
        "::error::no workflow builds an image with docker/build-push-action: this check "
        "covered 0 image builds, so it proves nothing; build the image with that action, or "
        "delete this check"
    )

# Per pull request trigger path that starts a build, its filters, the workflow whose trigger it
# is, the paths-filter filters that select it, and every file the builds it starts read: the
# reverse check below reads them.
PULL_REQUEST_EVENTS = ("pull_request", "pull_request_target")
started: dict[str, tuple[list, Path, tuple, set[str]]] = {}
for workflow, job_name, step, context, dockerfile in builds:
    if not Path(dockerfile).is_file():
        problems.append(f"::error file={workflow}::{workflow} builds {dockerfile}, which is missing; add the "
        f"Dockerfile, or correct the step's file:")
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
    # needs. A gate's event terms drop the triggers whose event it does not admit.
    gates = inherited_gates(workflow, job_name)
    if str(step.get("if", "")):
        where = step.get("name") or "an unnamed step"
        describe = f"jobs.{job_name} step '{where}'"
        found, admitted = gating_filters(workflow, describe, step)
        gates.append((f"{describe} if", found, admitted))
    triggers = apply_gates(triggers_for(workflow), gates, workflow)
    read = {path for _, files in inputs for path in files}
    for label, event, filters, origin, selectors in triggers:
        if event in PULL_REQUEST_EVENTS:
            started.setdefault(label, (filters, origin, selectors, set()))[3].update(read)
    for label, _, filters, _, _ in triggers:
        for name, files in inputs:
            uncovered = [path for path in files if not all(f.covers(path) for f in filters)]
            if uncovered:
                more = f" and {len(uncovered) - 1} more are" if len(uncovered) > 1 else " is"
                problems.append(
                    f"::error file={label.split()[0]}::{label} does not cover {name}: "
                    f"{uncovered[0]}{more} built into the image, so a commit touching only "
                    f"that builds no image; add a path covering it to that trigger"
                )
        print(
            f"{dockerfile}: checked {len(inputs)} inputs "
            f"({sum(len(files) for _, files in inputs)} files) against {label}"
        )
    if not triggers:
        print(f"{dockerfile}: no path-triggered event of {workflow} reaches this build; nothing to check")

building_jobs: dict[Path, set[str]] = {}
for workflow, job_name, *_ in builds:
    building_jobs.setdefault(workflow, set()).add(job_name)

# The steps a job may run and still have an image build as its only purpose: the build itself,
# and steps that prepare or publish it and read no file of the tree.
BUILD_PLUMBING = {
    "actions/checkout",
    "docker/build-push-action",
    "docker/login-action",
    "docker/metadata-action",
    "docker/setup-buildx-action",
    "docker/setup-qemu-action",
}


def local_call(job: dict) -> Path | None:
    called = str(job.get("uses", "")) if isinstance(job, dict) else ""
    target = Path(called[2:]) if called.startswith("./.github/workflows/") else None
    return target if target in documents else None


def only_builds(workflow: Path, seen: frozenset = frozenset()) -> bool:
    """True when every job of the workflow builds an image, calls a workflow that only builds,
    or transitively needs a job that does either, so whatever starts the workflow starts an
    image build and nothing else."""
    jobs = documents[workflow].get("jobs") or {}
    building = set(building_jobs.get(workflow, set()))
    for name, job in jobs.items():
        target = local_call(job)
        if target and target not in seen | {workflow} and only_builds(target, seen | {workflow}):
            building.add(name)

    def reaches(name: str, path: frozenset) -> bool:
        if name in building:
            return True
        needs = (jobs.get(name) or {}).get("needs") or []
        needs = [needs] if isinstance(needs, str) else list(needs)
        return any(need not in path and reaches(need, path | {name}) for need in needs)

    return bool(jobs) and all(reaches(name, frozenset()) for name in jobs)


def builds_only(workflow: Path, job_name: str) -> bool:
    """True for a job whose only purpose is an image build: a call of a workflow that only
    builds, or a building job whose every step is in BUILD_PLUMBING. A run step or any other
    action may read files no build reads, so a job with one does more."""
    job = (documents[workflow].get("jobs") or {}).get(job_name) or {}
    target = local_call(job)
    if target:
        return only_builds(target)
    if job_name not in building_jobs.get(workflow, set()):
        return False
    return all(
        isinstance(step, dict) and str(step.get("uses", "")).split("@", 1)[0] in BUILD_PLUMBING
        for step in job.get("steps") or []
    )


def jobs_selected_by(workflow: Path, label: str) -> list[str]:
    """The jobs of the workflow the paths-filter filter `label` decides: one whose `if:`, or
    that of a job it inherits a gate from (deciding_jobs), or that of one of its steps, tests
    that filter's output. Every test counts, not only `== 'true'`, so the list errs long."""
    document = documents[workflow]
    jobs = document.get("jobs") or {}

    def names(condition) -> set[str]:
        return {
            filter_label(needed, reference[1])
            for needed, output in NEEDS_OUTPUT.findall(str(condition or ""))
            for reference in [output_filter(document, needed, output)]
            if reference
        }

    selected = []
    for job_name, job in jobs.items():
        if not isinstance(job, dict):
            continue
        conditions = [(jobs.get(name) or {}).get("if") for name in deciding_jobs(document, job_name)]
        conditions += [step.get("if") for step in job.get("steps") or [] if isinstance(step, dict)]
        if any(label in names(condition) for condition in conditions):
            selected.append(job_name)
    return selected


# The other direction, for a pull request: a trigger path that starts an image build at every
# push to a pull request whose diff it matches must match nothing the builds it starts do not
# read (a file the .dockerignore drops, or one no COPY names), or a pull request touching only
# such files builds an image for nothing, in the queue the required checks wait in. A path is
# held to this when it starts builds and nothing else: when paths-filter filters select it, each
# of them decides only jobs that only build (builds_only); when none do, the workflow whose
# trigger it is only builds (only_builds). A push, a merge group, a filter that also decides a
# job doing more, and a workflow with other jobs are left alone, since what they start reads
# more than the image. The workflows involved, the building one and the caller, count as read.
for label, (filters, origin, selectors, read) in started.items():
    wider = next(
        (
            (name, job_name)
            for where, name in selectors
            for job_name in jobs_selected_by(where, name)
            if not builds_only(where, job_name)
        ),
        None,
    )
    if wider:
        print(
            f"{label}: not checked for files no image build reads, since {wider[0]} gates "
            f"jobs.{wider[1]}, which does more than build an image"
        )
        continue
    if not selectors and not only_builds(origin):
        continue
    extra = [
        path
        for path in FILES
        if all(f.covers(path) for f in filters) and path not in read and path != str(origin)
    ]
    if not extra:
        print(f"{label}: matches only files its image builds read")
        continue
    more = f" and {len(extra) - 1} more" if len(extra) > 1 else ""
    how = (
        "under predicate-quantifier: some-with-excludes, a `!` pattern excludes"
        if selectors
        else "a `!` pattern after the pattern that matches it excludes"
    )
    problems.append(
        f"::error file={origin}::{label} matches {extra[0]}{more}, which no image build reads, "
        f"so a pull request touching only that builds an image for nothing; narrow the filter "
        f"so it matches only files the build reads ({how})"
    )

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
