#!/usr/bin/env bash
# The report-only window's numbers and its day-14 decision (AGENTC-1305, Task 6 of the security
# plan). The Security workflow's daily scheduled job runs it (.github/workflows/security.yaml,
# `Report and decide`); anyone can run it from a checkout:
#
#   scripts/security-report.sh [--repo owner/repo] [--window-days 14] [--decision force|none]
#
# The window opens at the first `push` run of the Security workflow on main (the merge that added
# it) and lasts --window-days. Once it has closed, or with --decision force, the report ends in a
# DECISION block; while .github/security-window.json on main still says report_only: true, that
# block makes the exit code 1, which fails the scheduled run so its status badge turns red and the
# window watcher wakes the owner of the decision. With report_only: false it prints the numbers
# and no decision (unless forced), and exits 0.
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
#     zizmor-new marker; no marker means nothing new). Each new₀ finding is `ignored` when the PR's
#     own patches add a `# zizmor: ignore[<audit>]` line in its file, or add its file name to an
#     ignore list whose enclosing `rules.<audit>` key is read from .github/zizmor.yml at the PR's
#     merge commit (a hunk's context rarely reaches the key), `merged with findings` when the PR's
#     last run still reports it new against its base (compared as a multiset, so a repeated finding
#     counts once per copy), and `fixed` otherwise.
#   - Security[<tag>]: review threads opened inside the window, with the newest `Accepted:` reply
#     of each, from the repository's review comments.
# Nothing published here carries a CodeQL location or a secret: alerts are counted by rule,
# severity and state.
#
# Exit codes: 0 the report; 1 a DECISION block while report_only is true; 2 no Security workflow,
# a usage error, or a read that failed for a reason other than 403.
# CI runs its tests (security-report.test.sh) in the test job of pr-and-main.yaml.
set -euo pipefail

python3 - "$@" <<'PY'
import base64
import io
import json
import re
import subprocess
import sys
import zipfile
from collections import Counter, defaultdict
from datetime import datetime, timedelta, timezone
from urllib.parse import quote

USAGE = "usage: security-report.sh [--repo owner/repo] [--window-days 14] [--decision force|none]"
PRECISION_BAR = 0.7
MIN_DISPOSITIONS = 5
KILL_AT = 3
NARROW_AT = 3
CODEQL_BLOCKED = "BLOCKED (caller lacks security-events read; the scheduled Security run has it)"
SECRET_BLOCKED = "BLOCKED (caller lacks secret_scanning read; run as an admin)"
SECURITY_TAG = re.compile(r"^Security\[([a-z][a-z-]*)\]:")
INLINE_IGNORE = re.compile(r"zizmor:\s*ignore\[([^\]]*)\]")
HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")
RULE_KEY = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$")
IGNORE_ITEM = re.compile(r"^\s+-\s+[\"']?([^\"':\s]+)")


def fail(message, code=2):
    print(f"security-report.sh: {message}", file=sys.stderr)
    sys.exit(code)


args = sys.argv[1:]
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


run_artifacts = {}


def artifacts_of(run_id):
    if run_id not in run_artifacts:
        run_artifacts[run_id] = list(paged(f"repos/{repo}/actions/runs/{run_id}/artifacts", "artifacts"))
    return run_artifacts[run_id]


def artifact_file(artifact, filename):
    if artifact.get("expired"):
        return None
    data = gh(f"repos/{repo}/actions/artifacts/{artifact['id']}/zip", raw=True)
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        names = [name for name in archive.namelist() if name.rsplit("/", 1)[-1] == filename]
        if not names:
            fail(f"artifact {artifact['id']} ({artifact['name']}) holds no {filename}")
        return json.loads(archive.read(names[0]))


def run_file(run_id, name, filename):
    for artifact in artifacts_of(run_id):
        if artifact["name"] == name and not artifact.get("expired"):
            return artifact_file(artifact, filename)
    return None


reports = {}


def report_of(run_id):
    if run_id not in reports:
        reports[run_id] = run_file(run_id, "security-report", "security-report.json")
    return reports[run_id]


# --- the workflow, the flag, the window ---------------------------------------------------------
security = next(
    (w for w in paged(f"repos/{repo}/actions/workflows", "workflows")
     if w.get("path") == ".github/workflows/security.yaml" or w.get("name") == "Security"),
    None,
)
if security is None:
    print(f"no Security workflow on {repo}")
    sys.exit(2)

try:
    flag = gh(f"repos/{repo}/contents/.github/security-window.json?ref=main")
except NotFound:
    fail(f"{repo} has a Security workflow but no .github/security-window.json on main")
report_only = json.loads(base64.b64decode(flag["content"]))["report_only"]
if not isinstance(report_only, bool):
    fail(f".github/security-window.json on main says report_only: {report_only!r}, not true or false")

