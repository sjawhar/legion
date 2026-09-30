#!/usr/bin/env bash
# The report-only window's numbers and its day-14 decision (AGENTC-1305, Task 6 of the security
# plan). The Security workflow's daily scheduled job runs it (.github/workflows/security.yaml,
# `Report and decide`); anyone can run it from a checkout:
#
#   scripts/security-report.sh [--repo owner/repo] [--window-days 14] [--decision force|none]
#   scripts/security-report.sh --window-flags .github/security-window.json [--at REV]
#   scripts/security-report.sh --base-commit EVENT [MERGE_GROUP_BASE_SHA]
#
# .github/security-window.json holds one report-only flag per check,
# {"report_only": {"zizmor": true, "dependencies": true}}: while a check's flag is true its Gate
# step runs under continue-on-error. A check is blocking only where the file sets its flag to
# false: a missing or unreadable file, a document that is not an object with a report_only key,
# a missing key and a value that is not true or false each read as report-only, and each is named
# in a note. A bare boolean ({"report_only": true}, the file's first form) is that value for every
# check. --window-flags prints the file's flags as `zizmor=<bool>` and `dependencies=<bool>` lines
# (the window job appends them to $GITHUB_OUTPUT) and its notes on stderr, and exits 0; with --at
# it reads the file as it is at commit REV (fetched from origin at depth 1 when absent), which is
# how a pull request or merge group reads its base's flags rather than its own: a promotion takes
# effect on main from its merge, its own pull request's run stays report-only, and a pull request
# cannot make its own check report-only again by editing the file.
#
# --base-commit prints the commit a run's head is judged against, run from the checked-out tree:
# for pull_request the first parent of HEAD, which is a merge commit GitHub builds on the base
# branch's current tip, so its first parent is that tip (the event's pull_request.base.sha is the
# base when the pull request was opened or last pushed, and does not follow main); for
# merge_group the event's base_sha; for any other event nothing.
#
# The window opens at the first `push` run of the Security workflow on main (the merge that added
# it) and lasts --window-days. Once it has closed, or with --decision force, the report ends in a
# DECISION block with a line per check: the rule's PROMOTE or HOLD while that check is still
# report-only on main, `already blocking` once its flag is false. While any check is still
# report-only the block makes the exit code 1, which fails the scheduled run so its status badge
# turns red and the window watcher wakes the owner of the decision. With every check blocking it
# prints the numbers and no decision (unless forced), and exits 0.
#
# Decision rules (Task 6), each enforced by the DECISION block:
# 1. **zizmor → blocking** iff (a) the newest `main` run's `head_count` is 0 with the checked-in
#    `.github/zizmor.yml` and inline ignores, and (b) over the window, precision = fixed ÷ (fixed +
#    ignored) ≥ 0.7 over the *new-against-base* findings of merged PRs, when ≥ 5 such findings were
#    dispositioned; with fewer, (a) alone decides. **Kill:** an audit whose new findings were
#    ignored ≥ 3 times in the window is disabled for those inputs rather than tuned — for workflows
#    through `rules.<audit>.ignore` in `.github/zizmor.yml`, for composite actions through inline
#    `# zizmor: ignore[<audit>]` comments.
# 2. **osv-scanner / govulncheck → blocking** (osv findings with a fixed version; govulncheck
#    symbol-level findings) iff 0 runs in the window recorded `tool_error`; otherwise stay
#    report-only and switch osv-scanner to offline databases before re-deciding.
# 3. **CodeQL → add the `pull_request` trigger** iff ≥ 1 CodeQL alert on `main` reached `state:
#    fixed` inside the window (`code-scanning/alerts`, `fixed_at`); if none, or every alert was
#    dismissed, stay `main`-only and record it. Computed in CI, where the token has
#    `security-events: read`; `BLOCKED` on the devbox.
# 4. **Rubric rows:** a tag with ≥ 3 `Accepted: not a defect` and 0 `Accepted: fixed` in the window
#    is narrowed or removed in the promotion PR (both copies, kept identical); the ≤ 2 findings
#    budget stays.
# 5. **Required check:** only when rule 1 promotes, ask Sami to add the check `security`
#    (integration 15368) to ruleset 12331919's required checks, so the merger's `--ready` also
#    refuses; the promotion PR adds `scripts/security-settings.sh --require-security-check`, which
#    reads the ruleset's rules whole and writes them back with the check appended (`PUT
#    /repos/{owner}/{repo}/rulesets/{id}` replaces the rule list).
#
# Where the numbers come from. Every read is `gh api` (in CI the workflow token with the security
# job's permissions; on a devbox the routed GitHub App, which reads Actions runs and artifacts and
# is refused the alert endpoints — those rows read BLOCKED). The Security workflow's security job
# uploads, per run, `security-report` (its counts) and three marker artifacts listable by name, so
# a window of a few thousand pull request runs costs a few dozen calls rather than one download per
# run: `dependencies-tool-error` (osv-scanner or govulncheck recorded a tool error; rule 2 counts
# these), `zizmor-tool-error` (the run has no zizmor result; rule 1b skips it) and `zizmor-new` (a
# pull_request run found zizmor findings new against its base; its zip is that run's
# zizmor-findings.json).
#   - runs on main: every scheduled and dispatched run since the window opened, and main's newest
#     push run, each with its security-report; rule 1a reads the newest completed one with a
#     zizmor result.
#   - merged pull requests (merged inside the window): a PR's runs are matched by head branch and
#     time, since GitHub empties a run's pull_requests list once the PR closes, and a run with a
#     zizmor-tool-error marker is skipped. new₀ is the new set of its first run (from that run's
#     zizmor-new marker; no marker means nothing new). Each new₀ finding is compared, as a
#     multiset so a repeated finding counts once per copy, with the PR's last run's
#     zizmor-findings: `ignored` when that run's `ignored` lists it (the findings its --no-ignores
#     audit reports beyond its head audit, i.e. what the tree's inline and zizmor.yml ignores
#     suppress, as zizmor matches them), `merged with findings` when that run still reports it new
#     against its base, and `fixed` otherwise.
#   - Security[<tag>]: review threads opened inside the window, with the newest `Accepted:` reply
#     of each, from the repository's review comments.
# Every artifact is read strictly: a run's security-report or zizmor-findings artifact that is
# missing, expired or not in the shape the Security workflow writes (.github/scripts/
# deps-summary.sh, .github/scripts/zizmor-findings.sh), or a zizmor-new marker that lists nothing
# new, stops the report with exit 2 naming it. Only a half that recorded a tool error reads as no
# result.
# Nothing published here carries a CodeQL location or a secret: alerts are counted by rule,
# severity and state.
#
# Exit codes: 0 the report; 1 a DECISION block while a check is still report-only; 2 no Security
# workflow, a usage error, a read that failed for a reason other than 403, or an artifact read as
# above.
# CI runs its tests (security-report.test.sh) in the test job of pr-and-main.yaml.
set -euo pipefail

