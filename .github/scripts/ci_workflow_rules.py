"""The rules about ci.yaml that stand on their own: what no `if:` of it may say, what every job's
`if:` must say, what ci-caches.yaml must save
for the entries ci.yaml restores and which packages its race build must compile, which workspaces
ci_job_reads.py's WORKSPACES must name, and
that no other workflow of this repository runs on a merge group.
None of them
reads a filter, a gate or the closure of a go command, so none belongs in the filter-coverage
engine beside them; check-ci-filters.sh's header numbers them 10, 12, 11, 13, 14 and 16, and
ci_filter_coverage.main()
is what runs them, over the ci.yaml document it already loaded - the workspace rule over the
root manifest and the WORKSPACES table main() hands it, and the race-fill rule over ci-caches.yaml
and the DAEMON_RACE_PACKAGES list main() hands it the same way, since what this module imports is
the engine's names, never the data's, and the merge-group rule over the workflow directory, which
it reads itself.

It sits beside that engine (and workflow_paths.py, the path model) in the repository it checks,
and imports the engine's names for the two workflow files, the repository root, the checks
main's ruleset requires and the three
helpers that read an `if:`, and the path model's reader of a workflow's `on:`. main()
imports this module rather than the other way round, so the one import of the engine here is of
a module Python has finished loading by then.
"""

import json
import re
from collections import defaultdict

import yaml

from ci_filter_coverage import (CACHE_WORKFLOW, REPO, REQUIRED_CHECKS, WORKFLOW, conditions, script_lines, squash,
                                where_if)
from workflow_paths import events


# ---------------------------------------------------------------- the conditions ci.yaml cannot hold
# ci.yaml runs on a pull request, a merge group and a workflow_dispatch, and on no push, so an if:
# testing for a push event, or comparing github.ref with a branch ref, holds its job or step back
# on every run there is. Either way round, as a GitHub expression may be written; its strings are
# single-quoted, a double-quoted one being no expression at all. github.ref_name, and github.ref
# in anything but such a comparison - startsWith against refs/heads/gh-readonly-queue/, the branch
# a merge_group run does carry, among them - are left alone.
def comparison(left: str, right: str) -> str:
    """The regex of a GitHub expression comparing `left` with `right`, either operand first."""
    return rf"{left}\s*[=!]=\s*{right}|{right}\s*[=!]=\s*{left}"


PUSH_CONDITION = re.compile("|".join((comparison(r"github\.event_name", "'push'"),
                                      comparison(r"github\.ref", r"'refs/heads/[^']*'"))))
# Where the work such a condition used to gate lives now.
PUSH_INSTEAD = (f"a cache that must be filled on a push to main belongs in {CACHE_WORKFLOW}, and a job that must "
                f"run at main's tip is started by the workflow_dispatch "
                f".github/workflows/main-bypass-check.yaml sends")


def check_push_conditions(document: dict, problems: list) -> None:
    """A job or a step of ci.yaml gated on a push event, or on github.ref being a branch ref, is
    dead: the workflow has no push trigger, and on its three events github.ref is a pull request's
    merge ref or the merge queue's own branch, never the ref such a condition names. Dead code
    that reads as live is worse than none - a step held back by `if: github.event_name ==
    'push'`, or a job by `if: github.ref == 'refs/heads/main'`, runs in no run of this workflow
    while its text says it runs on main - so a later pull
    request may not bring one back. Read off the if: text, one comparison at a time: a committer
    who writes the same test as a function call is the review's to catch."""
    for job_id, job in (document.get("jobs") or {}).items():
        job = job or {}
        refs = conditions(job)
        for step, condition in refs:
            for found in dict.fromkeys(m.group(0) for m in PUSH_CONDITION.finditer(condition)):
                where = where_if(job_id, step)
                problems.append(f"::error file={WORKFLOW}::{where} has `if: {squash(condition)}`, which tests "
                                f"`{found}`: {WORKFLOW} runs on a pull request, a merge group and a "
                                f"workflow_dispatch, never on a push to main, so nothing that condition gates ever "
                                f"runs; {PUSH_INSTEAD}")


