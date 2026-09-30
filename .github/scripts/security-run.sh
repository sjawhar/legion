#!/usr/bin/env bash
# Each Security run's plumbing (.github/workflows/security.yaml): the base a run is judged against,
# the checks' report-only flags, the dependency scanners' numbers, the run's report and its
# enforcement. It runs on every pull request, merge group and push; scripts/security-report.sh,
# the day-14 report, reads main's flags through window-flags.
#
# Usage:
#   security-run.sh base-commit EVENT [MERGE_GROUP_BASE_SHA]
#   security-run.sh window-flags .github/security-window.json [--at REV]
#   security-run.sh summarize [--osv osv.json] [--govulncheck govulncheck.json]… --out deps-summary.json
#   security-run.sh report --zizmor zizmor-findings.json --deps deps-summary.json --run-id ID
#       --event EVENT --head SHA --base SHA --report-only-zizmor true|false
#       --report-only-dependencies true|false --workflows RESULT --dependencies RESULT
#       --out security-report.json
#   security-run.sh enforce security-report.json
#
# base-commit — the commit a run's head is judged against, run from the checked-out tree: for
# pull_request the first parent of HEAD, which is a merge commit GitHub builds on the base branch's
# current tip, so its first parent is that tip (the event's pull_request.base.sha is the base when
# the pull request was opened or last pushed, and does not follow main); for merge_group the
# event's base_sha; for any other event nothing.
#
# window-flags — .github/security-window.json holds one report-only flag per check,
# {"report_only": {"zizmor": true, "dependencies": true}}: while a check's flag is true its Gate
# step runs under continue-on-error. A check is blocking only where the file sets its flag to
# false: a missing or unreadable file, a document that is not an object with a report_only key,
# a missing key and a value that is not true or false each read as report-only, and each is named
# in a note. A bare boolean ({"report_only": true}, the file's first form) is that value for every
# check. It prints the file's flags as `zizmor=<bool>` and `dependencies=<bool>` lines (the window
# job appends them to $GITHUB_OUTPUT) and its notes on stderr, and exits 0; with --at it reads the
# file as it is at commit REV (fetched from origin at depth 1 when absent), which is how a pull
# request or merge group reads its base's flags rather than its own: a promotion takes effect on
# main from its merge, its own pull request's run stays report-only, and a pull request cannot
# make its own check report-only again by editing the file.
#
# summarize — the dependencies job's numbers. --osv is osv-scanner's `--format json` output, given
# only when osv-scanner ran to completion; each --govulncheck is one module's `govulncheck -format
# json` stream, given only when govulncheck ran in every module. Output:
#   {"osv": {"total", "with_fix", "tool_error"}, "govulncheck": {"reachable", "informational", "tool_error"}}
#   - osv: `total` counts every vulnerability of every package of every result, `with_fix` those
#     with a `fixed` event in an affected range.
#   - govulncheck: one entry per (module, OSV id), `reachable` when any of its findings is
#     symbol-level (its trace's first frame names a function), `informational` otherwise.
# Each half fails closed. An absent input, or one not in the shape its scanner writes, records
# {"tool_error": true} with null counts and says why on stderr, so a scanner upgrade that renames
# a key reads as a tool error rather than as zero vulnerabilities:
#   - osv-scanner: an object whose `results` array holds results with a `packages` array, packages
#     with a `vulnerabilities` array, and vulnerabilities with an `affected` array;
#   - govulncheck: a stream of one-key records, each config, progress, SBOM, osv or finding, with
#     a config record, and every finding naming its OSV id and a non-empty trace.
#
# report — the security job's security-report.json, which scripts/security-report.sh reads, from
# the run's two artifacts: {run_id, event, head_sha, base_sha, report_only: {zizmor, dependencies},
# zizmor: {head_count, new_count, by_audit, tool_error}, osv, govulncheck, tool_error, gate:
# {workflows, dependencies}}. Each --report-only-* is that check's flag as the window job read it
# from .github/security-window.json; an empty one (the window job did not run) and an empty --base
# are null. A missing or malformed artifact records a tool error for its half, never a clean run,
# and says why on stderr: zizmor-findings.json is either zizmor-findings.sh's output or the
# workflow's {"tool_error": true} form; deps-summary.json is summarize's output. The step summary's
# markdown table goes to stdout.
#
# enforce — the security job's last step. A check whose flag in security-report.json is false is
# blocking, and enforce exits 1 when that check's job (workflows for zizmor, dependencies for the
# dependency scanners) concluded anything but success: that job's Gate no longer runs under
# continue-on-error, so its failure is the finding. A check whose flag is true or unknown (the
# window job did not run) is report-only and never fails here, so requiring the security check
# refuses nothing a report-only check found.
#
# Exit codes: 0 printed, written or nothing to enforce; 1 a blocking check's job did not succeed;
# 2 a usage error, a report enforce cannot read, a base that cannot be fetched or read, or a
# pull_request HEAD that is not a merge commit.
# CI runs its tests (security-run.test.sh) in the test job of pr-and-main.yaml.
set -euo pipefail