python3 - "$@" <<'PY'
import base64
import functools
import io
import json
import re
import subprocess
import sys
import zipfile
from collections import Counter, defaultdict
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from itertools import takewhile
from urllib.parse import quote

USAGE = """usage: security-report.sh [--repo owner/repo] [--window-days 14] [--decision force|none]
       security-report.sh --window-flags .github/security-window.json [--at REV]
       security-report.sh --base-commit EVENT [MERGE_GROUP_BASE_SHA]"""
CHECKS = ("zizmor", "dependencies")
PRECISION_BAR = 0.7
MIN_DISPOSITIONS = 5
KILL_AT = 3
NARROW_AT = 3
CODEQL_BLOCKED = "BLOCKED (caller lacks security-events read; the scheduled Security run has it)"
SECRET_BLOCKED = "BLOCKED (caller lacks secret_scanning read; run as an admin)"
SECURITY_TAG = re.compile(r"^Security\[([a-z][a-z-]*)\]:")
# The counts of each half of a security-report.json (.github/scripts/deps-summary.sh report).
REPORT_HALVES = {"zizmor": ("head_count",), "osv": ("total", "with_fix"), "govulncheck": ("reachable", "informational")}


def fail(message, code=2):
    print(f"security-report.sh: {message}", file=sys.stderr)
    sys.exit(code)