# ------------------------------------------------------- the re-run of a merge-group run
# A group is tested once, so every job of a re-run attempt of a merge-group run must skip. The
# term that does it is a job's own: `gh run rerun --failed` leaves out a `changes` that
# succeeded and hands the attempt that job's outputs again, so an `if:` testing only
# `needs.changes.outputs.<x>` runs the job in full on an attempt the group has no use for, and
# its report step posts a ci-ok on the dead group's head. A job that runs on merge_group alone
# (`cancel-started`) needs only the attempt half, so each job is asked for one term and never
# for both.
#
# A job named for a required check (REQUIRED_CHECKS) is asked for neither, and refused one,
# because a skip is no safe outcome for it: a job an `if:` holds back is skipped, and a skipped
# job's check run is a passing required check, so the term that saves every other job from a dead
# group would hand that group the check it merges on, concluded by nothing. Each of those jobs
# runs on every attempt and refuses an attempt above the first in a step, which fails and makes
# the check a failure; the comment above each one's `if:` in ci.yaml says so.
RERUN_GUARD = ("(github.event_name != 'merge_group' || github.run_attempt == '1')",
               re.compile(r"\(\s*github\.event_name\s*!=\s*'merge_group'\s*\|\|"
                          r"\s*github\.run_attempt\s*==\s*'1'\s*\)"))
MERGE_GROUP_ONLY = re.compile(r"github\.event_name\s*==\s*'merge_group'")
ATTEMPT_ONE = re.compile(r"github\.run_attempt\s*==\s*'1'")


def check_rerun_guard(document: dict, problems: list) -> None:
    """Every job of ci.yaml but the REQUIRED_CHECKS ones must hold, in its own `if:`, the term
    that skips it on a re-run attempt of a merge-group run: the disjunction RERUN_GUARD names,
    or, for a job whose `if:` admits merge_group alone, `github.run_attempt == '1'` by itself. A
    job named for a required check must hold neither, for the reason above: its skip is a passing
    required check. Read off the if: text, as check_push_conditions is: the same test written
    another way is the review's to catch."""
    wanted, found = RERUN_GUARD
    for job_id, job in (document.get("jobs") or {}).items():
        condition = str((job or {}).get("if", ""))
        if job_id in REQUIRED_CHECKS:
            if found.search(condition) or ATTEMPT_ONE.search(condition):
                problems.append(f"::error file={WORKFLOW}::job {job_id} is named for a check main's ruleset "
                                f"requires and its if: holds a term that skips it on a re-run attempt of a "
                                f"merge-group run: a skipped job's check run is a passing required check, so that "
                                f"attempt would merge on {job_id} concluded by nothing; drop the term and refuse "
                                f"the attempt in a step of the job instead")
            continue
        if found.search(condition):
            continue
        if MERGE_GROUP_ONLY.search(condition):
            if not ATTEMPT_ONE.search(condition):
                problems.append(f"::error file={WORKFLOW}::job {job_id} runs on merge_group and its if: has no "
                                f"`github.run_attempt == '1'`, so a re-run attempt of a merge-group run runs it "
                                f"again on a group that is tested already")
            continue
        problems.append(f"::error file={WORKFLOW}::job {job_id}'s if: has no `{wanted}`, so a re-run attempt of a "
                        f"merge-group run runs it again on a group that is tested already; `gh run rerun --failed` "
                        f"does not re-run the `changes` that succeeded, so no job may lean on that job's skip for "
                        f"this")


# ---------------------------------------------------------------- the cache round trip
def cache_family(key: str) -> str:
    """The family a cache key names: its text up to the first expression - the Go version the
    entry is keyed on - without the separator that joins the two."""
    return key.split("${{")[0].strip().rstrip("-")


def path_lines(value) -> list:
    """A cache step's path:, as the ordered list of directories it names."""
    return [line.strip() for line in str(value).splitlines() if line.strip()]


def cache_steps(document: dict, action: str) -> list:
    """Every actions/cache/<action> step of a workflow, as (its job, its family, its with:)."""
    return [(job_id, cache_family(str((step.get("with") or {}).get("key", ""))), step.get("with") or {})
            for job_id, job in (document.get("jobs") or {}).items()
            for step in ((job or {}).get("steps") or [])
            if str((step or {}).get("uses", "")).split("@")[0] == f"actions/cache/{action}"]


