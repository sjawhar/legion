#!/usr/bin/env bash
# Tests for `select-e2e-proofs.sh`: the matrix e2e.yaml's proof job runs, and the refusal a line
# this script cannot read gets. The discipline this suite holds is that no input reaches a
# traceback: every refusal names the file, the row and the remedy, so the case that proves a
# hosted row with `jj` deleted is refused by name also asserts that the output holds no
# `Traceback`.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file e2e.yaml runs. The fixtures' scripts and
# rows are invented. One case runs the script over the repository's own
# .github/e2e-scripts.json and scripts/e2e/, so the real data is checked too.
#
# Run from anywhere: .github/scripts/select-e2e-proofs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/select-e2e-proofs.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: a tree that passes — two hosted proofs, one of them with packages and an
# evidence variable, one proof of the `elsewhere` list, and a scripts/e2e/ holding all three.
# Echoes the fixture root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/scripts/e2e"
  ln -s "$check_script" "$root/.github/scripts/select-e2e-proofs.sh"
  local name
  for name in alpha-proof beta-proof gamma-proof; do
    printf '#!/usr/bin/env bash\necho %s\n' "$name" > "$root/scripts/e2e/$name.sh"
  done
  cat > "$root/.github/e2e-scripts.json" <<'JSON'
{
  "hosted": [
    {
      "script": "alpha-proof.sh",
      "go_version_file": "packages/example/go.mod",
      "apt": [
        "tmux",
        "postgresql-client"
      ],
      "bun": true,
      "evidence_variable": "ALPHA_PROOF_EVIDENCE_DIR",
      "timeout_minutes": 25,
      "why_hosted": "it takes a checkout, go and the job's database, and starts no container",
      "jj": true
    },
    {
      "script": "beta-proof.sh",
      "go_version_file": "packages/example/go.mod",
      "apt": [],
      "bun": false,
      "evidence_variable": "",
      "timeout_minutes": 20,
      "why_hosted": "it takes a checkout and go, and keeps its work directory",
      "jj": false
    }
  ],
  "elsewhere": [
    {
      "script": "gamma-proof.sh",
      "needs": "a throwaway cluster on an amd64 Linux host and a worker image by digest"
    }
  ]
}
JSON
  echo "$root"
}

# run_select <root> <event> [requested]: runs the fixture's copy of the script. Sets out, status
# and proofs, the matrix the run wrote to its GITHUB_OUTPUT.
run_select() {
  local root=$1 event=$2 requested=${3:-}
  : > "$root/github-output"
  set +e
  out=$(EVENT="$event" REQUESTED="$requested" GITHUB_OUTPUT="$root/github-output" \
    "$root/.github/scripts/select-e2e-proofs.sh" 2>&1)
  status=$?
  set -e
  proofs=$(sed -n 's/^proofs=//p' "$root/github-output")
}

# write_row_key <root> <list> <index> <key> <json value>: sets one key of one row.
write_row_key() {
  python3 - "$1/.github/e2e-scripts.json" "$2" "$3" "$4" "$5" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data[sys.argv[2]][int(sys.argv[3])][sys.argv[4]] = json.loads(sys.argv[5])
path.write_text(json.dumps(data, indent=2) + "\n")
PY
}

# drop_row_key <root> <list> <index> <key>: deletes one key of one row.
drop_row_key() {
  python3 - "$1/.github/e2e-scripts.json" "$2" "$3" "$4" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
del data[sys.argv[2]][int(sys.argv[3])][sys.argv[4]]
path.write_text(json.dumps(data, indent=2) + "\n")
PY
}

# write_row <root> <list> <index> <json row>: replaces one whole row.
write_row() {
  python3 - "$1/.github/e2e-scripts.json" "$2" "$3" "$4" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
data = json.loads(path.read_text())
data[sys.argv[2]][int(sys.argv[3])] = json.loads(sys.argv[4])
path.write_text(json.dumps(data, indent=2) + "\n")
PY
}

# write_file <root> <text>: replaces the list file with text of any shape, JSON or not.
write_file() {
  printf '%s\n' "$2" > "$1/.github/e2e-scripts.json"
}

# proof_count: how many entries the run wrote.
proof_count() {
  python3 -c 'import json, sys; print(len(json.loads(sys.argv[1])))' "$proofs"
}

# proof_scripts: every entry's script, in the order the run wrote them.
proof_scripts() {
  python3 -c 'import json, sys; print(" ".join(e["script"] for e in json.loads(sys.argv[1])))' "$proofs"
}

# proof_keys <index>: one entry's keys, sorted and comma-separated.
proof_keys() {
  python3 -c 'import json, sys; print(",".join(sorted(json.loads(sys.argv[1])[int(sys.argv[2])])))' \
    "$proofs" "$1"
}