python3 - "$@" <<'PY'
import json
import subprocess
import sys
from collections import Counter

USAGE = """usage: security-run.sh base-commit EVENT [MERGE_GROUP_BASE_SHA]
       security-run.sh window-flags .github/security-window.json [--at REV]
       security-run.sh summarize [--osv osv.json] [--govulncheck govulncheck.json]... --out deps-summary.json
       security-run.sh report --zizmor zizmor-findings.json --deps deps-summary.json --run-id ID --event EVENT
                              --head SHA --base SHA --report-only-zizmor true|false
                              --report-only-dependencies true|false --workflows RESULT
                              --dependencies RESULT --out security-report.json
       security-run.sh enforce security-report.json"""
# The checks .github/security-window.json holds a flag for, each with the job whose Gate it governs.
GATE_JOBS = {"zizmor": "workflows", "dependencies": "dependencies"}
CHECKS = tuple(GATE_JOBS)
FLAG_TEXT = {True: "true", False: "false", None: "unknown"}
TOOL_ERROR = {
    "osv": {"total": None, "with_fix": None, "tool_error": True},
    "govulncheck": {"reachable": None, "informational": None, "tool_error": True},
    "zizmor": {"head_count": None, "new_count": None, "by_audit": None, "tool_error": True},
}
COUNTS = {"osv": ("total", "with_fix"), "govulncheck": ("reachable", "informational")}
GOVULNCHECK_RECORDS = ("config", "progress", "SBOM", "osv", "finding")


def fail(message):
    print(f"security-run.sh: {message}", file=sys.stderr)
    sys.exit(2)


def usage(message):
    fail(f"{message}\n{USAGE}")


def options(args, single, repeated=()):
    values = {flag: [] for flag in repeated}
    while args:
        flag = args.pop(0)
        if not args or (flag not in single and flag not in repeated):
            usage(f"unexpected argument {flag!r}")
        value = args.pop(0)
        if flag in repeated:
            values[flag].append(value)
        else:
            values[flag] = value
    return values


class Malformed(Exception):
    pass


def need(condition, what):
    if not condition:
        raise Malformed(what)


def is_count(value):
    return isinstance(value, int) and not isinstance(value, bool)


def read_text(path):
    try:
        with open(path, encoding="utf-8") as handle:
            return handle.read()
    except OSError as error:
        raise Malformed(f"cannot be read ({error.strerror})") from None


def read_json(path):
    try:
        return json.loads(read_text(path))
    except ValueError as error:
        raise Malformed(f"is not JSON ({error})") from None


def recorded(half, path, read, *args):
    """read(*args), or the half's tool error when PATH is not in its producer's shape."""
    try:
        return read(*args)
    except Malformed as error:
        print(f"security-run.sh: {half}: {path} {error}; recording a tool error", file=sys.stderr)
        return dict(TOOL_ERROR[half])


# --- base-commit and window-flags ---------------------------------------------------------------
def git(*args):
    return subprocess.run(["git", *args], capture_output=True, text=True)


def base_commit(args):
    """base-commit EVENT [MERGE_GROUP_BASE_SHA]: the commit a run's head is judged against."""
    if not args or len(args) > 2:
        usage("base-commit takes an event and, for merge_group, its base_sha")
    event = args[0]
    if event == "merge_group":
        if len(args) != 2 or not args[1]:
            fail("base-commit merge_group needs the merge group's base_sha")
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
    """window-flags FILE [--at REV]: the flags as the window job's $GITHUB_OUTPUT lines, notes on stderr."""
    if len(args) == 1:
        path, rev = args[0], None
    elif len(args) == 3 and args[1] == "--at":
        path, rev = args[0], args[2]
    else:
        usage("window-flags takes one file and, optionally, --at REV")
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


# --- summarize ----------------------------------------------------------------------------------
def osv_counts(path):
    document = read_json(path)
    need(isinstance(document, dict) and isinstance(document.get("results"), list), "has no results array")
    total = with_fix = 0
    for result in document["results"]:
        need(isinstance(result, dict) and isinstance(result.get("packages"), list), "has a result with no packages array")
        for package in result["packages"]:
            need(isinstance(package, dict) and isinstance(package.get("vulnerabilities"), list),
                 "has a package with no vulnerabilities array")
            for vulnerability in package["vulnerabilities"]:
                need(isinstance(vulnerability, dict) and isinstance(vulnerability.get("affected"), list),
                     "has a vulnerability with no affected array")
                fixed = False
                for affected in vulnerability["affected"]:
                    need(isinstance(affected, dict) and isinstance(affected.get("ranges", []), list),
                         "has an affected entry whose ranges is not an array")
                    for span in affected.get("ranges", []):
                        need(isinstance(span, dict) and isinstance(span.get("events"), list),
                             "has a range with no events array")
                        fixed = fixed or any(isinstance(event, dict) and "fixed" in event for event in span["events"])
                total += 1
                with_fix += fixed
    return {"total": total, "with_fix": with_fix, "tool_error": False}