def window_flags(text, where):
    """({check: report_only}, notes) from the text of .github/security-window.json, None when there is
    no file. A check is blocking only where the file says false; every other form reads as report-only
    and is named in a note. Values the file holds are quoted with json.dumps, so a note is one line."""
    every = dict.fromkeys(CHECKS, True)
    if text is None:
        return every, [f"{where}: missing; every check reads as report-only"]
    try:
        document = json.loads(text)
    except ValueError as error:
        return every, [f"{where}: not JSON ({error}); every check reads as report-only"]
    if not isinstance(document, dict) or "report_only" not in document:
        return every, [f"{where}: not an object with a report_only key; every check reads as report-only"]
    value = document["report_only"]
    if isinstance(value, bool):
        return dict.fromkeys(CHECKS, value), [
            f"{where}: report_only is one boolean ({json.dumps(value)}) for every check; the per-check form is "
            '{"report_only": {"zizmor": …, "dependencies": …}}']
    if not isinstance(value, dict):
        return every, [f"{where}: report_only is {json.dumps(value)}, not an object of checks; "
                       "every check reads as report-only"]
    flags, notes = dict(every), []
    for check in CHECKS:
        if check not in value:
            notes.append(f"{where}: report_only has no {check} key; {check} reads as report-only")
        elif not isinstance(value[check], bool):
            notes.append(f"{where}: report_only.{check} is {json.dumps(value[check])}, not true or false; "
                         f"{check} reads as report-only")
        else:
            flags[check] = value[check]
    for key in sorted(set(value) - set(CHECKS)):
        notes.append(f"{where}: report_only.{json.dumps(key)} names no check ({', '.join(CHECKS)}); ignored")
    return flags, notes


def git(*args):
    return subprocess.run(["git", *args], capture_output=True, text=True)


def file_at(rev, path):
    """PATH's text at commit REV, None when REV has no such file; REV is fetched from origin at depth 1
    when it is not local (a depth-1 checkout holds only HEAD)."""
    if git("cat-file", "-e", f"{rev}^{{commit}}").returncode != 0:
        fetched = git("fetch", "--no-tags", "--depth=1", "origin", rev)
        if fetched.returncode != 0:
            fail(f"cannot fetch {rev} from origin: {fetched.stderr.strip()}")
    if git("cat-file", "-e", f"{rev}:{path}").returncode != 0:
        return None
    shown = git("show", f"{rev}:{path}")
    if shown.returncode != 0:
        fail(f"cannot read {path} at {rev}: {shown.stderr.strip()}")
    return shown.stdout


def print_window_flags(args):
    """--window-flags FILE [--at REV]: the window job's reading of the flags, as $GITHUB_OUTPUT lines."""
    if len(args) == 1:
        path, rev = args[0], None
    elif len(args) == 3 and args[1] == "--at":
        path, rev = args[0], args[2]
    else:
        fail(f"--window-flags takes one file and, optionally, --at REV\n{USAGE}")
    if rev is None:
        try:
            with open(path, encoding="utf-8") as handle:
                text = handle.read()
        except OSError:
            text = None
        flags, notes = window_flags(text, path)
    else:
        flags, notes = window_flags(file_at(rev, path), f"{path} at {rev[:12]}")
    for check in CHECKS:
        print(f"{check}={json.dumps(flags[check])}")
    for note in notes:
        print(note, file=sys.stderr)
    sys.exit(0)


def print_base_commit(args):
    """--base-commit EVENT [MERGE_GROUP_BASE_SHA]: the commit a run's head is judged against."""
    if not args or len(args) > 2:
        fail(f"--base-commit takes an event and, for merge_group, its base_sha\n{USAGE}")
    event = args[0]
    if event == "merge_group":
        if len(args) != 2 or not args[1]:
            fail("--base-commit merge_group needs the merge group's base_sha")
        print(args[1])
    elif event == "pull_request":
        # A depth-1 checkout marks HEAD shallow, so `git rev-parse HEAD^1` fails; the raw commit still
        # names its parents.
        head = git("cat-file", "-p", "HEAD")
        if head.returncode != 0:
            fail(f"cannot read HEAD: {head.stderr.strip()}")
        parents = [line.split()[1] for line in head.stdout.split("\n\n", 1)[0].splitlines()
                   if line.startswith("parent ")]
        if len(parents) != 2:
            fail(f"HEAD is not a merge commit ({len(parents)} parents); a pull_request run checks out the pull "
                 "request's merge commit")
        print(parents[0])
    sys.exit(0)


def parse_args(args):
    """(repo, window days, decision) from the command line."""
    repo, window_days, decision = "sjawhar/legion", 14, "none"
    while args:
        flag = args.pop(0)
        if flag in ("-h", "--help"):
            print(USAGE)
            sys.exit(0)
        if flag not in ("--repo", "--window-days", "--decision") or not args:
            fail(f"unexpected argument {flag!r}\n{USAGE}")
        value = args.pop(0)
        if flag == "--repo":
            if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", value):
                fail(f"--repo takes owner/repo, not {value!r}")
            repo = value
        elif flag == "--window-days":
            if not value.isdigit() or int(value) < 1:
                fail(f"--window-days takes a positive number of days, not {value!r}")
            window_days = int(value)
        else:
            if value not in ("force", "none"):
                fail(f"--decision takes force or none, not {value!r}")
            decision = value
    return repo, window_days, decision