# proof_field <index> <key>: one key of one entry. A string is printed as it stands, every other
# value in its JSON form, so a boolean reads true or false.
proof_field() {
  python3 -c 'import json, sys
value = json.loads(sys.argv[1])[int(sys.argv[2])][sys.argv[3]]
print(value if isinstance(value, str) else json.dumps(value))' "$proofs" "$1" "$2"
}

echo "case: a workflow_dispatch of a hosted script emits one matrix entry with every key"
root=$(fixture dispatch-green)
run_select "$root" workflow_dispatch alpha-proof
check "exits 0" "$(is "$status" 0)"
check "says what it selected" "$(contains "$out" '^1 proof(s): alpha-proof.sh$')"
check "one entry" "$(is "$(proof_count)" 1)"
check "the entry carries every key the matrix reads" \
  "$(is "$(proof_keys 0)" "apt,bun,evidence_variable,go_version_file,jj,proof,script,timeout_minutes")"
check "script is the row's" "$(is "$(proof_field 0 script)" alpha-proof.sh)"
check "proof is that name without its .sh" "$(is "$(proof_field 0 proof)" alpha-proof)"
check "go_version_file is the row's" "$(is "$(proof_field 0 go_version_file)" packages/example/go.mod)"
check "apt is the row's list, joined by spaces" "$(is "$(proof_field 0 apt)" "tmux postgresql-client")"
check "bun is a boolean" "$(is "$(proof_field 0 bun)" true)"
check "jj is a boolean" "$(is "$(proof_field 0 jj)" true)"
check "evidence_variable is the row's" "$(is "$(proof_field 0 evidence_variable)" ALPHA_PROOF_EVIDENCE_DIR)"
check "timeout_minutes is a whole number" "$(is "$(proof_field 0 timeout_minutes)" 25)"
run_select "$root" workflow_dispatch alpha-proof.sh
check "a name given with its .sh selects the same row" "$(is "$(proof_field 0 script)" alpha-proof.sh)"
run_select "$root" workflow_dispatch "  alpha-proof  "
check "a name given with spaces around it selects the same row" "$(is "$(proof_field 0 script)" alpha-proof.sh)"

echo "case: a label run and the nightly emit every hosted row"
root=$(fixture every-row)
run_select "$root" schedule
check "exits 0" "$(is "$status" 0)"
check "every hosted row, in name order" "$(is "$(proof_scripts)" "alpha-proof.sh beta-proof.sh")"
check "says how many and which" "$(contains "$out" '^2 proof(s): alpha-proof.sh, beta-proof.sh$')"
check "a row with no package carries an empty apt" "$(is "$(proof_field 1 apt)" "")"
check "and the row's own limit" "$(is "$(proof_field 1 timeout_minutes)" 20)"
run_select "$root" pull_request
check "the e2e label selects the same rows" "$(is "$(proof_scripts)" "alpha-proof.sh beta-proof.sh")"

echo "case: a hosted row with jj deleted is refused by name, with no traceback"
root=$(fixture missing-jj)
drop_row_key "$root" hosted 0 jj
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file and the row" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::hosted row alpha-proof.sh lacks jj\.')"
check "names the remedy and every key the matrix reads" \
  "$(contains "$out" 'Add each one: the matrix e2e.yaml.s proof job runs reads script, go_version_file, apt, bun, jj, evidence_variable, timeout_minutes\.$')"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"
check "and writes no matrix" "$(is "$proofs" "")"

echo "case: a hosted row missing several keys names every one of them"
root=$(fixture missing-keys)
drop_row_key "$root" hosted 1 apt
drop_row_key "$root" hosted 1 timeout_minutes
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the row and both keys" "$(contains "$out" 'hosted row beta-proof.sh lacks apt, timeout_minutes\.')"

echo "case: a hosted key holding the wrong kind of value"
root=$(fixture wrong-timeout)
write_row_key "$root" hosted 0 timeout_minutes '"25"'
run_select "$root" schedule
check "a quoted timeout_minutes fails" "$(is "$status" 1)"
check "names the row, the key and what it must hold" \
  "$(contains "$out" "^::error file=.github/e2e-scripts.json::hosted row alpha-proof.sh has timeout_minutes '25', which is not a whole number. Write timeout_minutes as a whole number.$")"

root=$(fixture boolean-timeout)
write_row_key "$root" hosted 0 timeout_minutes true
run_select "$root" schedule
check "a boolean timeout_minutes fails, though Python counts one as a number" "$(is "$status" 1)"
check "and is named" "$(contains "$out" 'has timeout_minutes True, which is not a whole number')"