print(f"# Security report for {repo}")
print()
print(f"report_only: {str(report_only).lower()}")

runs_path = f"repos/{repo}/actions/workflows/{security['id']}/runs"
main_runs = [
    run for run in paged(f"{runs_path}?branch=main", "workflow_runs")
    if run.get("head_branch") == "main" and run.get("event") in ("push", "schedule", "workflow_dispatch")
]
pushes = [run for run in main_runs if run["event"] == "push"]
if not pushes:
    print("window has not started: the Security workflow has no push run on main yet")
    sys.exit(0)
start = min(when(run["created_at"]) for run in pushes)
end = start + timedelta(days=window_days)
now = datetime.now(timezone.utc)
closed = now >= end


def in_window(stamp):
    moment = when(stamp)
    return moment is not None and start <= moment < end


print(f"window: {start:%Y-%m-%dT%H:%M:%SZ} to {end:%Y-%m-%dT%H:%M:%SZ} "
      f"({'window closed' if closed else 'window closes'} {end:%Y-%m-%d})")

# --- runs on main --------------------------------------------------------------------------------
completed = sorted((run for run in main_runs if run.get("status") == "completed"
                    and run.get("conclusion") not in ("cancelled", "skipped")),
                   key=lambda run: run["created_at"])
rows = [run for run in completed if run["event"] != "push" and when(run["created_at"]) >= start]
newest_push = [run for run in completed if run["event"] == "push"][-1:]
rows = sorted(rows + newest_push, key=lambda run: run["created_at"])

print()
print("## Runs on main")
print()
print("| run | event | head | zizmor head/new | osv total/with-fix | govulncheck reachable/informational | tool error |")
print("| --- | --- | --- | --- | --- | --- | --- |")
for run in rows:
    found = report_of(run["id"])
    if found is None:
        print(f"| {run['id']} | {run['event']} | {run['head_sha'][:7]} | no report | | | |")
        continue
    zizmor, osv, govulncheck = (found.get(key) or {} for key in ("zizmor", "osv", "govulncheck"))
    print(f"| {run['id']} | {run['event']} | {run['head_sha'][:7]} "
          f"| {dash(zizmor.get('head_count'))}/{dash(zizmor.get('new_count'))} "
          f"| {dash(osv.get('total'))}/{dash(osv.get('with_fix'))} "
          f"| {dash(govulncheck.get('reachable'))}/{dash(govulncheck.get('informational'))} "
          f"| {'yes' if found.get('tool_error') else 'no'} |")

# Rule 1a reads the newest completed main run with a zizmor result, looking back ten runs at most:
# a longer run of tool errors is itself the answer (no result on main).
main_head_count = None
for run in list(reversed(completed))[:10]:
    found = report_of(run["id"])
    count = ((found or {}).get("zizmor") or {}).get("head_count")
    if count is not None:
        main_head_count = count
        print()
        print(f"zizmor on main: {count} findings (run {run['id']}, {run['event']})")
        break

# --- tool errors (rule 2) ------------------------------------------------------------------------
tool_error_markers = list(paged(f"repos/{repo}/actions/artifacts?name=dependencies-tool-error", "artifacts"))
tool_error_runs = sorted({m["workflow_run"]["id"] for m in tool_error_markers if in_window(m["created_at"])})
print()
print(f"## Dependency scanner tool errors in the window: {len(tool_error_runs)}")
for marker in sorted((m for m in tool_error_markers if in_window(m["created_at"])), key=lambda m: m["created_at"]):
    run = marker["workflow_run"]
    print(f"- run {run['id']} ({run.get('head_branch')}@{(run.get('head_sha') or '')[:7]}, {marker['created_at']})")

# --- merged pull requests (rule 1b) --------------------------------------------------------------
merged, page = [], 1
while True:
    batch = gh(f"repos/{repo}/pulls?state=closed&base=main&sort=updated&direction=desc&per_page=100&page={page}")
    merged += [pr for pr in batch if pr.get("merged_at") and in_window(pr["merged_at"])]
    if len(batch) < 100 or when(batch[-1]["updated_at"]) < start:
        break
    page += 1
merged.sort(key=lambda pr: pr["number"])

new_markers = list(paged(f"repos/{repo}/actions/artifacts?name=zizmor-new", "artifacts"))
zizmor_error_run_ids = {m["workflow_run"]["id"]
                        for m in paged(f"repos/{repo}/actions/artifacts?name=zizmor-tool-error", "artifacts")}


def belongs(pr, branch, stamp):
    return branch == pr["head"]["ref"] and when(pr["created_at"]) <= when(stamp) <= when(pr["merged_at"])


