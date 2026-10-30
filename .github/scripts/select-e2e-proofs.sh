#!/usr/bin/env bash
# Which scripts/e2e proofs one run of .github/workflows/e2e.yaml makes. The answer is the matrix
# that workflow's proof job runs: `proofs` in $GITHUB_OUTPUT, one entry per proof. e2e.yaml's
# select job runs this script, and e2e.yaml's header says what .github/e2e-scripts.json's two
# lists hold and why a refused script is a failed job.
#
# This script reads three variables: EVENT, the event that started the run; REQUESTED, the script
# a `workflow_dispatch` asks for, with or without its `.sh`; and GITHUB_OUTPUT, the file the
# matrix is written to. A `workflow_dispatch` selects the one row it names. Every other event
# selects every row of `hosted`, in name order.
#
# Every refusal names the file, the row and the remedy, and no input reaches a traceback. The
# script prints one `::error::` line per problem and exits 1 on:
#
#   1. a list file it cannot read, that is no JSON, or whose top level is not an object with a
#      `hosted` list and an `elsewhere` list;
#   2. a row that is not an object, or that has no `script` string;
#   3. a hosted row that lacks a key the matrix carries, or that holds the wrong kind of value
#      for one. MATRIX below is the one list of those keys and of the kind each one holds, so a
#      key added there is a key the validation then demands of every hosted row;
#   4. an `elsewhere` row with no `needs` text, which is the text a refusal of that script prints;
#   5. a script under scripts/e2e/ that neither list names, or a row naming a script the tree
#      does not hold;
#   6. a `workflow_dispatch` for a script of the `elsewhere` list, with what that script's own
#      header says it needs, or a `workflow_dispatch` for a name in neither list.
#
# Run from anywhere: .github/scripts/select-e2e-proofs.sh
# e2e.yaml's select job runs it, and its tests run in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import collections
import json
import os
import pathlib

LIST_FILE = ".github/e2e-scripts.json"
PROOF_DIR = "scripts/e2e"

Key = collections.namedtuple("Key", "name kind holds carry")


def a_string(value):
    return isinstance(value, str)


def a_boolean(value):
    return isinstance(value, bool)


def a_whole_number(value):
    # Python counts a boolean as an int, and `"timeout_minutes": true` is no limit.
    return isinstance(value, int) and not isinstance(value, bool)


def a_list_of_strings(value):
    return isinstance(value, list) and all(isinstance(item, str) for item in value)


def as_given(value):
    return value


# The matrix e2e.yaml's proof job runs: one line per key a hosted row gives it, with the kind of
# value that key holds and how the entry carries it. This is the one list of those keys. A key
# added here is a key every hosted row must then carry, with a value of that kind.
MATRIX = (
    Key("script", "a string", a_string, as_given),
    Key("go_version_file", "a string", a_string, as_given),
    Key("apt", "a list of strings", a_list_of_strings, " ".join),
    Key("bun", "a boolean", a_boolean, as_given),
    Key("jj", "a boolean", a_boolean, as_given),
    Key("evidence_variable", "a string", a_string, as_given),
    Key("timeout_minutes", "a whole number", a_whole_number, as_given),
)

# What each list holds, and the order the rows are read in.
LISTS = {
    "hosted": "one row for every scripts/e2e/*.sh a GitHub-hosted runner can run",
    "elsewhere": "one row for every other scripts/e2e/*.sh, with the needs its own header states",
}

KINDS = {
    dict: "an object",
    list: "a list",
    str: "a string",
    bool: "a boolean",
    int: "a whole number",
    float: "a number",
    type(None): "null",
}


def kind_of(value):
    return KINDS.get(type(value), "a value this script cannot name")


def refuse(*problems, about=LIST_FILE):
    """Print one error line per problem, and fail. `about` is the file a reader must open."""
    prefix = f"::error file={about}::" if about else "::error::"
    for problem in problems:
        print(f"{prefix}{problem}")
    raise SystemExit(1)


event = os.environ.get("EVENT", "")
output = os.environ.get("GITHUB_OUTPUT", "")
if not event:
    refuse("EVENT is empty. Give this script the event that started the run, as e2e.yaml's "
           "select job does.", about=None)