if sys.argv[1:2] == ["--window-flags"]:
    print_window_flags(sys.argv[2:])
if sys.argv[1:2] == ["--base-commit"]:
    print_base_commit(sys.argv[2:])
repo, window_days, decision = parse_args(sys.argv[1:])


class Forbidden(Exception):
    pass


class NotFound(Exception):
    pass


def gh(path, raw=False):
    proc = subprocess.run(["gh", "api", path], capture_output=True)
    if proc.returncode != 0:
        err = proc.stderr.decode("utf-8", "replace").strip()
        if "HTTP 403" in err:
            raise Forbidden(err)
        if "HTTP 404" in err:
            raise NotFound(err)
        fail(f"gh api {path} failed: {err}")
    if raw:
        return proc.stdout
    try:
        return json.loads(proc.stdout)
    except ValueError as error:
        fail(f"gh api {path} did not answer JSON: {error}")


def paged(path, key=None):
    joiner = "&" if "?" in path else "?"
    page = 1
    while True:
        data = gh(f"{path}{joiner}per_page=100&page={page}")
        items = data.get(key, []) if key else data
        yield from items
        if len(items) < 100:
            return
        page += 1


def when(stamp):
    return datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc) if stamp else None


def dash(value):
    return "-" if value is None else str(value)


def pair(section, first, second):
    return "-/-" if section is None else f"{dash(section[first])}/{dash(section[second])}"


@functools.cache
def artifacts_of(run_id):
    return list(paged(f"repos/{repo}/actions/runs/{run_id}/artifacts", "artifacts"))


def run_artifact(run_id, name):
    """The run's newest artifact called NAME; the Security workflow uploads one on every run."""
    found = [artifact for artifact in artifacts_of(run_id) if artifact["name"] == name]
    if not found:
        fail(f"run {run_id} has no {name} artifact")
    return max(found, key=lambda artifact: artifact["id"])


def artifact_file(artifact, filename):
    if artifact["expired"]:
        fail(f"artifact {artifact['id']} ({artifact['name']}) of run {artifact['workflow_run']['id']} has expired; "
             f"its {filename} can no longer be read")
    data = gh(f"repos/{repo}/actions/artifacts/{artifact['id']}/zip", raw=True)
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        names = [name for name in archive.namelist() if name.rsplit("/", 1)[-1] == filename]
        if not names:
            fail(f"artifact {artifact['id']} ({artifact['name']}) holds no {filename}")
        try:
            return json.loads(archive.read(names[0]))
        except ValueError as error:
            fail(f"artifact {artifact['id']} ({artifact['name']}) holds a {filename} that is not JSON: {error}")


def shape(condition, where, what):
    if not condition:
        fail(f"{where} {what}, not the shape the Security workflow writes")


def is_count(value):
    return isinstance(value, int) and not isinstance(value, bool)


@functools.cache
def security_report(run_id):
    """The run's security-report.json as {half: its counts, or None where that half recorded a tool
    error, "tool_error": bool}; fails when the run has none or it is not in the security job's shape."""
    report = artifact_file(run_artifact(run_id, "security-report"), "security-report.json")
    where = f"run {run_id}'s security-report.json"
    shape(isinstance(report, dict) and isinstance(report.get("tool_error"), bool), where, "has no tool_error")
    halves = {"tool_error": report["tool_error"]}
    for half, counts in REPORT_HALVES.items():
        section = report.get(half)
        shape(isinstance(section, dict) and isinstance(section.get("tool_error"), bool), where,
              f"has no {half} half with a tool_error")
        if section["tool_error"]:
            halves[half] = None
            continue
        shape(all(is_count(section.get(count)) for count in counts), where,
              f"has a {half} half without {' and '.join(counts)}")
        if half == "zizmor":
            shape("new_count" in section and (section["new_count"] is None or is_count(section["new_count"])),
                  where, "has a zizmor half without new_count")
        halves[half] = section
    return halves


def zizmor_findings(artifact):
    """A pull_request run's zizmor-findings.json (its zizmor-findings artifact or zizmor-new marker),
    None when it records a tool error; fails when it is in neither of zizmor-findings.sh's shapes."""
    document = artifact_file(artifact, "zizmor-findings.json")
    where = f"run {artifact['workflow_run']['id']}'s {artifact['name']} artifact"
    shape(isinstance(document, dict), where, "is not an object")
    if "tool_error" in document:
        shape(document["tool_error"] is True, where, "has a tool_error that is not true")
        return None
    for key in ("new", "ignored"):
        shape(isinstance(document.get(key), list) and all(
            isinstance(found, dict) and isinstance(found.get("fingerprint"), str) and isinstance(found.get("ident"), str)
            for found in document[key]), where, f"has no {key} list of findings")
    return document


