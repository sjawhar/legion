#!/usr/bin/env bash
# Fingerprints zizmor's findings so a pull request is judged only on what it adds. The Security
# workflow (.github/workflows/security.yaml) audits the pull request's head and its base commit,
# each from its own tree root, and this script compares the two json-v1 outputs.
#
# Usage: zizmor-findings.sh --head head.json [--base base.json] --out findings.json [--annotate]
#
# A finding's fingerprint is sha256(ident NUL path NUL route NUL text), read from its Primary
# location (the first location when none is Primary):
#   - path: verbatim_path from `.github/` on, else without a leading `./`, so `./.github/x`,
#     `/tmp/base/.github/x` and `.github/x` agree whatever directory zizmor was given;
#   - route: the Key entries of the symbolic route joined with `/`; Index entries are dropped, so
#     a step inserted above does not renumber the findings below it;
#   - text: the first 120 characters of the whitespace-collapsed feature, only when the route runs
#     through `steps` — two identical steps differ only in their text, while a job- or
#     workflow-level finding is unique by its route and its span covers the whole job, whose text
#     changes whenever anything inside the job is edited.
# Rows never enter the fingerprint; `line` in the output is the Primary location's row + 1.
#
# The difference is a multiset: `new` lists each head occurrence of a fingerprint beyond the
# number of times the base carries it, so a second copy of an existing finding is one new
# finding and removing one of two copies adds none.
#
# Output (--out): {"head_count", "new_count", "head": [entry], "base": [entry] | null,
# "new": [entry] | null}, entry = {fingerprint, ident, severity, confidence, path, line, route};
# new_count, base and new are null without --base. With --annotate, one GitHub workflow command
# `::warning file=<path>,line=<line>,title=zizmor <ident>::<desc>` per new finding on stdout.
#
# Exit codes: 0 written; 2 a usage error or an input that is not a json-v1 array.
# CI runs its tests (zizmor-findings.test.sh) in the test job of pr-and-main.yaml.
set -euo pipefail

python3 - "$@" <<'PY'
import hashlib
import json
import sys
from collections import Counter

USAGE = "usage: zizmor-findings.sh --head head.json [--base base.json] --out findings.json [--annotate]"


def fail(message):
    print(f"zizmor-findings.sh: {message}", file=sys.stderr)
    print(USAGE, file=sys.stderr)
    sys.exit(2)


args = sys.argv[1:]
opts = {"--head": None, "--base": None, "--out": None}
annotate = False
while args:
    flag = args.pop(0)
    if flag == "--annotate":
        annotate = True
    elif flag in opts and args:
        opts[flag] = args.pop(0)
    else:
        fail(f"unexpected argument {flag!r}")
if not opts["--head"] or not opts["--out"]:
    fail("--head and --out are required")


def load(path):
    try:
        with open(path, encoding="utf-8") as handle:
            document = json.load(handle)
    except (OSError, ValueError) as error:
        fail(f"cannot read {path}: {error}")
    if not isinstance(document, list):
        fail(f"{path} is not a zizmor json-v1 array")
    return document


def repository_path(verbatim):
    at = verbatim.find(".github/")
    if at >= 0:
        return verbatim[at:]
    return verbatim[2:] if verbatim.startswith("./") else verbatim


def entry(finding):
    locations = finding["locations"]
    primary = next((loc for loc in locations if loc["symbolic"].get("kind") == "Primary"), locations[0])
    symbolic, concrete = primary["symbolic"], primary["concrete"]
    path = repository_path(symbolic["key"]["Local"]["verbatim_path"])
    keys = [str(part["Key"]) for part in symbolic["route"]["route"] if "Key" in part]
    route = "/".join(keys)
    text = " ".join(concrete.get("feature", "").split())[:120] if "steps" in keys else ""
    ident = finding["ident"]
    digest = hashlib.sha256("\0".join((ident, path, route, text)).encode("utf-8")).hexdigest()
    determinations = finding.get("determinations", {})
    return {
        "fingerprint": digest,
        "ident": ident,
        "severity": determinations.get("severity"),
        "confidence": determinations.get("confidence"),
        "path": path,
        "line": concrete["location"]["start_point"]["row"] + 1,
        "route": route,
    }, finding.get("desc", "")


def escape_data(value):
    return value.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def escape_property(value):
    return escape_data(value).replace(":", "%3A").replace(",", "%2C")


head = [entry(finding) for finding in load(opts["--head"])]
result = {"head_count": len(head), "new_count": None, "head": [e for e, _ in head], "base": None, "new": None}

new = []
if opts["--base"]:
    base = [e for e, _ in (entry(finding) for finding in load(opts["--base"]))]
    allowance = Counter(e["fingerprint"] for e in base)
    for found, desc in head:
        if allowance[found["fingerprint"]] > 0:
            allowance[found["fingerprint"]] -= 1
        else:
            new.append((found, desc))
    result.update(base=base, new=[e for e, _ in new], new_count=len(new))

with open(opts["--out"], "w", encoding="utf-8") as handle:
    json.dump(result, handle, indent=2)
    handle.write("\n")

if annotate:
    for found, desc in new:
        print(
            f"::warning file={escape_property(found['path'])},line={found['line']},"
            f"title={escape_property('zizmor ' + found['ident'])}::{escape_data(desc)}"
        )
PY
