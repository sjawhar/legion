#!/usr/bin/env bash
# Tests for `main-bypass-check.sh`: which commit of a push to main reached it untested, and
# whether the bypass check must start a run of ci.yaml for it.
#
# main-bypass-check.sh reads only its environment, so each case hands it the commits
# `github.event.commits` would carry for a push of its own, the merge-group runs `gh run list`
# would give at each of those commits, and the ci-ok check runs the check-runs read would give.
# The fixtures carry the real shapes: 40-character shas, whole commit messages with their bodies,
# and the fields of each read. The script under test is the file CI runs, and every case runs it
# under `timeout`, so a decision that hung would fail its case instead of hanging the job.
#
# Run from anywhere: .github/scripts/main-bypass-check.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Four commits of a push, and a fifth that is the head of another group.
queued=1dc54c7e8a9b0f2d3c4e5a6b7c8d9e0f1a2b3c4d
queued_two=2e7f1a3b5c7d9e1f3a5b7c9d1e3f5a7b9c1d3e5f
bypassed=3f8a2b4c6d8e0f2a4b6c8d0e2f4a6b8c0d2e4f6a
bypassed_two=4a9b3c5d7e9f1a3b5c7d9e1f3a5b7c9d1e3f5a7b
elsewhere=5b0c4d6e8f0a2b4c6d8e0f2a4b6c8d0e2f4a6b8c

# commit <sha> <message>: one entry of the push's commits, as github.event.commits carries it.
commit() { jq -n --arg id "$1" --arg message "$2" '{id: $id, message: $message}'; }

# list <json>...: the JSON array of them, in the order given.
list() { printf '%s\n' "$@" | jq -s '.'; }

# mg_run <head sha> <conclusion>: one row of
# `gh run list --workflow ci.yaml --event merge_group --json conclusion,headSha,workflowName`.
# An empty conclusion is null, as gh gives it for a run that has not completed.
mg_run() {
  jq -n --arg head "$1" --arg conclusion "$2" \
    '{conclusion: (if $conclusion == "" then null else $conclusion end),
      headSha: $head,
      workflowName: "CI"}'
}

# ci_ok_run <conclusion>: one ci-ok check run of the check-runs read, conclusion as it gives it.
ci_ok_run() {
  jq -n --arg conclusion "$1" \
    '{name: "ci-ok", conclusion: (if $conclusion == "" then null else $conclusion end)}'
}

# readings <sha> <json array> ...: the RUNS or CI_OK object those pairs make.
readings() {
  local out='{}'
  while [ "$#" -gt 0 ]; do
    out=$(jq -c --arg sha "$1" --argjson value "$2" '. + {($sha): $value}' <<<"$out")
    shift 2
  done
  echo "$out"
}