def govulncheck_records(path):
    text, decoder, records, at = read_text(path), json.JSONDecoder(), [], 0
    while True:
        while at < len(text) and text[at].isspace():
            at += 1
        if at == len(text):
            return records
        try:
            record, at = decoder.raw_decode(text, at)
        except ValueError as error:
            raise Malformed(f"is not a JSON stream ({error})") from None
        records.append(record)


def govulncheck_module(path):
    """{OSV id: whether any of its findings is symbol-level} for one module's stream."""
    records = govulncheck_records(path)
    for record in records:
        need(isinstance(record, dict) and len(record) == 1 and next(iter(record)) in GOVULNCHECK_RECORDS,
             f"has a record keyed {sorted(record) if isinstance(record, dict) else record!r}, "
             f"not one of {', '.join(GOVULNCHECK_RECORDS)}")
    need(any("config" in record for record in records), "has no config record")
    reachable = {}
    for record in records:
        finding = record.get("finding")
        if finding is None:
            continue
        need(isinstance(finding, dict) and isinstance(finding.get("osv"), str)
             and isinstance(finding.get("trace"), list) and finding["trace"] and isinstance(finding["trace"][0], dict),
             "has a finding with no OSV id or no trace")
        reachable[finding["osv"]] = reachable.get(finding["osv"], False) or "function" in finding["trace"][0]
    return reachable


def govulncheck_counts(paths):
    entries = []
    for path in paths:
        try:
            entries += govulncheck_module(path).values()
        except Malformed as error:
            raise Malformed(f"{path} {error}") from None
    return {"reachable": sum(entries), "informational": len(entries) - sum(entries), "tool_error": False}


def summarize(args):
    opts = options(args, ("--osv", "--out"), ("--govulncheck",))
    if "--out" not in opts:
        usage("summarize needs --out")
    if "--osv" in opts:
        osv = recorded("osv", opts["--osv"], osv_counts, opts["--osv"])
    else:
        print("security-run.sh: osv: no --osv (osv-scanner did not run to completion); recording a tool error",
              file=sys.stderr)
        osv = dict(TOOL_ERROR["osv"])
    if opts["--govulncheck"]:
        try:
            govulncheck = govulncheck_counts(opts["--govulncheck"])
        except Malformed as error:
            print(f"security-run.sh: govulncheck: {error}; recording a tool error", file=sys.stderr)
            govulncheck = dict(TOOL_ERROR["govulncheck"])
    else:
        print("security-run.sh: govulncheck: no --govulncheck (govulncheck did not run in every module); "
              "recording a tool error", file=sys.stderr)
        govulncheck = dict(TOOL_ERROR["govulncheck"])
    write(opts["--out"], {"osv": osv, "govulncheck": govulncheck})


# --- report -------------------------------------------------------------------------------------
def zizmor_half(path):
    document = read_json(path)
    need(isinstance(document, dict), "is not an object")
    if "tool_error" in document:
        need(document["tool_error"] is True, "carries a tool_error that is not true")
        return dict(TOOL_ERROR["zizmor"])
    need(is_count(document.get("head_count")), "has no head_count")
    need("new_count" in document and (document["new_count"] is None or is_count(document["new_count"])),
         "has no new_count")
    head = document.get("head")
    need(isinstance(head, list) and all(isinstance(e, dict) and isinstance(e.get("ident"), str) for e in head),
         "has no head array of findings")
    need(len(head) == document["head_count"], "has a head_count that is not the length of head")
    by_audit = Counter(e["ident"] for e in head)
    return {"head_count": document["head_count"], "new_count": document["new_count"],
            "by_audit": dict(sorted(by_audit.items())), "tool_error": False}


def counts_half(half, section):
    need(isinstance(section, dict) and isinstance(section.get("tool_error"), bool), f"has no {half} half")
    if section["tool_error"]:
        return dict(TOOL_ERROR[half])
    counts = COUNTS[half]
    need(all(is_count(section.get(count)) for count in counts), f"has a {half} half without {' and '.join(counts)}")
    return {**{count: section[count] for count in counts}, "tool_error": False}