def check_cache_round_trip(document: dict, problems: list) -> None:
    """ci-caches.yaml fills on a push to main the entries ci.yaml's jobs restore, so each family's
    save must carry exactly the key and the paths its restores name: a key or a path that drifts
    apart makes every restore miss, and nothing fails - each job just rebuilds from nothing what
    the fill already built. A family is the key's text up to its first expression, and it is what
    pairs a save with the restores to hold it to; the keys are compared as text and the paths as
    the ordered lists of lines they name. A family ci.yaml restores that this workflow does not
    save is no fault: docs-pages.yaml saves go-docs. The other way round is one, a family nothing
    restores being a fill nobody reads. Each restore here is a lookup, which must be
    `lookup-only: true` with no restore-keys: the question it asks is whether that exact key is
    already there, the entry it would download is read by nothing in this workflow, and a prefix
    hit would answer for an older key and skip the build that fills this one. A ci-caches.yaml
    that is not in the repository saves nothing, and so has nothing to hold."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return
    caches = yaml.safe_load((REPO / CACHE_WORKFLOW).read_text()) or {}
    restores = defaultdict(list)
    for job_id, family, given in cache_steps(document, "restore"):
        restores[family].append((job_id, given))
    for job_id, family, given in cache_steps(caches, "save"):
        key, paths = str(given.get("key", "")).rstrip(), path_lines(given.get("path", ""))
        if family not in restores:
            problems.append(f"::error file={CACHE_WORKFLOW}::job {job_id} saves cache family {family}, which no "
                            f"actions/cache/restore of {WORKFLOW} reads; this workflow exists to fill what "
                            f"{WORKFLOW} restores, so a family nothing restores is a fill nobody reads: restore it "
                            f"there, or drop the save")
            continue
        for restore_job, restored in restores[family]:
            restored_key = str(restored.get("key", "")).rstrip()
            restored_paths = path_lines(restored.get("path", ""))
            if key != restored_key:
                problems.append(f"::error file={CACHE_WORKFLOW}::job {job_id} saves family {family} under key "
                                f"`{key}`, but job {restore_job} of {WORKFLOW} restores it under "
                                f"`{restored_key}`: a restore whose key is not the save's "
                                f"misses every time, and nothing fails - the job rebuilds from nothing instead")
            if paths != restored_paths:
                problems.append(f"::error file={CACHE_WORKFLOW}::job {job_id} saves family {family} from "
                                f"{', '.join(paths) or '(no path)'}, but job {restore_job} of {WORKFLOW} restores "
                                f"it into {', '.join(restored_paths) or '(no path)'}: the "
                                f"entry then holds other directories than the job reads, and the job rebuilds what "
                                f"the fill already built")
    for job_id, family, given in cache_steps(caches, "restore"):
        if str(given.get("lookup-only", "")).strip().lower() != "true":
            problems.append(f"::error file={CACHE_WORKFLOW}::job {job_id}'s actions/cache/restore of family "
                            f"{family} is not `lookup-only: true`: it asks only whether that exact key is already "
                            f"there, and the entry a restore downloads is read by no step of this workflow")
        if given.get("restore-keys") is not None:
            problems.append(f"::error file={CACHE_WORKFLOW}::job {job_id}'s actions/cache/restore of family "
                            f"{family} has restore-keys: a prefix hit would answer for an older key, and the build "
                            f"that fills this one would be skipped, leaving the key empty")


# ------------------------------------------- the race build that fills the race lane's cache
# What makes a line of ci-caches.yaml the race build this rule holds: a `go test` running with
# -race. Spelled as the engine's tag-run triggers are, so `-racy` is not the flag and a go vet or
# go build line is no such command, and read through script_lines, so a comment is no trigger. The
# packages are the words beginning `./`, which is how a go command names one and how
# ci_job_reads.py's DAEMON_RACE_PKGS spells that same list; the `-o /dev/null` of the build is no
# package. TABLE, the file the list is in, is the workspaces section's constant below: both rules
# name that one file, so there is one spelling of it here.
RACE_TEST = re.compile(r"\bgo\s+(?:-C\s+\S+\s+)?test\b")
RACE_FLAG = re.compile(r"(?<![\w-])-race\b")
RACE_LIST = "DAEMON_RACE_PACKAGES"


def race_builds(document: dict) -> list:
    """Every race build of a workflow: (its job, its line, the packages that line names)."""
    return [(job_id, line, [word for word in line.split() if word.startswith("./")])
            for job_id, job in (document.get("jobs") or {}).items()
            for step in ((job or {}).get("steps") or [])
            for line in script_lines(str((step or {}).get("run", "")))
            if RACE_TEST.search(line) and RACE_FLAG.search(line)]


def check_race_fill_packages(packages: list, problems: list) -> None:
    """ci-caches.yaml's race build compiles exactly the packages the daemon's race lane runs. The
    workflow fills on a push to main the build cache ci.yaml's race step restores, so the two
    sides must name one set: a package the fill compiles and the lane does not leaves the entry
    holding a race binary no job ever restores, and a package the lane runs that the fill leaves
    out is linked from nothing in every run of that step. Neither fails anything - it is the
    silence check_cache_round_trip is about one level up, where the entry is found and holds the
    wrong objects - and the command has drifted twice for want of this rule: the fill kept
    ./internal/record/ after #2867 took it out of the lane. The packages are held as a set and in
    order, each spelled as a command spells it (./internal/daemon/), since a command that reads in
    another order than the list is one a reader must sort before comparing, and an unreadable pair
    is where the next drift hides. A workflow with no race build at all fails too, rather than
    passing quietly: this rule exists to hold that one command, so a tree without it is a lane
    nothing fills, not a rule with nothing to do. A ci-caches.yaml that is not in the repository
    fills nothing, and so has nothing to hold.

    The list is an argument, as check_workspace_table's is: main() holds ci_job_reads.py already,
    and nothing here imports it - the engine's names are all this module takes."""
    if not (REPO / CACHE_WORKFLOW).is_file():
        return
    caches = yaml.safe_load((REPO / CACHE_WORKFLOW).read_text()) or {}
    wanted = [f"./{package}/" for package in packages]
    builds = race_builds(caches)
    if not builds:
        problems.append(f"::error file={CACHE_WORKFLOW}::no step of this workflow runs a `go test` with -race, so "
                        f"nothing fills the build cache {WORKFLOW}'s race step restores and that step links "
                        f"{', '.join(wanted) or '(no package)'} from nothing on every run; put the race build "
                        f"back, or drop this rule with the lane it holds")
        return
    for job_id, line, named in builds:
        where = f"job {job_id}'s race build"
        extra = [package for package in named if package not in wanted]
        missing = [package for package in wanted if package not in named]
        for package in extra:
            problems.append(f"::error file={CACHE_WORKFLOW}::{where} names {package}, which {TABLE}'s {RACE_LIST} "
                            f"does not ({', '.join(named)} against {', '.join(wanted) or '(no package)'}): this "
                            f"build fills the cache {WORKFLOW}'s race step restores, so it compiles a race binary "
                            f"no job of {WORKFLOW} restores, and nothing fails; either the command follows the "
                            f"list, or the package joins {RACE_LIST}, the one place the lane is named - "
                            f"{WORKFLOW}'s two race steps follow it already")
        for package in missing:
            problems.append(f"::error file={CACHE_WORKFLOW}::{TABLE}'s {RACE_LIST} names {package}, which {where} "
                            f"does not ({', '.join(wanted)} against {', '.join(named) or '(no package)'}): the "
                            f"fill then misses a race binary {WORKFLOW}'s race step links from nothing on every "
                            f"run, and nothing fails; either the command follows the list, or the package leaves "
                            f"{RACE_LIST}, the one place the lane is named - {WORKFLOW}'s two race steps follow "
                            f"it already")
        if named != wanted and not extra and not missing:
            problems.append(f"::error file={CACHE_WORKFLOW}::{where} names {', '.join(named)}, where {TABLE}'s "
                            f"{RACE_LIST} has {', '.join(wanted)}: the same packages in another order, so the "
                            f"fill is right and only a reader who sorts both lists can see that it is; spell the "
                            f"command as the list reads, so the next change to either shows as a difference")