# run_bypass <commits> <runs> <ci-ok map>: sets out (stdout, the token), err (stderr, the
# reasoning) and status.
run_bypass() {
  set +e
  out=$(COMMITS=$1 RUNS=$2 CI_OK=$3 timeout 60 "$script_dir/main-bypass-check.sh" 2>"$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

echo "case: a push whose every commit came through the merge queue starts nothing"
queue_commits=$(list \
  "$(commit "$queued" "feat(daemon): the keeper retries the claim")" \
  "$(commit "$queued_two" "fix(envoy): the listener closes its held port")")
queue_runs=$(readings \
  "$queued" "$(list "$(mg_run "$queued" success)")" \
  "$queued_two" "$(list "$(mg_run "$queued_two" success)")")
queue_ci_ok=$(readings \
  "$queued" "$(list "$(ci_ok_run success)")" \
  "$queued_two" "$(list "$(ci_ok_run success)")")
run_bypass "$queue_commits" "$queue_runs" "$queue_ci_ok"
check "decides" "$(is "$status" 0)"
check "starts nothing" "$(is "$out" "")"
check "says what it read of the first commit" \
  "$(contains "$err" "^commit $queued (feat(daemon): the keeper retries the claim): it is the head of 1 ci.yaml merge-group run(s) (CI success) and its 1 ci-ok check run(s) concluded success: tested\$")"
check "says the push needs no run" \
  "$(contains "$err" '^every commit of this push was tested on the merge queue or carries a skip marker: no run is started$')"

echo "case: a commit that reached main with no merge-group run and no ci-ok is named"
bypass_commits=$(list \
  "$(commit "$queued" "feat(daemon): the keeper retries the claim")" \
  "$(commit "$bypassed" "fix(ci): an admin merge, pushed past the queue")")
bypass_runs=$(readings \
  "$queued" "$(list "$(mg_run "$queued" success)")" \
  "$bypassed" '[]')
bypass_ci_ok=$(readings "$queued" "$(list "$(ci_ok_run success)")" "$bypassed" '[]')
run_bypass "$bypass_commits" "$bypass_runs" "$bypass_ci_ok"
check "decides" "$(is "$status" 0)"
check "names that commit, and only it" "$(is "$out" "dispatch $bypassed")"
check "says what is missing" \
  "$(contains "$err" "^commit $bypassed (fix(ci): an admin merge, pushed past the queue): it is the head of no ci.yaml merge-group run and it carries no ci-ok check run: not tested\$")"
check "says what the one run it starts tests" \
  "$(contains "$err" "^commit $bypassed reached this branch untested, so ci.yaml is started on this branch by workflow_dispatch: that run tests the branch's tip, which contains it. The 0 commit(s) after it are left unjudged")"

echo "case: a commit whose message carries a skip marker in its subject is passed over"
for marker in '[skip ci]' '[ci skip]' '[no ci]' '[skip actions]' '[actions skip]'; do
  marked=$(list "$(commit "$bypassed" "chore(release): pi-legion 18.16.0 $marker")")
  # The marker is a bracket expression to grep -E, so it is escaped once and read twice below.
  escaped=$(sed 's/[][]/\\&/g' <<<"$marker")
  run_bypass "$marked" "$(readings "$bypassed" '[]')" "$(readings "$bypassed" '[]')"
  check "$marker in the subject starts nothing, though the commit has no run and no ci-ok" \
    "$(is "$out" "")"
  check "  and the marker is named" \
    "$(contains "$err" "^commit $bypassed (chore(release): pi-legion 18.16.0 $escaped) carries the skip marker $escaped, so no run was ever expected for it: passed over\$")"
done

echo "case: a commit whose message carries the skip-checks trailer is passed over"
for trailer in 'skip-checks:true' 'skip-checks: true'; do
  trailered=$(list "$(commit "$bypassed" "$(printf 'docs: fold the changelog\n\n%s\n' "$trailer")")")
  run_bypass "$trailered" "$(readings "$bypassed" '[]')" "$(readings "$bypassed" '[]')"
  check "the trailer $trailer starts nothing" "$(is "$out" "")"
  check "  and the subject, not the trailer line, is what the reasoning quotes" \
    "$(contains "$err" "^commit $bypassed (docs: fold the changelog) carries the skip marker $trailer, so no run was ever expected for it: passed over\$")"
done
# The control: the same words in the subject are no trailer, and that commit is read.
not_a_trailer=$(list "$(commit "$bypassed" "docs: say what skip-checks: true does")")
run_bypass "$not_a_trailer" "$(readings "$bypassed" '[]')" "$(readings "$bypassed" '[]')"
check "the trailer's words inside a subject line are no trailer" "$(is "$out" "dispatch $bypassed")"

echo "case: two untested commits start one run, named for the first of them"
two_bypassed=$(list \
  "$(commit "$bypassed" "fix(ci): an admin merge, pushed past the queue")" \
  "$(commit "$bypassed_two" "fix(ci): and the commit on top of it")" \
  "$(commit "$queued" "feat(daemon): the keeper retries the claim")")
run_bypass "$two_bypassed" \
  "$(readings "$bypassed" '[]' "$bypassed_two" '[]' "$queued" "$(list "$(mg_run "$queued" success)")")" \
  "$(readings "$bypassed" '[]' "$bypassed_two" '[]' "$queued" "$(list "$(ci_ok_run success)")")"
check "decides" "$(is "$status" 0)"
check "prints one token, the first untested commit" "$(is "$out" "dispatch $bypassed")"
check "says the two commits after it are left unjudged" \
  "$(contains "$err" 'The 2 commit(s) after it are left unjudged, because that one run tests them too[.]$')"
check "judges none of them: the reasoning names neither" "$(is "$(contains "$err" "$bypassed_two")" false)"

echo "case: a ci-ok that concluded failure is no passing ci-ok"
red=$(list "$(commit "$bypassed" "fix(daemon): the rescue test reads the pod's workspace")")
run_bypass "$red" \
  "$(readings "$bypassed" "$(list "$(mg_run "$bypassed" failure)")")" \
  "$(readings "$bypassed" "$(list "$(ci_ok_run failure)")")"
check "that commit is not tested" "$(is "$out" "dispatch $bypassed")"
check "says both readings" \
  "$(contains "$err" "^commit $bypassed (fix(daemon): the rescue test reads the pod's workspace): it is the head of 1 ci.yaml merge-group run(s) (CI failure) and its 1 ci-ok check run(s) concluded failure: not tested\$")"

echo "case: a passing ci-ok on a commit that is the head of no merge-group run is not enough"
# What a run of this very check leaves behind: its dispatched ci.yaml run posts a passing ci-ok on
# a commit the merge queue never built a group for.
dispatched=$(list "$(commit "$bypassed" "fix(ci): an admin merge, pushed past the queue")")
run_bypass "$dispatched" \
  "$(readings "$bypassed" '[]')" \
  "$(readings "$bypassed" "$(list "$(ci_ok_run success)")")"
check "that commit is not tested" "$(is "$out" "dispatch $bypassed")"
check "says the ci-ok is there and the merge-group run is not" \
  "$(contains "$err" "^commit $bypassed (fix(ci): an admin merge, pushed past the queue): it is the head of no ci.yaml merge-group run and its 1 ci-ok check run(s) concluded success: not tested\$")"

echo "case: of two ci-ok check runs on one commit, the passing one is what counts"
# `filter=all` is what returns both: the failing one the run whose job failed posted, and the
# passing one a later run of the same group posted.
twice=$(list "$(commit "$queued" "feat(daemon): the keeper retries the claim")")
run_bypass "$twice" \
  "$(readings "$queued" "$(list "$(mg_run "$queued" failure)" "$(mg_run "$queued" success)")")" \
  "$(readings "$queued" "$(list "$(ci_ok_run failure)" "$(ci_ok_run success)")")"
check "the commit is tested" "$(is "$out" "")"
check "and both check runs are in the reasoning" \
  "$(contains "$err" "its 2 ci-ok check run(s) concluded failure, success: tested\$")"

echo "case: a merge-group run of another commit is no run of this one"
other_head=$(list "$(commit "$bypassed" "fix(ci): an admin merge, pushed past the queue")")
run_bypass "$other_head" \
  "$(readings "$bypassed" "$(list "$(mg_run "$elsewhere" success)")")" \
  "$(readings "$bypassed" "$(list "$(ci_ok_run success)")")"
check "the commit is not tested" "$(is "$out" "dispatch $bypassed")"
check "says it is the head of no merge-group run" \
  "$(contains "$err" "it is the head of no ci.yaml merge-group run and its 1 ci-ok check run(s) concluded success: not tested\$")"

echo "case: a run that has not finished carries no conclusion, and is read as it comes"
running=$(list "$(commit "$bypassed" "fix(ci): an admin merge, pushed past the queue")")
run_bypass "$running" \
  "$(readings "$bypassed" "$(list "$(mg_run "$bypassed" "")")")" \
  "$(readings "$bypassed" "$(list "$(ci_ok_run "")")")"
check "the commit is not tested" "$(is "$out" "dispatch $bypassed")"
check "and the reasoning says neither finished" \
  "$(contains "$err" "it is the head of 1 ci.yaml merge-group run(s) (CI not finished) and its 1 ci-ok check run(s) concluded not finished: not tested\$")"

echo "case: a push that carries no commit"
run_bypass '[]' '{}' '{}'
check "decides" "$(is "$status" 0)"
check "starts nothing" "$(is "$out" "")"
check "says there was nothing to read" \
  "$(contains "$err" '^this push carries no commit, so there is nothing to read and nothing is started$')"

echo "case: a bad input is named and nothing is decided"
run_bypass '' '{}' '{}'
check "no COMMITS fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" '^::error::main-bypass-check.sh needs COMMITS in its environment$')"
run_bypass '[{"id": "'"$queued"'"}]' '{}' '{}'
check "a commit without its message fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::COMMITS is not a JSON array of the push')"
run_bypass '[{"id": "refs/heads/main", "message": "feat: one"}]' '{}' '{}'
check "an id that is no commit sha fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::COMMITS holds an id that is no commit sha: refs/heads/main$')"
run_bypass "$(list "$(commit "$queued" "feat: one")")" '{"a": 1}' '{}'
check "a RUNS that maps a sha to something other than a list fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" '^::error::RUNS is not a JSON object mapping a commit sha to an array of what was read for it')"
run_bypass "$(list "$(commit "$queued" "feat: one")")" "$(readings "$queued" '[]')" '{}'
check "a commit with no reading at all fails, rather than reading as untested" "$(is "$status" 1)"
check "names the commit" \
  "$(contains "$err" "^::error::RUNS and CI_OK need a reading for every commit of COMMITS, and one of them holds none for: $queued\$")"

echo "case: the skip-checks words on a line no trailer block holds are no trailer"
# GitHub reads skip-checks only as a trailer: at the end of the message, after two empty lines
# (docs.github.com, "Skipping workflow runs"). A commit that quotes the words in the middle of
# its body got every workflow GitHub starts, so it reached main past the queue like any other
# commit and must be read here.
mid_body=$(list "$(commit "$bypassed" "$(printf 'docs(ci): write the trailer down\n\nThe trailer is spelled\n\nskip-checks: true\n\nand GitHub reads it only where it is last. This paragraph comes after it.\n')")")
run_bypass "$mid_body" "$(readings "$bypassed" '[]')" "$(readings "$bypassed" '[]')"
check "the commit is read rather than passed over" "$(is "$out" "dispatch $bypassed")"

echo "case: the workflow step that gathers the push runs under a shell with pipefail"
# GitHub runs a `run:` step that names no shell as `bash -e {0}`, which has no `pipefail`, and
# `shell: bash` as `bash --noprofile --norc -eo pipefail {0}` (docs.github.com, "Workflow syntax
# for GitHub Actions", the default shell table). Every read this step makes ends in `jq -s`,
# which exits 0 on an empty input, so without pipefail a `gh` that answers 502 hands the
# decision below an empty push and the check goes green having started nothing — the one outcome
# this backstop exists to prevent.
bypass_workflow="$script_dir/../workflows/main-bypass-check.yaml"
bypass_default_shell=$(yq -r '.defaults.run.shell // ""' "$bypass_workflow" 2>/dev/null)
no_pipefail=$(SHELL_DEFAULT="$bypass_default_shell" yq -r '
  [.jobs[].steps[]
   | select(has("run"))
   | select((.shell // strenv(SHELL_DEFAULT)) != "bash")
   | (.name // "an unnamed step")] | join(", ")' "$bypass_workflow" 2>/dev/null)
check "no run: step of main-bypass-check.yaml runs under a shell without pipefail" \
  "$(is "$no_pipefail" "")"

echo "case: skip-checks last in a trailer block of its own is GitHub's trailer"
# GitHub: "If you already have other trailers in your commit message, `skip-checks` should be
# last" (docs.github.com, "Skipping workflow runs"). The rule here asks for an empty line
# immediately before the trailer, which forbids every other trailer above it, so a commit GitHub
# skipped every workflow for is read as untested and starts a full ci.yaml run of every job on
# main — the most expensive run in this repository, for a commit that asked for none.
trailer_block=$(list "$(commit "$bypassed" "$(printf 'plan: record handoff\n\nCo-authored-by: a <a@example.com>\nskip-checks: true\n')")")
run_bypass "$trailer_block" "$(readings "$bypassed" '[]')" "$(readings "$bypassed" '[]')"
check "a trailer block ending in skip-checks starts nothing" "$(is "$out" "")"

echo "case: the message shapes the skip-checks rule reads as a trailer, and those it does not"
# The rule: the words are the message's last non-empty line, that line ends the message's last
# block of non-empty lines, every line of the block is a trailer, and an empty line comes before
# the block (docs.github.com, "Skipping workflow runs": the trailers section is at the end of the
# message, preceded by two empty lines, and `skip-checks` is last of the trailers). Git's own
# parsing agrees on every shape below but the mixed block, which `git interpret-trailers --parse`
# reads as trailers only when one of them is git-generated; this rule reads it as none, which
# costs one dispatched run and misses no untested commit.
shape() { # shape <description> <message> <passed over|read>
  run_bypass "$(list "$(commit "$bypassed" "$2")")" \
    "$(readings "$bypassed" '[]')" "$(readings "$bypassed" '[]')"
  if [ "$3" = "passed over" ]; then
    check "$1: passed over" "$(is "$out" "")"
  else
    check "$1: read, and the run is started" "$(is "$out" "dispatch $bypassed")"
  fi
}
shape "a body paragraph, then a trailer block ending in skip-checks" \
  "$(printf 'plan: record the handoff\n\nA paragraph of body.\n\nOmp-Session: 0a1b2c\nskip-checks: true\n')" \
  'passed over'
shape "a trailer folded over two lines, then skip-checks" \
  "$(printf 'plan: record the handoff\n\nCo-authored-by: a\n <a@example.com>\nskip-checks: true\n')" \
  'passed over'
shape "the two empty lines the page asks for, which git's cleanup folds into one" \
  "$(printf 'plan: record the handoff\n\n\nOmp-Session: 0a1b2c\nskip-checks: true\n')" \
  'passed over'
shape "the carriage returns a Windows client sends" \
  "$(printf 'plan: record the handoff\r\n\r\nCo-authored-by: a <a@example.com>\r\nskip-checks: true\r\n')" \
  'passed over'
shape "empty lines after the trailer block" \
  "$(printf 'plan: record the handoff\n\nCo-authored-by: a <a@example.com>\nskip-checks: true\n\n\n')" \
  'passed over'
shape "a last block that mixes a line of prose with the words" \
  "$(printf 'docs: write the rule down\n\nSome prose here\nskip-checks: true\n')" read
shape "skip-checks above another trailer, where the page asks for it last" \
  "$(printf 'plan: record the handoff\n\nskip-checks: true\nCo-authored-by: a <a@example.com>\n')" read
shape "the words with no empty line above them" \
  "$(printf 'docs: fold the changelog\nskip-checks: true\n')" read
shape "the words as the whole message, which makes them its subject" 'skip-checks: true' read
shape "skip-checks: false, which is no marker of GitHub's" \
  "$(printf 'plan: record the handoff\n\nCo-authored-by: a <a@example.com>\nskip-checks: false\n')" read

summary "the main bypass check's decision"