# --- the workflow, the flags, the window --------------------------------------------------------
def security_workflow():
    return next((w for w in paged(f"repos/{repo}/actions/workflows", "workflows")
                 if w.get("path") == ".github/workflows/security.yaml" or w.get("name") == "Security"), None)


def report_only_flags():
    """({check: report_only}, notes) from .github/security-window.json on main (window_flags)."""
    try:
        document = gh(f"repos/{repo}/contents/.github/security-window.json?ref=main")
        text = base64.b64decode(document["content"]).decode("utf-8", "replace")
    except NotFound:
        text = None
    return window_flags(text, ".github/security-window.json on main")


class Window:
    """window_days from the Security workflow's first push run on main."""

    def __init__(self, start):
        self.start, self.end = start, start + timedelta(days=window_days)
        self.closed = datetime.now(timezone.utc) >= self.end

    def holds(self, stamp):
        moment = when(stamp)
        return moment is not None and self.start <= moment < self.end


# --- runs on main (rule 1a) ----------------------------------------------------------------------
def completed(runs):
    return sorted((run for run in runs if run.get("status") == "completed"
                   and run.get("conclusion") not in ("cancelled", "skipped")), key=lambda run: run["created_at"])


def print_main_runs(done, window):
    """A row for every scheduled and dispatched run since the window opened, and main's newest push."""
    rows = [run for run in done if run["event"] != "push" and when(run["created_at"]) >= window.start]
    rows += [run for run in done if run["event"] == "push"][-1:]
    print()
    print("## Runs on main")
    print()
    print("| run | event | head | zizmor head/new | osv total/with-fix | govulncheck reachable/informational | tool error |")
    print("| --- | --- | --- | --- | --- | --- | --- |")
    for run in sorted(rows, key=lambda run: run["created_at"]):
        found = security_report(run["id"])
        print(f"| {run['id']} | {run['event']} | {run['head_sha'][:7]} | {pair(found['zizmor'], 'head_count', 'new_count')} "
              f"| {pair(found['osv'], 'total', 'with_fix')} | {pair(found['govulncheck'], 'reachable', 'informational')} "
              f"| {'yes' if found['tool_error'] else 'no'} |")


def zizmor_on_main(done):
    """Rule 1a: the head_count of the newest completed main run with a zizmor result, looking back ten
    runs at most, or None: a longer run of tool errors is itself the answer (no result on main)."""
    for run in list(reversed(done))[:10]:
        zizmor = security_report(run["id"])["zizmor"]
        if zizmor is not None:
            print()
            print(f"zizmor on main: {zizmor['head_count']} findings (run {run['id']}, {run['event']})")
            return zizmor["head_count"]
    return None


# --- tool errors (rule 2) ------------------------------------------------------------------------
def dependency_tool_errors(window):
    """Rule 2: how many runs inside the window marked a dependency scanner tool error."""
    markers = sorted((m for m in paged(f"repos/{repo}/actions/artifacts?name=dependencies-tool-error", "artifacts")
                      if window.holds(m["created_at"])), key=lambda m: m["created_at"])
    runs = {marker["workflow_run"]["id"] for marker in markers}
    print()
    print(f"## Dependency scanner tool errors in the window: {len(runs)}")
    for marker in markers:
        run = marker["workflow_run"]
        print(f"- run {run['id']} ({run.get('head_branch')}@{(run.get('head_sha') or '')[:7]}, {marker['created_at']})")
    return len(runs)


# --- merged pull requests (rule 1b) --------------------------------------------------------------
@dataclass
class Dispositions:
    """How the merged pull requests' new₀ findings ended: a row per pull request, (number, first run,
    last run, len(new₀), Counter of outcomes), and the outcomes per audit."""
    rows: list = field(default_factory=list)
    per_audit: defaultdict = field(default_factory=lambda: defaultdict(Counter))

    @property
    def totals(self):
        return sum((row[-1] for row in self.rows), Counter())

    @property
    def count(self):
        """Fixed and ignored findings: the dispositions precision is taken over."""
        return self.totals["fixed"] + self.totals["ignored"]

    @property
    def precision(self):
        return self.totals["fixed"] / self.count if self.count else None


