#!/usr/bin/env bash
# The release workflows keep every credential away from the code the releases are built from. Each
# release is a build job, which runs the dependencies' code (install, pack, build, test, smoke) and
# holds no write scope and no secret, and a publish job, which holds the credentials and runs no
# dependency code: only release-push.sh, git, gh, jq, sha256sum, npm and a handful of pinned
# actions, with no cache restored and the deploy key in the env of the one or two steps that push.
# This check pins that shape, so a later edit cannot undo it quietly:
#
# 1. A credentialed job — one whose `permissions` grants contents, id-token or attestations: write
#    (the workflow's `permissions` when the job declares none, as GitHub gives it), or whose text
#    anywhere names `secrets.DEPLOY_KEY` — (a) runs no dependency code, (b) uses only the actions
#    on CREDENTIALED_USES below, (c) sets `cache-mode: none`, and (d) names the deploy key only as
#    `DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}` in the step-level `env:` of a step whose `run:` calls
#    `release-push.sh push-tag` or `push-main`.
# 2. A build job — one that runs dependency code — holds none of those three write scopes and no
#    `secrets.DEPLOY_KEY` text, and its last step is exactly `run: .github/scripts/release-push.sh
#    no-credential`, with no `if:`, so the tripwire always runs.
# 3. Every `actions/checkout` sets `persist-credentials: false` (the boolean), so no token is left
#    in the checkout for a later step.
# 4. release.yaml's pi_legion_publish needs pi_envoy_publish: both end in `release-push.sh
#    push-main`, whose rebase repair handles one package's bump at a time.
# 5. release-envoy-listener.yaml's release_publish runs in the concurrency group
#    `release-envoy-listener` with `cancel-in-progress: false`.
#
# "Runs dependency code" is a tripwire over named patterns, not a proof that no other code runs: a
# step runs dependency code when its `uses:` starts with one of DEPENDENCY_USES, or when a word in
# command position of its `run:` is one of DEPENDENCY_COMMANDS, is `docker build` / `docker buildx
# build`, or is a path with a `scripts/` or `out/` component (`./scripts/x`, `../../scripts/x`,
# `out/x`, `"$GITHUB_WORKSPACE/out/x"`), other than .github/scripts/release-push.sh itself. Command
# position is found by splitting each line of the `run:` on the shell's separators (`;`, `&&`,
# `||`, `|`, `(`, `{`, `$(` and backticks; `${{ … }}` expressions are elided first), dropping
# comments, leading `VAR=value` assignments and the keywords that precede a command (if, then, for,
# do, time, exec, env, …), and taking the first word left. A variable in command position
# (`"$tool" build`), a command behind `sh -c '…'`, `xargs` or `nohup`, or a program not on the list
# passes here; the job-level tripwire (`release-push.sh no-credential`) and review hold the rest.
#
# Scope: exactly the three release workflows named in WORKFLOWS, every job that has `steps:`. A job
# that calls a reusable workflow (`uses:` and no steps, release.yaml's worker_image) is out of
# scope: what it runs is that workflow's own jobs.
#
# Run from anywhere: .github/scripts/check-release-credentials.sh
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
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where each key was written."""

    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

WORKFLOWS = [
    Path(".github/workflows/release.yaml"),
    Path(".github/workflows/release-envoy-listener.yaml"),
    Path(".github/workflows/release-envoy-infra.yaml"),
]

# What makes a job credentialed: one of these scopes, or the deploy key's name anywhere in it.
WRITE_SCOPES = ("contents", "id-token", "attestations")
DEPLOY_KEY_TEXT = "secrets.DEPLOY_KEY"
DEPLOY_KEY_VALUE = re.compile(r"\$\{\{\s*secrets\.DEPLOY_KEY\s*\}\}")
RELEASE_PUSH = ".github/scripts/release-push.sh"
TRIPWIRE = f"{RELEASE_PUSH} no-credential"
PUSH_COMMANDS = ("release-push.sh push-tag", "release-push.sh push-main")

# What makes a step run dependency code.
DEPENDENCY_COMMANDS = {
    "bun", "bunx", "go", "node", "npx", "pulumi", "mise", "make",
    "python", "python3", "pip", "uv", "cargo", "sudo",
}
DEPENDENCY_USES = (
    "actions/setup-go",
    "./.github/actions/setup-bun",
    "oven-sh/setup-bun",
    "./.github/actions/install-",
    "docker/build-push-action",
    "docker/setup-qemu-action",
)
DEPENDENCY_PATH = re.compile(r"(^|/)(scripts|out)/")
# What a word sheds to become a path relative to the checkout: $GITHUB_WORKSPACE/, ./ and ../.
PATH_PREFIX = re.compile(r"^(\$\{?GITHUB_WORKSPACE\}?/)?(\.\.?/)*")

# The only actions a credentialed job may use, compared before the `@`.
CREDENTIALED_USES = {
    "actions/checkout",
    "actions/download-artifact",
    "actions/setup-node",
    "actions/attest-build-provenance",
    "docker/login-action",
}

# Just enough shell to find each simple command's first word.
EXPRESSION = re.compile(r"\$\{\{.*?\}\}")
SEPARATORS = re.compile(r"\|\||&&|;|\||\$\(|\(|(?<!\$)\{|`")
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\+?=")
SKIP_WORDS = {
    "if", "then", "else", "elif", "fi", "do", "done", "while", "until", "for", "in",
    "time", "exec", "env", "!", "{", "}", "case", "esac",
}


def strip_comment(segment: str) -> str:
    """Cuts an unquoted ` #` and everything after it."""
    quote = None
    i = 0
    while i < len(segment):
        char = segment[i]
        if quote:
            if char == quote:
                quote = None
            elif char == "\\" and quote == '"':
                i += 1
        elif char in "'\"":
            quote = char
        elif char == "\\":
            i += 1
        elif char == "#" and i > 0 and segment[i - 1].isspace():
            return segment[:i]
        i += 1
    return segment


def words_of(segment: str) -> list[str]:
    """Splits on unquoted whitespace and drops the quotes. A quote the separator split left open
    runs to the end of the segment."""
    words: list[str] = []
    word = ""
    started = False
    quote = None
    i = 0
    while i < len(segment):
        char = segment[i]
        if quote:
            if char == quote:
                quote = None
            elif char == "\\" and quote == '"' and i + 1 < len(segment):
                i += 1
                word += segment[i]
            else:
                word += char
        elif char in "'\"":
            quote = char
            started = True
        elif char == "\\" and i + 1 < len(segment):
            i += 1
            word += segment[i]
            started = True
        elif char.isspace():
            if started:
                words.append(word)
            word = ""
            started = False
        else:
            word += char
            started = True
        i += 1
    if started:
        words.append(word)
    return words


def commands_of(run: str) -> list[list[str]]:
    """Each simple command of a `run:`, as its words with the command first."""
    commands = []
    for line in EXPRESSION.sub("EXPR", run).splitlines():
        for segment in SEPARATORS.split(line):
            segment = segment.strip()
            if not segment or segment.startswith("#"):
                continue
            words = words_of(strip_comment(segment))
            while words and (ASSIGNMENT.match(words[0]) or words[0] in SKIP_WORDS):
                words.pop(0)
            if words:
                commands.append(words)
    return commands


def dependency_command(words: list[str]) -> str | None:
    """What the command runs when it is dependency code, else None."""
    command = words[0]
    if command in DEPENDENCY_COMMANDS:
        return command
    if command == "docker":
        if words[1:2] == ["build"]:
            return "docker build"
        if words[1:3] == ["buildx", "build"]:
            return "docker buildx build"
        return None
    path = PATH_PREFIX.sub("", command)
    if DEPENDENCY_PATH.search(path) and path != RELEASE_PUSH:
        return command
    return None


def dependency_code(step: Mapping) -> str | None:
    """How the step runs dependency code (`runs 'bun'`, `uses 'actions/setup-go@…'`), or None."""
    found = []
    uses = step.get("uses")
    if isinstance(uses, str) and uses.startswith(DEPENDENCY_USES):
        found.append(f"uses '{uses}'")
    run = step.get("run")
    if isinstance(run, str):
        commands = []
        for words in commands_of(run):
            what = dependency_command(words)
            if what and what not in commands:
                commands.append(what)
        if commands:
            found.append("runs " + ", ".join(f"'{what}'" for what in commands))
    return " and ".join(found) or None


def write_scopes(job: Mapping, workflow: Mapping) -> list[str]:
    """The write scopes the job holds: its own `permissions`, or, when it declares none, the
    workflow's, which GitHub gives a job that has no block of its own."""
    permissions = job["permissions"] if "permissions" in job else workflow.get("permissions")
    if permissions == "write-all":
        return list(WRITE_SCOPES)
    if not isinstance(permissions, Mapping):
        return []
    return [scope for scope in WRITE_SCOPES if permissions.get(scope) == "write"]