# ---------------------------------------------------------------- the workspaces and the table
# The root manifest, whose workspaces ci_job_reads.py's WORKSPACES must name one for one, and the
# file that table is in. Both are named in the errors below, each in the one it is about.
MANIFEST = "package.json"
TABLE = ".github/scripts/ci_job_reads.py"


def check_workspace_table(workspaces: list, problems: list) -> None:
    """The root manifest's workspaces and ci_job_reads.py's WORKSPACES name the same packages.
    A frozen `bun install` reads every workspace manifest, so bun_job() declares each of
    WORKSPACES as a read of every Bun job, and the engine holds every filter gating such a job to
    all of them. A workspace the table leaves out is therefore a manifest no filter has to select,
    and a change to that package starts no job at all - which is how packages/broker-client came
    to have its typecheck and its tests run nowhere. The other way round is a read every Bun job
    declares of a manifest no install reads. The table is an argument because main() holds it
    already, and because nothing here imports ci_job_reads: the engine's names are all this
    module takes.

    The manifest is read as .github/scripts/check-bun-workspace-manifests.sh:103-111 reads it,
    at every shape, since the two checks are about the same list and a list one of them cannot
    read is not a list the other may pass over: a root manifest that is missing or is not valid
    JSON, and a `workspaces` that is not a list or holds anything but non-empty strings, are
    named rather than taken for an empty list, which would hold the table to nothing - and would
    do it exactly where every read WORKSPACES feeds is wrong anyway, since a tree with no root
    manifest runs no `bun install` the table could describe. An empty array is accepted, as that
    script accepts it: a root that declares no workspace is consistent with a table that names
    none, and a table that names one is caught below as an entry the manifest does not list. A
    trailing slash is stripped on both sides, since `packages/x/` and `packages/x` are one
    workspace to bun, and a rule that stripped it on one side alone would report the table as
    missing a workspace it names in the other spelling."""
    if not (REPO / MANIFEST).is_file():
        problems.append(f"::error file={MANIFEST}::the root manifest is missing, so {TABLE}'s WORKSPACES is held to "
                        f"nothing while every read it gives each Bun job names a manifest this tree does not have")
        return
    try:
        manifest = json.loads((REPO / MANIFEST).read_text())
    except json.JSONDecodeError as error:
        problems.append(f"::error file={MANIFEST}::the root manifest is not valid JSON ({error.msg} at line "
                        f"{error.lineno}), so its workspaces cannot be held to {TABLE}'s WORKSPACES")
        return
    found = manifest.get("workspaces") if isinstance(manifest, dict) else None
    if not isinstance(found, list) or not all(isinstance(w, str) and w for w in found):
        problems.append(f"::error file={MANIFEST}::workspaces must be an array of workspace paths for "
                        f"{TABLE}'s WORKSPACES to be held to it")
        return
    declared = [workspace.removesuffix("/") for workspace in found]
    table = [workspace.removesuffix("/") for workspace in workspaces]
    for workspace in declared:
        if workspace in table:
            continue
        problems.append(f"::error file={MANIFEST}::workspaces names {workspace}, which {TABLE}'s WORKSPACES does "
                        f"not: a frozen `bun install` reads that manifest in every Bun job, so until the table "
                        f"names it no filter has to select it, and a change to the package starts no job at all")
    for workspace in table:
        if workspace in declared:
            continue
        problems.append(f"::error file={TABLE}::WORKSPACES names {workspace}, which the root manifest's workspaces "
                        f"does not: every Bun job then declares a read of {workspace}/package.json that no install "
                        f"of this repository reads")


