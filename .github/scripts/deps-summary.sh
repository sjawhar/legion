#!/usr/bin/env bash
# The dependency scanners' numbers and each Security run's report (.github/workflows/security.yaml).
#
# Usage:
#   deps-summary.sh summarize [--osv osv.json] [--govulncheck govulncheck.json]… --out deps-summary.json
#   deps-summary.sh report --zizmor zizmor-findings.json --deps deps-summary.json --run-id ID
#       --event EVENT --head SHA --base SHA --report-only-zizmor true|false
#       --report-only-dependencies true|false --workflows RESULT --dependencies RESULT
#       --out security-report.json
#   deps-summary.sh enforce security-report.json
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
# Exit codes: 0 written or nothing to enforce; 1 a blocking check's job did not succeed; 2 a usage
# error, or a report enforce cannot read.
# CI runs its tests (deps-summary.test.sh) in the test job of pr-and-main.yaml.
set -euo pipefail

python3 - "$@" <<'PY'
import json
import sys
from collections import Counter

USAGE = """usage: deps-summary.sh summarize [--osv osv.json] [--govulncheck govulncheck.json]... --out deps-summary.json
       deps-summary.sh report --zizmor zizmor-findings.json --deps deps-summary.json --run-id ID --event EVENT
                              --head SHA --base SHA --report-only-zizmor true|false
                              --report-only-dependencies true|false --workflows RESULT
                              --dependencies RESULT --out security-report.json
       deps-summary.sh enforce security-report.json"""
GATE_JOBS = {"zizmor": "workflows", "dependencies": "dependencies"}
CHECKS = ("zizmor", "dependencies")
FLAG_TEXT = {True: "true", False: "false", None: "unknown"}
TOOL_ERROR = {
    "osv": {"total": None, "with_fix": None, "tool_error": True},
    "govulncheck": {"reachable": None, "informational": None, "tool_error": True},
    "zizmor": {"head_count": None, "new_count": None, "by_audit": None, "tool_error": True},
}
COUNTS = {"osv": ("total", "with_fix"), "govulncheck": ("reachable", "informational")}
GOVULNCHECK_RECORDS = ("config", "progress", "SBOM", "osv", "finding")


def usage(message):
    print(f"deps-summary.sh: {message}\n{USAGE}", file=sys.stderr)
    sys.exit(2)


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
        print(f"deps-summary.sh: {half}: {path} {error}; recording a tool error", file=sys.stderr)
        return dict(TOOL_ERROR[half])


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
        print("deps-summary.sh: osv: no --osv (osv-scanner did not run to completion); recording a tool error",
              file=sys.stderr)
        osv = dict(TOOL_ERROR["osv"])
    if opts["--govulncheck"]:
        try:
            govulncheck = govulncheck_counts(opts["--govulncheck"])
        except Malformed as error:
            print(f"deps-summary.sh: govulncheck: {error}; recording a tool error", file=sys.stderr)
            govulncheck = dict(TOOL_ERROR["govulncheck"])
    else:
        print("deps-summary.sh: govulncheck: no --govulncheck (govulncheck did not run in every module); "
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
        print(f"deps-summary.sh: osv and govulncheck: {path} {error}; recording a tool error", file=sys.stderr)
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
if command == "summarize":
    summarize(args)
elif command == "report":
    report(args)
elif command == "enforce":
    enforce(args)
else:
    usage("the first argument is summarize, report or enforce")
PY