def deps_halves(path):
    """Each dependency half of deps-summary.json, or its tool error where it is not summarize's shape."""
    try:
        document = read_json(path)
        need(isinstance(document, dict), "is not an object")
    except Malformed as error:
        print(f"security-run.sh: osv and govulncheck: {path} {error}; recording a tool error", file=sys.stderr)
        return {half: dict(TOOL_ERROR[half]) for half in COUNTS}
    halves = {}
    for half in COUNTS:
        halves[half] = recorded(half, path, counts_half, half, document.get(half))
    return halves


def markdown(report):
    zizmor, osv, govulncheck = report["zizmor"], report["osv"], report["govulncheck"]
    if zizmor["tool_error"]:
        zizmor_row = "tool error"
    else:
        zizmor_row = f"{zizmor['head_count']} findings on this tree, " + (
            "no base to compare" if zizmor["new_count"] is None else f"{zizmor['new_count']} new against the base")
    flags = ", ".join(f"{check} {FLAG_TEXT[report['report_only'][check]]}" for check in CHECKS)
    return "\n".join([
        f"## Security (report_only: {flags})",
        "",
        "| check | result |",
        "| --- | --- |",
        f"| zizmor | {zizmor_row} |",
        "| zizmor by audit | " + ", ".join(f"{ident} {n}" for ident, n in (zizmor["by_audit"] or {}).items()) + " |",
        "| osv-scanner | " + ("tool error" if osv["tool_error"]
                             else f"{osv['total']} vulnerabilities, {osv['with_fix']} with a fixed version") + " |",
        "| govulncheck | " + ("tool error" if govulncheck["tool_error"]
                             else f"{govulncheck['reachable']} reachable, {govulncheck['informational']} informational")
        + " |",
    ])


def report(args):
    flags = ("--zizmor", "--deps", "--run-id", "--event", "--head", "--base", "--report-only-zizmor",
             "--report-only-dependencies", "--workflows", "--dependencies", "--out")
    opts = options(args, flags)
    missing = [flag for flag in flags if flag not in opts]
    if missing:
        usage(f"report needs {', '.join(missing)}")
    if not opts["--run-id"].isdigit():
        usage(f"--run-id takes a run id, not {opts['--run-id']!r}")
    for check in CHECKS:
        if opts[f"--report-only-{check}"] not in ("true", "false", ""):
            usage(f"--report-only-{check} takes true, false or nothing, not {opts[f'--report-only-{check}']!r}")
    zizmor = recorded("zizmor", opts["--zizmor"], zizmor_half, opts["--zizmor"])
    deps = deps_halves(opts["--deps"])
    result = {
        "run_id": int(opts["--run-id"]),
        "event": opts["--event"],
        "head_sha": opts["--head"],
        "base_sha": opts["--base"] or None,
        "report_only": {check: {"true": True, "false": False}.get(opts[f"--report-only-{check}"]) for check in CHECKS},
        "zizmor": zizmor,
        "osv": deps["osv"],
        "govulncheck": deps["govulncheck"],
        "tool_error": zizmor["tool_error"] or deps["osv"]["tool_error"] or deps["govulncheck"]["tool_error"],
        "gate": {"workflows": opts["--workflows"], "dependencies": opts["--dependencies"]},
    }
    write(opts["--out"], result)
    print(markdown(result))


def write(path, document):
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=2)
        handle.write("\n")


def enforce(args):
    if len(args) != 1:
        usage("enforce takes one security-report.json")
    try:
        document = read_json(args[0])
        need(isinstance(document, dict) and isinstance(document.get("report_only"), dict)
             and isinstance(document.get("gate"), dict), "has no report_only and gate objects")
        for check, job in GATE_JOBS.items():
            flag = document["report_only"].get(check, "missing")
            need(flag is None or isinstance(flag, bool), f"has no report_only.{check} of true, false or null")
            need(isinstance(document["gate"].get(job), str), f"has no gate.{job}")
    except Malformed as error:
        usage(f"enforce: {args[0]} {error}")
    failed = False
    for check, job in GATE_JOBS.items():
        flag, result = document["report_only"][check], document["gate"][job]
        if flag is not False:
            print(f"{check}: report-only (report_only.{check} is {FLAG_TEXT[flag]}); not enforced")
        elif result == "success":
            print(f"{check}: blocking, and its {job} job succeeded")
        else:
            print(f"::error::{check} is blocking (report_only.{check} is false) and its {job} job concluded {result}")
            failed = True
    sys.exit(1 if failed else 0)


args = sys.argv[1:]
command = args.pop(0) if args else None
if command == "base-commit":
    base_commit(args)
elif command == "window-flags":
    print_window_flags(args)
elif command == "summarize":
    summarize(args)
elif command == "report":
    report(args)
elif command == "enforce":
    enforce(args)
else:
    usage("the first argument is base-commit, window-flags, summarize, report or enforce")
PY