def merged_pull_requests(window):
    """The pull requests merged into main inside the window, by number. The listing runs newest update
    first and a pull request is updated no earlier than its merge, so it stops at the first one last
    updated before the window opened."""
    closed = paged(f"repos/{repo}/pulls?state=closed&base=main&sort=updated&direction=desc")
    recent = takewhile(lambda pr: when(pr["updated_at"]) >= window.start, closed)
    return sorted((pr for pr in recent if pr.get("merged_at") and window.holds(pr["merged_at"])),
                  key=lambda pr: pr["number"])


def belongs(pr, branch, stamp):
    return branch == pr["head"]["ref"] and when(pr["created_at"]) <= when(stamp) <= when(pr["merged_at"])


def pull_request_runs(pr, runs_path, zizmor_error_run_ids):
    """The pull request's completed pull_request runs with a zizmor result, oldest first."""
    return sorted(
        (run for run in paged(f"{runs_path}?event=pull_request&branch={quote(pr['head']['ref'], safe='')}", "workflow_runs")
         if run.get("event") == "pull_request" and run.get("status") == "completed"
         and run.get("conclusion") not in ("cancelled", "skipped") and run["id"] not in zizmor_error_run_ids
         and belongs(pr, run.get("head_branch"), run["created_at"])),
        key=lambda run: run["created_at"],
    )


def outcomes(new0, final):
    """(finding, outcome) for each new₀ finding against the last run's findings, as multisets."""
    ignored = Counter(found["fingerprint"] for found in final["ignored"])
    remaining = Counter(found["fingerprint"] for found in final["new"])
    for found in new0:
        fingerprint = found["fingerprint"]
        if ignored[fingerprint] > 0:
            ignored[fingerprint] -= 1
            yield found, "ignored"
        elif remaining[fingerprint] > 0:
            remaining[fingerprint] -= 1
            yield found, "merged with findings"
        else:
            yield found, "fixed"


def merged_findings(window, runs_path):
    """Rule 1b: the dispositions of the new₀ findings of the pull requests merged inside the window."""
    new_markers = list(paged(f"repos/{repo}/actions/artifacts?name=zizmor-new", "artifacts"))
    zizmor_error_run_ids = {m["workflow_run"]["id"]
                            for m in paged(f"repos/{repo}/actions/artifacts?name=zizmor-tool-error", "artifacts")}
    tally, nothing_new = Dispositions(), []
    for pr in merged_pull_requests(window):
        markers = {m["workflow_run"]["id"]: m for m in new_markers
                   if belongs(pr, m["workflow_run"].get("head_branch"), m["created_at"])}
        runs = pull_request_runs(pr, runs_path, zizmor_error_run_ids) if markers else []
        if not runs or runs[0]["id"] not in markers:
            nothing_new.append(pr["number"])
            continue
        first, last = runs[0], runs[-1]
        first_findings = zizmor_findings(markers[first["id"]])
        if first_findings is None or not first_findings["new"]:
            fail(f"run {first['id']}'s zizmor-new marker lists no new finding; "
                 "the Security workflow uploads it only for new findings")
        final = zizmor_findings(run_artifact(last["id"], "zizmor-findings"))
        if final is None:
            fail(f"run {last['id']}'s zizmor-findings records a tool error, but the run has no zizmor-tool-error marker")
        counts = Counter()
        for found, outcome in outcomes(first_findings["new"], final):
            counts[outcome] += 1
            tally.per_audit[found["ident"]][outcome] += 1
        tally.rows.append((pr["number"], first["id"], last["id"], len(first_findings["new"]), counts))

    print()
    print("## Merged pull requests in the window")
    print()
    print("| pr | first run | last run | new | fixed | ignored | merged with findings |")
    print("| --- | --- | --- | --- | --- | --- | --- |")
    for number, first_id, last_id, new_count, counts in tally.rows:
        print(f"| #{number} | {first_id} | {last_id} | {new_count} | {counts['fixed']} | {counts['ignored']} "
              f"| {counts['merged with findings']} |")
    if nothing_new:
        print()
        print("no new findings: " + ", ".join(f"#{number}" for number in nothing_new))
    print()
    if tally.count >= MIN_DISPOSITIONS:
        print(f"precision {tally.precision:.2f} ({tally.totals['fixed']} fixed / {tally.totals['ignored']} ignored)")
    else:
        print(f"precision undecided ({tally.count} of {MIN_DISPOSITIONS} dispositions)")
    for ident, counts in sorted(tally.per_audit.items()):
        if counts["ignored"] >= KILL_AT:
            print(f"KILL: {ident} (ignored {counts['ignored']}×, fixed {counts['fixed']}×) — "
                  f"workflows: rules.{ident}.ignore in .github/zizmor.yml; composite actions: inline comment")
    return tally