def deploy_key_sites(node, path=(), line=0) -> list[tuple[tuple, int, object]]:
    """Every key and value under node whose text names secrets.DEPLOY_KEY: (path, line, value)."""
    sites = []
    if isinstance(node, Mapping):
        for key, value in node.items():
            key_line = node.lines.get(key, line)
            if DEPLOY_KEY_TEXT in str(key):
                sites.append((path + (key,), key_line, key))
            sites += deploy_key_sites(value, path + (key,), key_line)
    elif isinstance(node, list):
        for index, item in enumerate(node):
            sites += deploy_key_sites(item, path + (index,), line)
    elif DEPLOY_KEY_TEXT in str(node):
        sites.append((path, line, node))
    return sites


def deploy_key_allowed(steps: list, path: tuple, value) -> bool:
    """`DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}` in the env of a step that pushes, and nothing else."""
    if len(path) != 4 or path[0] != "steps" or path[2:] != ("env", "DEPLOY_KEY"):
        return False
    if not isinstance(value, str) or not DEPLOY_KEY_VALUE.fullmatch(value.strip()):
        return False
    run = steps[path[1]].get("run")
    return isinstance(run, str) and any(push in run for push in PUSH_COMMANDS)


def describe(step: Mapping, index: int) -> str:
    if isinstance(step.get("name"), str):
        return f"step '{step['name']}'"
    if isinstance(step.get("uses"), str):
        return f"step '{step['uses']}'"
    return f"step {index + 1}"


