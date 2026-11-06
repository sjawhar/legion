#!/usr/bin/env bash
# Tests for `main-bypass-check.sh`: whether the head of a push to main reached it untested, and
# whether the bypass check must start a run of ci.yaml for it.
#
# main-bypass-check.sh reads only its environment, so each case hands it the head `github.event`
# would carry for a push of its own — its sha and its whole message — the merge-group runs
# `gh run list` would give at that head, and the ci-ok check runs the check-runs read would give.
# The fixtures carry the real shapes: 40-character shas, whole commit messages with their bodies,
# and the fields of each read. The script under test is the file CI runs, and every case runs it
# under `timeout`, so a decision that hung would fail its case instead of hanging the job.
#
# The contract these cases pin is the head's alone: no case hands the decision another commit of
# the push, because it reads none.
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

# A head the queue landed, a head that reached main past it, and a commit that is neither.
queued=1dc54c7e8a9b0f2d3c4e5a6b7c8d9e0f1a2b3c4d
bypassed=3f8a2b4c6d8e0f2a4b6c8d0e2f4a6b8c0d2e4f6a
elsewhere=5b0c4d6e8f0a2b4c6d8e0f2a4b6c8d0e2f4a6b8c

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

# run_bypass <head sha> <head message> <runs> <ci-ok runs>: sets out (stdout, the token), err
# (stderr, the reasoning) and status.
run_bypass() {
  set +e
  out=$(HEAD_SHA=$1 HEAD_MESSAGE=$2 RUNS=$3 CI_OK=$4 timeout 60 "$script_dir/main-bypass-check.sh" 2>"$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

echo "case: a head with no ci.yaml merge-group run at all starts a run"
run_bypass "$bypassed" "fix(ci): an admin merge, pushed past the queue" '[]' '[]'
check "decides" "$(is "$status" 0)"
check "names the head" "$(is "$out" "dispatch $bypassed")"
check "says what is missing" \
  "$(contains "$err" "^commit $bypassed (fix(ci): an admin merge, pushed past the queue): it is the head of no ci.yaml merge-group run and it carries no ci-ok check run: not tested\$")"
check "says what the one run it starts tests" \
  "$(contains "$err" "^commit $bypassed reached this branch untested, so ci.yaml is started on this branch by workflow_dispatch: that run tests the branch's tip, which contains it[.]\$")"

echo "case: a head whose merge-group run posted a failing ci-ok starts a run"
run_bypass "$bypassed" "fix(daemon): the rescue test reads the pod's workspace" \
  "$(list "$(mg_run "$bypassed" failure)")" \
  "$(list "$(ci_ok_run failure)")"
check "decides" "$(is "$status" 0)"
check "the head is not tested" "$(is "$out" "dispatch $bypassed")"
check "says both readings" \
  "$(contains "$err" "^commit $bypassed (fix(daemon): the rescue test reads the pod's workspace): it is the head of 1 ci.yaml merge-group run(s) (CI failure) and its 1 ci-ok check run(s) concluded failure: not tested\$")"

echo "case: a head whose only passing ci-ok came from the check's own dispatched run starts a run"
# What a run of this very check leaves behind: the ci.yaml run it dispatches posts a passing
# ci-ok on a commit the merge queue never built a group for, so the head is the head of no
# merge-group run and that ci-ok answers for nothing.
run_bypass "$bypassed" "fix(ci): an admin merge, pushed past the queue" \
  '[]' \
  "$(list "$(ci_ok_run success)")"
check "the head is not tested" "$(is "$out" "dispatch $bypassed")"
check "says the ci-ok is there and the merge-group run is not" \
  "$(contains "$err" "^commit $bypassed (fix(ci): an admin merge, pushed past the queue): it is the head of no ci.yaml merge-group run and its 1 ci-ok check run(s) concluded success: not tested\$")"

echo "case: a head that is the head of a merge-group run with a passing ci-ok starts nothing"
# The negative control of the three above: the shape every push from the queue has.
run_bypass "$queued" "feat(daemon): the keeper retries the claim" \
  "$(list "$(mg_run "$queued" success)")" \
  "$(list "$(ci_ok_run success)")"
check "decides" "$(is "$status" 0)"
check "starts nothing" "$(is "$out" "")"
check "says what it read of the head" \
  "$(contains "$err" "^commit $queued (feat(daemon): the keeper retries the claim): it is the head of 1 ci.yaml merge-group run(s) (CI success) and its 1 ci-ok check run(s) concluded success: tested\$")"
check "says the push needs no run" \
  "$(contains "$err" "^this push's head was tested on the merge queue: no run is started\$")"

echo "case: a head whose message carries a skip marker in its subject is passed over"
for marker in '[skip ci]' '[ci skip]' '[no ci]' '[skip actions]' '[actions skip]'; do
  # The marker is a bracket expression to grep -E, so it is escaped once and read twice below.
  escaped=$(sed 's/[][]/\\&/g' <<<"$marker")
  run_bypass "$bypassed" "chore(release): pi-legion 18.16.0 $marker" '[]' '[]'
  check "$marker in the subject starts nothing, though the head has no run and no ci-ok" \
    "$(is "$out" "")"
  check "  and the marker is named" \
    "$(contains "$err" "^commit $bypassed (chore(release): pi-legion 18.16.0 $escaped) carries the skip marker $escaped, so no run was ever expected for it: passed over\$")"
done

echo "case: a head whose message carries the skip-checks trailer is passed over"
for trailer in 'skip-checks:true' 'skip-checks: true'; do
  run_bypass "$bypassed" "$(printf 'docs: fold the changelog\n\n%s\n' "$trailer")" '[]' '[]'
  check "the trailer $trailer starts nothing" "$(is "$out" "")"
  check "  and the subject, not the trailer line, is what the reasoning quotes" \
    "$(contains "$err" "^commit $bypassed (docs: fold the changelog) carries the skip marker $trailer, so no run was ever expected for it: passed over\$")"
done
# The control: the same words in the subject are no trailer, and that head is read.
run_bypass "$bypassed" "docs: say what skip-checks: true does" '[]' '[]'
check "the trailer's words inside a subject line are no trailer" "$(is "$out" "dispatch $bypassed")"

echo "case: of two ci-ok check runs on one head, the passing one is what counts"
# `filter=all` is what returns both: the failing one the run whose job failed posted, and the
# passing one a later run of the same group posted.
run_bypass "$queued" "feat(daemon): the keeper retries the claim" \
  "$(list "$(mg_run "$queued" failure)" "$(mg_run "$queued" success)")" \
  "$(list "$(ci_ok_run failure)" "$(ci_ok_run success)")"
check "the head is tested" "$(is "$out" "")"
check "and both check runs are in the reasoning" \
  "$(contains "$err" "its 2 ci-ok check run(s) concluded failure, success: tested\$")"

echo "case: a merge-group run of another commit is no run of this head"
run_bypass "$bypassed" "fix(ci): an admin merge, pushed past the queue" \
  "$(list "$(mg_run "$elsewhere" success)")" \
  "$(list "$(ci_ok_run success)")"
check "the head is not tested" "$(is "$out" "dispatch $bypassed")"
check "says it is the head of no merge-group run" \
  "$(contains "$err" "it is the head of no ci.yaml merge-group run and its 1 ci-ok check run(s) concluded success: not tested\$")"

echo "case: a run that has not finished carries no conclusion, and is read as it comes"
run_bypass "$bypassed" "fix(ci): an admin merge, pushed past the queue" \
  "$(list "$(mg_run "$bypassed" "")")" \
  "$(list "$(ci_ok_run "")")"
check "the head is not tested" "$(is "$out" "dispatch $bypassed")"
check "and the reasoning says neither finished" \
  "$(contains "$err" "it is the head of 1 ci.yaml merge-group run(s) (CI not finished) and its 1 ci-ok check run(s) concluded not finished: not tested\$")"

echo "case: a push payload that carried no head message is read, not passed over"
# `github.event.head_commit` is what the message comes from, and a payload carrying none leaves
# HEAD_MESSAGE empty. No message holds no marker, so the head is read by its runs and checks —
# the side that starts the run.
run_bypass "$bypassed" "" '[]' '[]'
check "decides" "$(is "$status" 0)"
check "the head is read rather than passed over" "$(is "$out" "dispatch $bypassed")"
check "and the reasoning says there was no message to quote" \
  "$(contains "$err" "^commit $bypassed (no commit message in the push payload): it is the head of no ci.yaml merge-group run and it carries no ci-ok check run: not tested\$")"

echo "case: a bad input is named and nothing is decided"
run_bypass '' 'feat: one' '[]' '[]'
check "no HEAD_SHA fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" '^::error::main-bypass-check.sh needs HEAD_SHA in its environment$')"
run_bypass "$queued" 'feat: one' '' '[]'
check "no RUNS fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" '^::error::main-bypass-check.sh needs RUNS in its environment$')"
run_bypass 'refs/heads/main' 'feat: one' '[]' '[]'
check "a head that is no commit sha fails" "$(is "$status" 1)"
check "names it" "$(contains "$err" '^::error::HEAD_SHA is no commit sha: refs/heads/main$')"
run_bypass "$queued" 'feat: one' '{"a": 1}' '[]'
check "a RUNS that is no array fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" "^::error::RUNS is not a JSON array of what was read at the push's head")"
run_bypass "$queued" 'feat: one' '[]' '["ci-ok"]'
check "a CI_OK holding something other than a check run fails" "$(is "$status" 1)"
check "names it" \
  "$(contains "$err" "^::error::CI_OK is not a JSON array of what was read at the push's head")"