# --- CodeQL (rule 3) -----------------------------------------------------------------------------
def codeql_fixed_in_window(window):
    """Rule 3: how many CodeQL alerts reached fixed inside the window, None when the caller may not
    read them."""
    print()
    print("## CodeQL")
    print()
    try:
        # The newest hundred analyses on main: every push adds one per language, so their categories
        # are the languages CodeQL currently analyses.
        categories = sorted({a.get("category") or "(none)" for a in
                             gh(f"repos/{repo}/code-scanning/analyses?tool_name=CodeQL&ref=refs/heads/main&per_page=100")})
        print(f"CodeQL analyses on main: {len(categories)}" + (f" ({', '.join(categories)})" if categories else ""))
    except Forbidden:
        print(f"CodeQL analyses on main: {CODEQL_BLOCKED}")
    except NotFound:
        print("CodeQL analyses on main: 0")
    try:
        alerts = list(paged(f"repos/{repo}/code-scanning/alerts?tool_name=CodeQL"))
    except Forbidden:
        print(f"CodeQL alerts: {CODEQL_BLOCKED}")
        return None
    except NotFound:
        print("CodeQL alerts: none (no CodeQL analysis on this repository)")
        return 0
    fixed = sum(1 for a in alerts if a.get("state") == "fixed" and window.holds(a.get("fixed_at")))
    tally = Counter((a["rule"].get("id"), a["rule"].get("security_severity_level") or a["rule"].get("severity"),
                     a.get("state")) for a in alerts)
    print(f"CodeQL alerts: {len(alerts)}; fixed inside the window: {fixed}")
    if tally:
        print()
        print("| rule | severity | state | alerts |")
        print("| --- | --- | --- | --- |")
        for (rule, severity, state), count in sorted(tally.items(), key=lambda item: tuple(map(str, item[0]))):
            print(f"| {rule} | {dash(severity)} | {state} | {count} |")
    return fixed


# --- secret scanning -----------------------------------------------------------------------------
def print_secret_scanning():
    print()
    print("## Secret scanning")
    print()
    try:
        states = Counter(a.get("state") for a in paged(f"repos/{repo}/secret-scanning/alerts"))
        print(f"secret-scanning alerts: {states['open']} open, {sum(states.values()) - states['open']} resolved")
    except Forbidden:
        print(f"secret-scanning alerts: {SECRET_BLOCKED}")
    except NotFound:
        print("secret-scanning alerts: secret scanning is not enabled")


# --- review threads (rule 4) ---------------------------------------------------------------------
def narrowed(counts):
    """Rule 4: a rubric row with NARROW_AT or more not-a-defect threads and none fixed."""
    return counts["not a defect"] >= NARROW_AT and counts["fixed"] == 0


def review_threads(window):
    """Rule 4: {tag: Counter of fixed, not a defect and open} over the Security[<tag>]: review threads
    opened inside the window, each by its newest `Accepted:` reply."""
    comments = list(paged(f"repos/{repo}/pulls/comments?sort=created&direction=asc"
                          f"&since={window.start:%Y-%m-%dT%H:%M:%SZ}"))
    replies = defaultdict(list)
    for comment in comments:
        if comment.get("in_reply_to_id"):
            replies[comment["in_reply_to_id"]].append(comment)
    threads = defaultdict(Counter)
    for comment in comments:
        tag = SECURITY_TAG.match(comment.get("body") or "")
        if comment.get("in_reply_to_id") or not tag or not window.holds(comment["created_at"]):
            continue
        accepted = [r for r in sorted(replies[comment["id"]], key=lambda r: r["created_at"])
                    if (r.get("body") or "").startswith("Accepted:")]
        reply = accepted[-1]["body"] if accepted else ""
        if reply.startswith("Accepted: fixed"):
            threads[tag.group(1)]["fixed"] += 1
        elif reply.startswith("Accepted: not a defect"):
            threads[tag.group(1)]["not a defect"] += 1
        else:
            threads[tag.group(1)]["open"] += 1
    print()
    print("## Security review threads")
    print()
    if not threads:
        print("no Security[<tag>]: review threads in the window")
    for tag, counts in sorted(threads.items()):
        print(f"{'NARROW' if narrowed(counts) else 'keep'}: {tag} row ({counts['not a defect']} not a defect, "
              f"{counts['fixed']} fixed" + (f", {counts['open']} unanswered" if counts["open"] else "") + ")")
    return threads