def describe_site(steps: list, path: tuple) -> str:
    if len(path) >= 2 and path[0] == "steps" and isinstance(path[1], int):
        rest = ".".join(str(part) for part in path[2:])
        return f"{describe(steps[path[1]], path[1])} {rest}".rstrip()
    return ".".join(str(part) for part in path)


def line_of(step: Mapping, fallback: int) -> int:
    return min(step.lines.values(), default=fallback)


problems: list[str] = []
credentialed_jobs = 0
build_jobs = 0


def problem(path: Path, line: int, message: str) -> None:
    problems.append(f"{path}:{line}: {message}")


def check_job(
    path: Path, name: str, job: Mapping, job_line: int, steps: list, workflow: Mapping
) -> None:
    global credentialed_jobs, build_jobs
    if not all(isinstance(step, Mapping) for step in steps):
        problem(
            path, job.lines.get("steps", job_line), f"job '{name}' has a step that is not a mapping"
        )
        return
    scopes = write_scopes(job, workflow)
    key_sites = deploy_key_sites(job, line=job_line)
    dependency = [
        (index, step, found) for index, step in enumerate(steps) if (found := dependency_code(step))
    ]

    # 3. Every checkout leaves no token behind, whatever the job holds.
    for index, step in enumerate(steps):
        uses = step.get("uses")
        if not (isinstance(uses, str) and uses.startswith("actions/checkout@")):
            continue
        with_ = step.get("with")
        if isinstance(with_, Mapping) and "persist-credentials" in with_:
            value = with_["persist-credentials"]
            if value is False:
                continue
            line = with_.lines.get("persist-credentials", line_of(step, job_line))
            found = f"found {json.dumps(value)}"
            if isinstance(value, str):
                found += ", a string"
        else:
            line = line_of(step, job_line)
            found = "it is not set"
        problem(
            path, line,
            f"job '{name}' {describe(step, index)} must set 'persist-credentials: false', the "
            f"boolean, so no token is left in the checkout ({found})",
        )

    if scopes or key_sites:
        credentialed_jobs += 1
        # 1a. No dependency code.
        for index, step, found in dependency:
            problem(
                path, line_of(step, job_line),
                f"job '{name}' holds a credential, but {describe(step, index)} {found}; "
                f"dependency code runs only in a job that holds none",
            )
        # 1b. Only the allow-listed actions.
        for index, step in enumerate(steps):
            uses = step.get("uses")
            if isinstance(uses, str) and uses.split("@", 1)[0] not in CREDENTIALED_USES:
                problem(
                    path, step.lines.get("uses", line_of(step, job_line)),
                    f"job '{name}' holds a credential, but {describe(step, index)} uses '{uses}', "
                    f"which is not on the checker's allow-list for a job that holds a credential; "
                    f"add it there deliberately",
                )
        # 1c. No cache restored.
        if job.get("cache-mode") != "none":
            found = (
                f"found {json.dumps(job['cache-mode'])}" if "cache-mode" in job else "it is not set"
            )
            problem(
                path, job.lines.get("cache-mode", job_line),
                f"job '{name}' holds a credential, so it must set 'cache-mode: none' to restore "
                f"no cache ({found})",
            )
        # 1d. The deploy key reaches only the steps that push.
        for key_path, line, value in key_sites:
            if not deploy_key_allowed(steps, key_path, value):
                problem(
                    path, line,
                    f"job '{name}' names secrets.DEPLOY_KEY at {describe_site(steps, key_path)}; "
                    f"the deploy key may appear only as 'DEPLOY_KEY: ${{{{ secrets.DEPLOY_KEY }}}}' "
                    f"in the env of a step whose run calls release-push.sh push-tag or push-main",
                )

    if dependency:
        build_jobs += 1
        index, step, found = dependency[0]
        what = f"{describe(step, index)} {found}"
        # 2. No credential, and the tripwire last.
        for scope in scopes:
            line = job.lines.get("permissions") or workflow.lines.get("permissions", job_line)
            held = f"holds '{scope}: write'"
            if "permissions" not in job:
                held += " through the workflow's permissions, having none of its own"
            problem(path, line, f"job '{name}' runs dependency code ({what}), but {held}")
        if key_sites:
            problem(
                path, key_sites[0][1],
                f"job '{name}' runs dependency code ({what}), but its text names "
                f"secrets.DEPLOY_KEY",
            )
        last = steps[-1]
        run = last.get("run")
        if not (isinstance(run, str) and run.strip() == TRIPWIRE and "uses" not in last):
            problem(
                path, line_of(last, job_line),
                f"job '{name}' runs dependency code ({what}), so its last step must be the "
                f"tripwire 'run: {TRIPWIRE}'",
            )
        elif "if" in last:
            problem(
                path, last.lines.get("if", line_of(last, job_line)),
                f"job '{name}' runs dependency code ({what}), so its tripwire step must carry "
                f"no 'if:'",
            )