root=$(fixture apt-string)
write_row_key "$root" hosted 0 apt '"tmux"'
run_select "$root" schedule
check "an apt that is one string fails" "$(is "$status" 1)"
check "and is named" "$(contains "$out" "has apt 'tmux', which is not a list of strings. Write apt as a list of strings.")"

root=$(fixture apt-numbers)
write_row_key "$root" hosted 0 apt '["tmux", 7]'
run_select "$root" schedule
check "an apt holding a number fails" "$(is "$status" 1)"
check "and is named" "$(contains "$out" "has apt \['tmux', 7\], which is not a list of strings")"

root=$(fixture bun-string)
write_row_key "$root" hosted 0 bun '"yes"'
run_select "$root" schedule
check "a bun that is a string fails" "$(is "$status" 1)"
check "and is named" "$(contains "$out" "has bun 'yes', which is not a boolean. Write bun as a boolean.")"

root=$(fixture jj-number)
write_row_key "$root" hosted 0 jj 1
run_select "$root" schedule
check "a jj that is a number fails" "$(is "$status" 1)"
check "and is named" "$(contains "$out" 'has jj 1, which is not a boolean. Write jj as a boolean.')"

echo "case: a row that is not an object, or that has no script string"
root=$(fixture row-not-object)
write_row "$root" hosted 0 '["alpha-proof.sh"]'
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the list, the row and the remedy" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::hosted\[0\] is a list, not an object. Write every row as an object whose "script" names a file of scripts/e2e/.$')"

root=$(fixture row-no-script)
drop_row_key "$root" elsewhere 0 script
run_select "$root" schedule
check "a row with no script fails" "$(is "$status" 1)"
check "and names the list, the row and the remedy" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::elsewhere\[0\] has no "script" string. Give the row a "script" naming a file of scripts/e2e/.$')"

root=$(fixture script-not-string)
write_row_key "$root" hosted 1 script 7
run_select "$root" schedule
check "a script that is no string fails" "$(is "$status" 1)"
check "and names the row" "$(contains "$out" 'hosted\[1\] has no "script" string')"

echo "case: an elsewhere row with no needs text"
root=$(fixture no-needs)
drop_row_key "$root" elsewhere 0 needs
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the row, the remedy and what the text is for" \
  "$(contains "$out" "^::error file=.github/e2e-scripts.json::elsewhere row gamma-proof.sh has no needs text. Write what the script's own header says it needs: e2e.yaml prints that text when it refuses this script.$")"

root=$(fixture blank-needs)
write_row_key "$root" elsewhere 0 needs '"   "'
run_select "$root" schedule
check "needs holding spaces alone fails too" "$(is "$status" 1)"
check "and is named" "$(contains "$out" 'elsewhere row gamma-proof.sh has no needs text')"

echo "case: a file that is not JSON, or that this script cannot read"
root=$(fixture not-json)
write_file "$root" '{"hosted": ['
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the file and where the syntax breaks" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::this file is no JSON: .*line [0-9]* column [0-9]*.*\. Fix the syntax at that line\.$')"
check "and reaches no traceback" "$(is "$(contains "$out" Traceback)" false)"

root=$(fixture no-file)
rm "$root/.github/e2e-scripts.json"
run_select "$root" schedule
check "a missing file fails" "$(is "$status" 1)"
check "and names the file and what to write" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::this script cannot read the file (No such file or directory). Write it as an object with a "hosted" list and an "elsewhere" list.$')"

echo "case: a top level that is not an object holding both lists"
root=$(fixture top-level-list)
write_file "$root" '[]'
run_select "$root" schedule
check "a list at the top level fails" "$(is "$status" 1)"
check "and names what to write" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::this file holds a list at the top level. Write an object with a "hosted" list and an "elsewhere" list.$')"

root=$(fixture no-hosted)
write_file "$root" '{"elsewhere": []}'
run_select "$root" schedule
check "a file with no hosted list fails" "$(is "$status" 1)"
check "and names that list and what it holds" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::this file has no "hosted" list. Write "hosted" as a list of rows, one row for every scripts/e2e/\*.sh a GitHub-hosted runner can run.$')"

root=$(fixture elsewhere-not-list)
write_file "$root" '{"hosted": [], "elsewhere": {}}'
run_select "$root" schedule
check "an elsewhere that is an object fails" "$(is "$status" 1)"
check "and names that list and what it holds" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::this file has no "elsewhere" list. Write "elsewhere" as a list of rows, one row for every other scripts/e2e/\*.sh, with the needs its own header states.$')"