echo "case: the skip-checks words on a line no trailer block holds are no trailer"
# GitHub reads skip-checks only as a trailer: at the end of the message, after two empty lines
# (docs.github.com, "Skipping workflow runs"). A head that quotes the words in the middle of its
# body got every workflow GitHub starts, so it reached main past the queue like any other commit
# and must be read here.
run_bypass "$bypassed" \
  "$(printf 'docs(ci): write the trailer down\n\nThe trailer is spelled\n\nskip-checks: true\n\nand GitHub reads it only where it is last. This paragraph comes after it.\n')" \
  '[]' '[]'
check "the head is read rather than passed over" "$(is "$out" "dispatch $bypassed")"

echo "case: the workflow step that gathers the head's readings runs under a shell with pipefail"
# GitHub runs a `run:` step that names no shell as `bash -e {0}`, which has no `pipefail`, and
# `shell: bash` as `bash --noprofile --norc -eo pipefail {0}` (docs.github.com, "Workflow syntax
# for GitHub Actions", the default shell table). A `gh` that answers 502 at the head of a
# pipeline must end the step: the alternative is a decision handed an empty reading, going green
# having started nothing — the one outcome this backstop exists to prevent.
bypass_workflow="$script_dir/../workflows/main-bypass-check.yaml"
bypass_default_shell=$(yq -r '.defaults.run.shell // ""' "$bypass_workflow" 2>/dev/null)
# The names are filled in over the collected array (`.[] |=`), never as a `|` stage of the
# selection itself: yq reads a literal as a node of its own, so `… | (.name // "x")` over a
# selection that matched nothing still yields "x", and this case would report a step the
# workflow does not have and fail on every run.
no_pipefail=$(SHELL_DEFAULT="$bypass_default_shell" yq -r '
  [.jobs[].steps[]
   | select(has("run"))
   | select((.shell // strenv(SHELL_DEFAULT)) != "bash")]
  | .[] |= (.name // "an unnamed step") | join(", ")' "$bypass_workflow" 2>/dev/null)
check "no run: step of main-bypass-check.yaml runs under a shell without pipefail" \
  "$(is "$no_pipefail" "")"

echo "case: the workflow hands the decision the push's head and reads no other commit of it"
# The other half of the head-only contract is the job's wiring: the decision's HEAD_SHA and
# HEAD_MESSAGE come from the push payload's head, and no step walks `github.event.commits` or
# falls back to the compare API, which the walk over every commit of the push needed.
head_env=$(yq -r '[.jobs[].steps[]
  | select(has("run"))
  | (.env // {})
  | to_entries[]
  | select(.key == "HEAD_SHA" or .key == "HEAD_MESSAGE")
  | "\(.key)=\(.value)"] | join(" ")' "$bypass_workflow")
# shellcheck disable=SC2016 # ${{ … }} is GitHub's expression syntax, not a shell expansion
check "the step takes the head and its message from the push payload" \
  "$(is "$head_env" 'HEAD_SHA=${{ github.event.after }} HEAD_MESSAGE=${{ github.event.head_commit.message }}')"
walked=$(yq -r '.jobs[].steps[] | select(has("run")) | .run' "$bypass_workflow" |
  grep -c 'event\.commits\|/compare/' || true)
check "and no run: step walks the push's commits or reads the compare API" "$(is "$walked" 0)"

echo "case: skip-checks last in a trailer block of its own is GitHub's trailer"
# GitHub: "If you already have other trailers in your commit message, `skip-checks` should be
# last" (docs.github.com, "Skipping workflow runs"). The rule here asks for an empty line
# immediately before the trailer, which forbids every other trailer above it, so a commit GitHub
# skipped every workflow for is read as untested and starts a full ci.yaml run of every job on
# main — the most expensive run in this repository, for a commit that asked for none.
run_bypass "$bypassed" \
  "$(printf 'plan: record handoff\n\nCo-authored-by: a <a@example.com>\nskip-checks: true\n')" \
  '[]' '[]'
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
  run_bypass "$bypassed" "$2" '[]' '[]'
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