# --- the decision --------------------------------------------------------------------------------
def zizmor_decision(main_count, dispositions):
    """Rule 1: (whether zizmor is promoted, its DECISION line)."""
    if main_count is None:
        return False, "HOLD zizmor — no zizmor result in main's newest ten runs (tool errors)"
    if main_count > 0:
        return False, (f"HOLD zizmor — {main_count} findings on main (fix each, or ignore it with a reason: "
                       "workflows in .github/zizmor.yml, composite actions inline)")
    if dispositions.count < MIN_DISPOSITIONS:
        return True, "PROMOTE zizmor (rule 1a alone)"
    if dispositions.precision >= PRECISION_BAR:
        return True, "PROMOTE zizmor"
    return False, (f"HOLD zizmor — precision {dispositions.precision:.2f} is below {PRECISION_BAR} "
                   f"over {dispositions.count} dispositions")


def dependencies_decision(tool_error_runs):
    """Rule 2: the dependency scanners' DECISION line."""
    if tool_error_runs:
        return f"HOLD dependencies — {tool_error_runs} tool errors in the window"
    return "PROMOTE dependencies"


def decide(flags, main_count, dispositions, tool_error_runs, codeql_fixed, threads):
    """The DECISION lines from each rule's value, with rule 5's NEXT when rule 1 promotes zizmor. A check
    whose flag on main is already false is blocking, and is not decided again."""
    lines = []
    if flags["zizmor"]:
        promote_zizmor, zizmor_line = zizmor_decision(main_count, dispositions)
        lines.append(f"DECISION: {zizmor_line}")
        if promote_zizmor:
            lines.append("NEXT: ask a repository admin to require the check `security` (rule 5)")
    else:
        lines.append("DECISION: zizmor already blocking (report_only.zizmor is false)")
    lines.append(f"DECISION: {dependencies_decision(tool_error_runs)}" if flags["dependencies"]
                 else "DECISION: dependencies already blocking (report_only.dependencies is false)")
    if codeql_fixed is None:
        lines.append(f"DECISION: codeql: {CODEQL_BLOCKED}")
    elif codeql_fixed:
        lines.append(f"DECISION: PROMOTE codeql pull_request trigger "
                     f"({codeql_fixed} fixed alert{'s' if codeql_fixed > 1 else ''})")
    else:
        lines.append("DECISION: HOLD codeql — no CodeQL alert on main reached fixed inside the window; stay main-only")
    narrow = [tag for tag, counts in sorted(threads.items()) if narrowed(counts)]
    lines.append(f"DECISION: NARROW the rubric rows {', '.join(narrow)}" if narrow
                 else "DECISION: keep every rubric row")
    return lines


def main():
    security = security_workflow()
    if security is None:
        print(f"no Security workflow on {repo}")
        sys.exit(2)
    flags, notes = report_only_flags()
    print(f"# Security report for {repo}")
    print()
    print("report_only: " + ", ".join(f"{check} {json.dumps(flags[check])}" for check in CHECKS))
    for note in notes:
        print(f"note: {note}")

    runs_path = f"repos/{repo}/actions/workflows/{security['id']}/runs"
    runs = [run for run in paged(f"{runs_path}?branch=main", "workflow_runs")
            if run.get("head_branch") == "main" and run.get("event") in ("push", "schedule", "workflow_dispatch")]
    pushes = [run for run in runs if run["event"] == "push"]
    if not pushes:
        print("window has not started: the Security workflow has no push run on main yet")
        sys.exit(0)
    window = Window(min(when(run["created_at"]) for run in pushes))
    print(f"window: {window.start:%Y-%m-%dT%H:%M:%SZ} to {window.end:%Y-%m-%dT%H:%M:%SZ} "
          f"({'window closed' if window.closed else 'window closes'} {window.end:%Y-%m-%d})")

    done = completed(runs)
    print_main_runs(done, window)
    main_count = zizmor_on_main(done)
    tool_error_runs = dependency_tool_errors(window)
    dispositions = merged_findings(window, runs_path)
    codeql_fixed = codeql_fixed_in_window(window)
    print_secret_scanning()
    threads = review_threads(window)

    still_report_only = [check for check in CHECKS if flags[check]]
    forced = decision == "force"
    if not (forced or (window.closed and still_report_only)):
        sys.exit(0)
    print()
    print("## Decision" + (" (FORCED)" if forced else ""))
    print()
    if forced:
        print("FORCED: --decision force" if window.closed
              else "FORCED: --decision force printed this block before the window closed")
    for line in decide(flags, main_count, dispositions, tool_error_runs, codeql_fixed, threads):
        print(line)
    sys.exit(1 if still_report_only else 0)


main()
PY