echo "case: a script under scripts/e2e/ that neither list names"
root=$(fixture unlisted-script)
printf '#!/usr/bin/env bash\necho delta\n' > "$root/scripts/e2e/delta-proof.sh"
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the script and both remedies" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::scripts/e2e/delta-proof.sh is in neither list: add it to .hosted. with what a job must give it, or to .elsewhere. with what its own header says it needs$')"

echo "case: a row naming a script the tree does not hold"
root=$(fixture stale-row)
rm "$root/scripts/e2e/gamma-proof.sh"
run_select "$root" schedule
check "fails" "$(is "$status" 1)"
check "names the row and the remedy" \
  "$(contains "$out" '^::error file=.github/e2e-scripts.json::this file names gamma-proof.sh, and scripts/e2e/ does not hold it: delete its row$')"

echo "case: a workflow_dispatch for a script of the elsewhere list"
root=$(fixture dispatch-elsewhere)
run_select "$root" workflow_dispatch gamma-proof
check "fails" "$(is "$status" 1)"
check "names the script, what it needs and why no job runs it" \
  "$(contains "$out" '^::error::scripts/e2e/gamma-proof.sh needs a throwaway cluster on an amd64 Linux host and a worker image by digest. A GitHub-hosted runner gives none of that, so this workflow runs it nowhere.$')"

echo "case: a workflow_dispatch for a name in neither list"
root=$(fixture dispatch-unknown)
run_select "$root" workflow_dispatch nothing-here
check "fails" "$(is "$status" 1)"
check "names the script and every name it could have been given" \
  "$(contains "$out" '^::error::scripts/e2e/nothing-here.sh is no proof a GitHub-hosted runner can run. Name one of: alpha-proof.sh, beta-proof.sh.$')"
run_select "$root" workflow_dispatch ""
check "a dispatch naming nothing fails" "$(is "$status" 1)"
check "and is refused by name, with no traceback" \
  "$(contains "$out" '^::error::scripts/e2e/.sh is no proof a GitHub-hosted runner can run')"
check "  no traceback" "$(is "$(contains "$out" Traceback)" false)"

echo "case: the environment e2e.yaml's select job sets"
root=$(fixture environment)
set +e
out=$(env -u EVENT GITHUB_OUTPUT="$root/github-output" \
  "$root/.github/scripts/select-e2e-proofs.sh" 2>&1)
status=$?
set -e
check "an unset EVENT fails" "$(is "$status" 1)"
check "and is named, with no traceback" "$(contains "$out" '^::error::EVENT is empty\.')"
check "  no traceback" "$(is "$(contains "$out" Traceback)" false)"
set +e
out=$(env -u GITHUB_OUTPUT EVENT=schedule \
  "$root/.github/scripts/select-e2e-proofs.sh" 2>&1)
status=$?
set -e
check "an unset GITHUB_OUTPUT fails" "$(is "$status" 1)"
check "and is named, with no traceback" "$(contains "$out" '^::error::GITHUB_OUTPUT is empty\.')"
check "  no traceback" "$(is "$(contains "$out" Traceback)" false)"

# The real data, through the real script: every hosted row of .github/e2e-scripts.json against the
# scripts/e2e/ of this repository.
hosted_scripts=$(python3 -c 'import json, pathlib, sys
rows = json.loads(pathlib.Path(sys.argv[1]).read_text())["hosted"]
print(" ".join(sorted(row["script"] for row in rows)))' "$script_dir/../e2e-scripts.json")
echo "case: the repository's own .github/e2e-scripts.json and scripts/e2e/ ($hosted_scripts)"
real_output="$work/real-output"
: > "$real_output"
set +e
out=$(EVENT=schedule REQUESTED= GITHUB_OUTPUT="$real_output" "$check_script" 2>&1)
status=$?
set -e
proofs=$(sed -n 's/^proofs=//p' "$real_output")
check "a schedule run of the repository's own data passes" "$(is "$status" 0)"
check "and emits every hosted row" "$(is "$(proof_scripts)" "$hosted_scripts")"
for name in $hosted_scripts; do
  : > "$real_output"
  set +e
  out=$(EVENT=workflow_dispatch REQUESTED="$name" GITHUB_OUTPUT="$real_output" "$check_script" 2>&1)
  status=$?
  set -e
  proofs=$(sed -n 's/^proofs=//p' "$real_output")
  check "a workflow_dispatch of $name passes" "$(is "$status" 0)"
  check "  and emits that row alone" "$(is "$(proof_scripts)" "$name")"
  check "  with every key the matrix reads" \
    "$(is "$(proof_keys 0)" "apt,bun,evidence_variable,go_version_file,jj,proof,script,timeout_minutes")"
done

summary "select-e2e-proofs.sh"