# ------------------------------------------------- the one workflow a merge group runs
# The directory every workflow of this repository is in, and the two suffixes GitHub reads there.
WORKFLOW_DIR = ".github/workflows"
WORKFLOW_SUFFIXES = (".yaml", ".yml")
MERGE_GROUP = "merge_group"


def check_one_merge_group_workflow(problems: list) -> None:
    """ci.yaml is the only workflow of this repository that runs on a merge group.

    What holds it there is ci-ok-report.sh: it POSTs the failing ci-ok of a merge-group run the
    reporting job's own result decides, rather than waiting for the ci-ok job, so the queue
    learns the group is doomed while the rest of the run is still going. A POSTed check run
    joins the Actions App's first check suite at that commit rather than the posting run's
    (measured on pull request #2860 at 57182d42), and a ci-ok in a suite the queue does not read
    is a ci-ok the queue does not have: #2847 was refused with `Required status check "ci-ok" is
    expected` while a green ci-ok sat in PR Title's suite. That POST therefore lands where the
    queue reads it only because ci.yaml is
    the one workflow a merge group starts, which makes its suite the first one. A second
    merge_group workflow puts the early POST back in another suite, and nothing in ci.yaml would
    say so: the workflow that broke it is the new file. So the rule is about every other
    workflow, and names the file it found."""
    for path in sorted(p for p in (REPO / WORKFLOW_DIR).iterdir() if p.suffix in WORKFLOW_SUFFIXES):
        name = f"{WORKFLOW_DIR}/{path.name}"
        if name == WORKFLOW or MERGE_GROUP not in events(yaml.safe_load(path.read_text()) or {}):
            continue
        problems.append(f"::error file={name}::{name} runs on {MERGE_GROUP}, which only {WORKFLOW} may: "
                        f"ci-ok-report.sh POSTs the failing ci-ok of a merge-group run the reporting job's own "
                        f"result decides, and a POSTed check run joins the Actions App's first check suite at that "
                        f"commit rather than the posting run's, so a second {MERGE_GROUP} workflow can hold that "
                        f"first suite and the queue never reads the POST - measured on pull request #2847, which "
                        f"the queue refused to enqueue with `Required status check \"ci-ok\" is expected` while a "
                        f"green ci-ok sat in another workflow's check suite. Run this work on pull_request, or "
                        f"move it into {WORKFLOW}")