loaded: dict[Path, tuple[Mapping, Mapping]] = {}
for path in WORKFLOWS:
    try:
        text = path.read_text()
    except FileNotFoundError:
        problems.append(f"{path} is missing")
        continue
    try:
        doc = yaml.load(text, Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"{path} is not valid YAML: {error}")
        continue
    jobs = doc.get("jobs") if isinstance(doc, Mapping) else None
    if not isinstance(jobs, Mapping):
        problems.append(f"{path}:1: has no jobs mapping")
        continue
    loaded[path] = (doc, jobs)
    for name, job in jobs.items():
        if not isinstance(job, Mapping) or not isinstance(job.get("steps"), list):
            continue
        check_job(path, str(name), job, jobs.lines.get(name, 1), job["steps"], doc)


def required_job(path: Path, name: str, must: str) -> tuple[Mapping, int] | None:
    """The named job and its line, or None after reporting that the workflow lacks it."""
    doc, jobs = loaded[path]
    job = jobs.get(name)
    if isinstance(job, Mapping):
        return job, jobs.lines.get(name, 1)
    problem(path, doc.lines.get("jobs", 1), f"no job '{name}', which {must}")
    return None


# 4. The two npm publishes that push a bump to main run one after the other.
if WORKFLOWS[0] in loaded:
    path = WORKFLOWS[0]
    must = "must need 'pi_envoy_publish', so the two bump commits to main never race"
    if found := required_job(path, "pi_legion_publish", must):
        job, job_line = found
        needs = job.get("needs")
        needs_list = [needs] if isinstance(needs, str) else needs if isinstance(needs, list) else []
        if "pi_envoy_publish" not in needs_list:
            problem(
                path, job.lines.get("needs", job_line),
                f"job 'pi_legion_publish' {must} (found needs: {json.dumps(needs)})",
            )

# 5. The listener's publish job moves `latest` and tags one run at a time, and finishes.
if WORKFLOWS[1] in loaded:
    path = WORKFLOWS[1]
    must = (
        "must run in concurrency group 'release-envoy-listener' with 'cancel-in-progress: false', "
        "the boolean"
    )
    if found := required_job(path, "release_publish", must):
        job, job_line = found
        concurrency = job.get("concurrency")
        group = concurrency.get("group") if isinstance(concurrency, Mapping) else None
        cancel = concurrency.get("cancel-in-progress") if isinstance(concurrency, Mapping) else None
        if group != "release-envoy-listener" or cancel is not False:
            problem(
                path, job.lines.get("concurrency", job_line),
                f"job 'release_publish' {must} (found concurrency: {json.dumps(concurrency)})",
            )

if problems:
    for entry in problems:
        print(f"::error::{entry}")
    print(f"{len(problems)} release credential problem(s)", file=sys.stderr)
    sys.exit(1)

print(
    f"check-release-credentials: {credentialed_jobs} credentialed job(s) and {build_jobs} build "
    f"job(s) across {len(WORKFLOWS)} release workflows keep credentials out of jobs that run "
    f"dependency code"
)
PY