def config_ignores(pr, patch):
    """The (rule, file name) pairs a PR's .github/zizmor.yml patch adds to ignore lists. Each added
    line is numbered from its hunk header and its enclosing `  <rule>:` key is read from the whole
    file at the PR's merge commit, since a hunk's three context lines rarely reach the key."""
    try:
        content = gh(f"repos/{repo}/contents/.github/zizmor.yml?ref={pr['merge_commit_sha']}")
    except NotFound:
        return set()
    lines = base64.b64decode(content["content"]).decode("utf-8").splitlines()
    added, number = set(), None
    for line in patch.splitlines():
        header = HUNK.match(line)
        if header:
            number = int(header.group(1))
            continue
        if number is None or line.startswith("-") or line.startswith("\\"):
            continue
        item = IGNORE_ITEM.match(line[1:]) if line.startswith("+") else None
        if item:
            rule = next((key.group(1) for key in map(RULE_KEY.match, reversed(lines[:number - 1])) if key), None)
            added.add((rule, item.group(1)))
        number += 1
    return added


def ignored_by(found, files, configured):
    ident, path = found["ident"], found["path"]
    if (ident, path.rsplit("/", 1)[-1]) in configured:
        return True
    for changed in files:
        if changed["filename"] != path:
            continue
        for line in (changed.get("patch") or "").splitlines():
            if line.startswith("+") and not line.startswith("+++"):
                for match in INLINE_IGNORE.finditer(line):
                    if ident in (part.strip() for part in match.group(1).split(",")):
                        return True
    return False


totals = Counter()
per_audit = defaultdict(Counter)
nothing_new, pr_rows = [], []
for pr in merged:
    markers = {m["workflow_run"]["id"]: m for m in new_markers
               if belongs(pr, m["workflow_run"].get("head_branch"), m["created_at"])}
    if not markers:
        nothing_new.append(pr)
        continue
    runs = sorted(
        (run for run in paged(f"{runs_path}?event=pull_request&branch={quote(pr['head']['ref'], safe='')}", "workflow_runs")
         if run.get("event") == "pull_request" and run.get("status") == "completed"
         and run.get("conclusion") not in ("cancelled", "skipped") and run["id"] not in zizmor_error_run_ids
         and belongs(pr, run.get("head_branch"), run["created_at"])),
        key=lambda run: run["created_at"],
    )
    if not runs or runs[0]["id"] not in markers:
        nothing_new.append(pr)
        continue
    first, last = runs[0], runs[-1]
    new0 = artifact_file(markers[first["id"]], "zizmor-findings.json").get("new") or []
    if not new0:
        nothing_new.append(pr)
        continue
    final = (artifact_file(markers[last["id"]], "zizmor-findings.json") if last["id"] in markers
             else run_file(last["id"], "zizmor-findings", "zizmor-findings.json")) or {}
    remaining = Counter(found["fingerprint"] for found in final.get("new") or [])
    files = list(paged(f"repos/{repo}/pulls/{pr['number']}/files"))
    configured = set().union(*(config_ignores(pr, changed.get("patch") or "") for changed in files
                               if changed["filename"] == ".github/zizmor.yml"))
    counts = Counter()
    for found in new0:
        if ignored_by(found, files, configured):
            outcome = "ignored"
        elif remaining[found["fingerprint"]] > 0:
            remaining[found["fingerprint"]] -= 1
            outcome = "merged with findings"
        else:
            outcome = "fixed"
        counts[outcome] += 1
        per_audit[found["ident"]][outcome] += 1
    totals.update(counts)
    pr_rows.append((pr["number"], first["id"], last["id"], len(new0), counts))

print()
print("## Merged pull requests in the window")
print()
print("| pr | first run | last run | new | fixed | ignored | merged with findings |")
print("| --- | --- | --- | --- | --- | --- | --- |")
for number, first_id, last_id, new_count, counts in pr_rows:
    print(f"| #{number} | {first_id} | {last_id} | {new_count} | {counts['fixed']} | {counts['ignored']} "
          f"| {counts['merged with findings']} |")
if nothing_new:
    print()
    print("no new findings: " + ", ".join(f"#{pr['number']}" for pr in nothing_new))

dispositions = totals["fixed"] + totals["ignored"]
precision = totals["fixed"] / dispositions if dispositions else None
print()
if dispositions >= MIN_DISPOSITIONS:
    print(f"precision {precision:.2f} ({totals['fixed']} fixed / {totals['ignored']} ignored)")
else:
    print(f"precision undecided ({dispositions} of {MIN_DISPOSITIONS} dispositions)")
for ident in sorted(per_audit):
    if per_audit[ident]["ignored"] >= KILL_AT:
        print(f"KILL: {ident} (ignored {per_audit[ident]['ignored']}×, fixed {per_audit[ident]['fixed']}×) — "
              f"workflows: rules.{ident}.ignore in .github/zizmor.yml; composite actions: inline comment")