if not output:
    refuse("GITHUB_OUTPUT is empty. Give this script the file to write the matrix to, as a "
           "GitHub runner does.", about=None)

try:
    text = pathlib.Path(LIST_FILE).read_text(encoding="utf-8")
except OSError as error:
    refuse(f"this script cannot read the file ({error.strerror}). Write it as an object with a "
           f'"hosted" list and an "elsewhere" list.')

try:
    data = json.loads(text)
except json.JSONDecodeError as error:
    refuse(f"this file is no JSON: {error}. Fix the syntax at that line.")

if not isinstance(data, dict):
    refuse(f"this file holds {kind_of(data)} at the top level. Write an object with a "
           f'"hosted" list and an "elsewhere" list.')

problems = [f'this file has no "{name}" list. Write "{name}" as a list of rows, {remedy}.'
            for name, remedy in LISTS.items() if not isinstance(data.get(name), list)]
if problems:
    refuse(*problems)

# Each list by script name, as the rest of this script reads it, and every row the shape of a row.
rows = {name: {} for name in LISTS}
problems = []
for name in LISTS:
    for index, row in enumerate(data[name]):
        if not isinstance(row, dict):
            problems.append(f"{name}[{index}] is {kind_of(row)}, not an object. Write every row "
                            f'as an object whose "script" names a file of {PROOF_DIR}/.')
            continue
        if not isinstance(row.get("script"), str):
            problems.append(f'{name}[{index}] has no "script" string. Give the row a "script" '
                            f"naming a file of {PROOF_DIR}/.")
            continue
        rows[name][row["script"]] = row

every_key = ", ".join(key.name for key in MATRIX)
for script, row in rows["hosted"].items():
    missing = [key.name for key in MATRIX if key.name not in row]
    if missing:
        problems.append(f"hosted row {script} lacks {', '.join(missing)}. Add each one: the "
                        f"matrix e2e.yaml's proof job runs reads {every_key}.")
    for key in MATRIX:
        if key.name in row and not key.holds(row[key.name]):
            problems.append(f"hosted row {script} has {key.name} {row[key.name]!r}, which is not "
                            f"{key.kind}. Write {key.name} as {key.kind}.")

for script, row in rows["elsewhere"].items():
    if not isinstance(row.get("needs"), str) or not row["needs"].strip():
        problems.append(f"elsewhere row {script} has no needs text. Write what the script's own "
                        f"header says it needs: e2e.yaml prints that text when it refuses this "
                        f"script.")

if problems:
    refuse(*problems)

hosted = rows["hosted"]
elsewhere = rows["elsewhere"]
listed = set(hosted) | set(elsewhere)
present = sorted(found.name for found in pathlib.Path(PROOF_DIR).glob("*.sh"))

problems = [
    f"{PROOF_DIR}/{name} is in neither list: add it to `hosted` with what a job must "
    f"give it, or to `elsewhere` with what its own header says it needs"
    for name in present
    if name not in listed
] + [
    f"this file names {name}, and {PROOF_DIR}/ does not hold it: delete its row"
    for name in sorted(listed)
    if name not in present
]
if problems:
    refuse(*problems)

if event == "workflow_dispatch":
    asked = os.environ.get("REQUESTED", "").strip()
    name = asked if asked.endswith(".sh") else f"{asked}.sh"
    if name in elsewhere:
        refuse(f"{PROOF_DIR}/{name} needs {elsewhere[name]['needs']}. A GitHub-hosted runner "
               f"gives none of that, so this workflow runs it nowhere.", about=None)
    if name not in hosted:
        refuse(f"{PROOF_DIR}/{name} is no proof a GitHub-hosted runner can run. Name one of: "
               f"{', '.join(sorted(hosted))}.", about=None)
    chosen = [hosted[name]]
else:
    chosen = [hosted[name] for name in sorted(hosted)]

proofs = []
for row in chosen:
    entry = {key.name: key.carry(row[key.name]) for key in MATRIX}
    entry["proof"] = entry["script"][: -len(".sh")]
    proofs.append(entry)

with open(output, "a", encoding="utf-8") as out:
    out.write(f"proofs={json.dumps(proofs)}\n")
print(f"{len(proofs)} proof(s): {', '.join(row['script'] for row in proofs)}")
PY