# --- CodeQL (rule 3) -----------------------------------------------------------------------------
print()
print("## CodeQL")
print()
codeql_fixed = None
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
    codeql_fixed = sum(1 for a in alerts if a.get("state") == "fixed" and in_window(a.get("fixed_at")))
    tally = Counter((a["rule"].get("id"), a["rule"].get("security_severity_level") or a["rule"].get("severity"),
                     a.get("state")) for a in alerts)
    print(f"CodeQL alerts: {len(alerts)}; fixed inside the window: {codeql_fixed}")
    if tally:
        print()
        print("| rule | severity | state | alerts |")
        print("| --- | --- | --- | --- |")
        for (rule, severity, state), count in sorted(tally.items(), key=lambda item: tuple(map(str, item[0]))):
            print(f"| {rule} | {dash(severity)} | {state} | {count} |")
except Forbidden:
    print(f"CodeQL alerts: {CODEQL_BLOCKED}")
except NotFound:
    codeql_fixed = 0
    print("CodeQL alerts: none (no CodeQL analysis on this repository)")

# --- secret scanning -----------------------------------------------------------------------------
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
comments = list(paged(f"repos/{repo}/pulls/comments?sort=created&direction=asc&since={start:%Y-%m-%dT%H:%M:%SZ}"))
replies = defaultdict(list)
for comment in comments:
    if comment.get("in_reply_to_id"):
        replies[comment["in_reply_to_id"]].append(comment)
threads = defaultdict(Counter)
for comment in comments:
    tag = SECURITY_TAG.match(comment.get("body") or "")
    if comment.get("in_reply_to_id") or not tag or not in_window(comment["created_at"]):
        continue
    accepted = [r for r in sorted(replies[comment["id"]], key=lambda r: r["created_at"])
                if (r.get("body") or "").startswith("Accepted:")]
    verdict = accepted[-1]["body"] if accepted else ""
    if verdict.startswith("Accepted: fixed"):
        threads[tag.group(1)]["fixed"] += 1
    elif verdict.startswith("Accepted: not a defect"):
        threads[tag.group(1)]["not a defect"] += 1
    else:
        threads[tag.group(1)]["open"] += 1
print()
print("## Security review threads")
print()
if not threads:
    print("no Security[<tag>]: review threads in the window")
for tag in sorted(threads):
    counts = threads[tag]
    verdict = "NARROW" if counts["not a defect"] >= NARROW_AT and counts["fixed"] == 0 else "keep"
    print(f"{verdict}: {tag} row ({counts['not a defect']} not a defect, {counts['fixed']} fixed"
          + (f", {counts['open']} unanswered" if counts["open"] else "") + ")")

# --- the decision --------------------------------------------------------------------------------
forced = decision == "force"
if not (forced or (closed and report_only)):
    sys.exit(0)
print()
print("## Decision" + (" (FORCED)" if forced else ""))
print()
if forced:
    print("FORCED: --decision force printed this block before the window closed" if not closed
          else "FORCED: --decision force")
if main_head_count is None:
    zizmor_line = "HOLD zizmor — no zizmor result in main's newest ten runs (tool errors)"
elif main_head_count > 0:
    zizmor_line = (f"HOLD zizmor — {main_head_count} findings on main (fix each, or ignore it with a reason: "
                   "workflows in .github/zizmor.yml, composite actions inline)")
elif dispositions < MIN_DISPOSITIONS:
    zizmor_line = "PROMOTE zizmor (rule 1a alone)"
elif precision >= PRECISION_BAR:
    zizmor_line = "PROMOTE zizmor"
else:
    zizmor_line = f"HOLD zizmor — precision {precision:.2f} is below {PRECISION_BAR} over {dispositions} dispositions"
print(f"DECISION: {zizmor_line}")
if zizmor_line.startswith("PROMOTE"):
    print("NEXT: ask a repository admin to require the check `security` (rule 5)")
print("DECISION: " + (f"HOLD dependencies — {len(tool_error_runs)} tool errors in the window" if tool_error_runs
                      else "PROMOTE dependencies"))
if codeql_fixed is None:
    print(f"DECISION: codeql: {CODEQL_BLOCKED}")
elif codeql_fixed:
    print(f"DECISION: PROMOTE codeql pull_request trigger ({codeql_fixed} fixed alert{'s' if codeql_fixed > 1 else ''})")
else:
    print("DECISION: HOLD codeql — no CodeQL alert on main reached fixed inside the window; stay main-only")
narrowed = [tag for tag in sorted(threads) if threads[tag]["not a defect"] >= NARROW_AT and threads[tag]["fixed"] == 0]
print("DECISION: " + (f"NARROW the rubric rows {', '.join(narrowed)}" if narrowed else "keep every rubric row"))
sys.exit(1 if report_only else 0)
PY
