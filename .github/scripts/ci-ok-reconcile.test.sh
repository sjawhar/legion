#!/usr/bin/env bash
# Tests for `ci-ok-reconcile.sh`, the last step of ci.yaml's ci-ok job: every ci-ok check run on
# the head that a job posted is PATCHed to what that job concluded, so one verdict stands at the
# end of every attempt. A post is an external_id carrying the ci-ok-post: marker or carrying
# nothing at all, which is what the ci-ok-report.sh of the release before the marker posted. It
# leaves alone this job's own check run, one that already carries this conclusion, one that is
# not completed, and one whose external_id is a UUID, which is a job's own ci-ok check run; a
# refused read or PATCH warns and never fails the job, since failing it would turn a passing
# verdict red on the head this step exists to unblock.
#
# Each case writes the head's ci-ok check runs as the script's `gh --jq` prints them (id, status,
# conclusion, external_id) and runs the real script with a stand-in `gh` first on PATH, which
# answers that read and records the body of each PATCH it is sent.
#
# Run from anywhere: .github/scripts/ci-ok-reconcile.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

mkdir "$work/bin"
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
case "\$*" in
  *"/commits/abc123/check-runs?check_name=ci-ok&filter=all&per_page=100"*)
    if [ -e "$work/list-fails" ]; then echo "HTTP 502" >&2; exit 1; fi
    cat "$work/check-runs" ;;
  *"--method PATCH repos/o/r/check-runs/"*)
    # The body goes as a file, so that a retried PATCH sends it again (post-check-run.sh says why).
    id=
    body=
    while [ \$# -gt 0 ]; do
      case \$1 in
        --input) body=\$2 ;;
        repos/o/r/check-runs/*) id=\${1##*/} ;;
      esac
      shift
    done
    n=\$(find "$work" -maxdepth 1 -name 'patched.*.json' | wc -l)
    cp "\$body" "$work/patched.\$n.json"
    printf '%s\n' "\$id" >> "$work/patched-ids"
    if [ -e "$work/patch-fails" ]; then echo "HTTP 403" >&2; exit 1; fi ;;
  *) echo "unexpected gh \$*" >&2; exit 2 ;;
esac
GH
chmod +x "$work/bin/gh"

# check_runs <id>=<status>[/<conclusion>][@<external id>]...: the head's ci-ok check runs, one
# per line, as `gh api --jq '.check_runs[] | "\(.id) \(.status) \(.conclusion) \(…)"'` prints
# them. A check run named with no `@` carries ci-ok-post:9, the marker post-check-run.sh sends
# with a ci-ok a job posted; `@-` is what the script's jq writes for a check run GitHub returns
# no external_id for at all, and a UUID is what a job's own check run carries.
check_runs() {
  local pair spec marker rest
  : > "$work/check-runs"
  for pair in "$@"; do
    spec=${pair%%@*}
    marker=ci-ok-post:9
    [ "$spec" = "$pair" ] || marker=${pair#*@}
    rest=${spec#*=}
    printf '%s %s %s %s\n' "${spec%%=*}" "${rest%%/*}" "${rest#*/}" "$marker" >> "$work/check-runs"
  done
  rm -f "$work"/patched.*.json "$work/patched-ids" "$work/list-fails" "$work/patch-fails"
}

# run_reconcile <conclusion> [<own check run id>]: sets out and status.
run_reconcile() {
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r HEAD_SHA=abc123 \
    GITHUB_SERVER_URL=https://github.com GITHUB_RUN_ID=41 GH_RETRY_SECONDS=0 \
    CHECK_RUN_ID=${2:-900} CONCLUSION=$1 "$script_dir/ci-ok-reconcile.sh" 2>&1)
  status=$?
  set -e
}

patched() { [ -e "$work/patched-ids" ] && tr '\n' ' ' < "$work/patched-ids" || echo ""; }
calls() { find "$work" -maxdepth 1 -name 'patched.*.json' | wc -l | tr -d ' '; }
field() { jq -r "$1" "$work/patched.0.json"; }

echo "case: a stale failing ci-ok on a head this attempt passed"
check_runs "900=in_progress/null" "100=completed/failure"
run_reconcile success
check "exits 0" "$(is "$status" 0)"
check "patches the stale one alone" "$(is "$(patched)" "100 ")"
check "to this job's conclusion" "$(is "$(field .conclusion)" success)"
check "linking this job" "$(is "$(field .details_url)" https://github.com/o/r/actions/runs/41/job/900)"
check "titled with the run that decided it" \
  "$(is "$(field .output.title)" "ci-ok is success on this head: the ci-ok job of run 41 decided it")"
check "says a posted check run is updated by no re-run" \
  "$(contains "$(field .output.summary)" 'posted by a job of a run rather than concluded by one, so no re-run updates it')"
check "says what it did" "$(contains "$out" '^set ci-ok check run 100 from failure to success$')"
check "and counts it" "$(contains "$out" '^ci-ok on abc123 is success; 1 other check run(s) of that name were set to it$')"

echo "case: this job's own check run"
check_runs "900=completed/failure"
run_reconcile failure
check "is left alone" "$(is "$(patched)" "")"
check "and named as this job's own" "$(contains "$out" "^ci-ok check run 900 is this job's own (failure)$")"

echo "case: a ci-ok that already carries this conclusion"
check_runs "900=in_progress/null" "100=completed/success"
run_reconcile success
check "is not written again" "$(is "$(patched)" "")"
check "and says so" "$(contains "$out" '^ci-ok check run 100 already concluded success$')"

echo "case: a ci-ok another run is still deciding"
check_runs "900=in_progress/null" "100=in_progress/null" "101=queued/null"
run_reconcile success
check "is left to that run" "$(is "$(patched)" "")"
check "naming each" "$(contains "$out" '^ci-ok check run 101 is queued, so the run deciding it writes its conclusion$')"

echo "case: a ci-ok a job of a run posted, which carries the marker"
check_runs "900=in_progress/null" "100=completed/failure@ci-ok-post:7"
run_reconcile success
check "exits 0" "$(is "$status" 0)"
check "is patched" "$(is "$(patched)" "100 ")"
check "to this job's conclusion" "$(is "$(field .conclusion)" success)"
check "and the body carries a marker, so the next attempt's read still finds one" \
  "$(contains "$(field .external_id)" '^ci-ok-post:')"

echo "case: a ci-ok the release before the marker posted, which carries no external_id at all"
# The older ci-ok-report.sh posted with no external_id, so GitHub returns an empty one and the
# script's jq writes `-` for it. Those posts are the stale ci-ok check runs that sit on open
# heads today, measured on 2026-11-06: 126296045537 on pull request #2726 at 4a878981 and
# 127791633790 on #2860 at 57182d42, both success, both with an empty external_id. They are the
# one class of stale verdict this step exists to clear, so an absent marker is a post.
check_runs "900=in_progress/null" "100=completed/failure@-"
run_reconcile success
check "exits 0" "$(is "$status" 0)"
check "is patched as a marked post is" "$(is "$(patched)" "100 ")"
check "to this job's conclusion" "$(is "$(field .conclusion)" success)"
check "and the body carries a marker, so the next attempt's read finds a marked post" \
  "$(contains "$(field .external_id)" '^ci-ok-post:')"
check "says what it did" "$(contains "$out" '^set ci-ok check run 100 from failure to success$')"

echo "case: another workflow run's own ci-ok job check run on this head, whose external_id is a UUID"
# main-bypass-check.yaml dispatches ci.yaml on main, so the dispatched run's github.sha is
# main's tip, which the queue landed carrying its own group's green ci-ok. Its external_id is a
# UUID rather than a ci-ok-post: marker or an absent one (measured on INFRA-1066's k28 probe
# head), and GitHub refuses a PATCH of it under GITHUB_TOKEN with "Check run status and
# conclusions can only be updated internally by GitHub Actions", measured on the same probe: an
# unfiltered step spends its whole retry ladder on a call that cannot succeed and warns that a
# verdict stands which was never its to change.
check_runs "900=in_progress/null" "100=completed/success@07c6d79e-718e-5fab-b991-ebb3c3a6833c"
run_reconcile failure
check "exits 0" "$(is "$status" 0)"
check "is left alone" "$(is "$(patched)" "")"
check "writes nothing" "$(is "$(calls)" 0)"
check "and names the external_id it saw and whose check run that makes it" \
  "$(contains "$out" "^ci-ok check run 100 carries the external_id 07c6d79e-718e-5fab-b991-ebb3c3a6833c, which is neither a ci-ok-post: marker nor absent, so it is a job's own ci-ok check run, which the run whose job it is concludes itself$")"
check "and still counts what it patched" \
  "$(contains "$out" '^ci-ok on abc123 is failure; 0 other check run(s) of that name were set to it$')"

echo "case: a head carrying this job's own ci-ok, a marked post, a legacy post and another run's own"
check_runs "900=in_progress/null" "100=completed/failure@ci-ok-post:7" "101=completed/failure@-" \
  "102=completed/failure@4a53aeee-0d45-5854-8639-11c4a3f4d78f"
run_reconcile success
check "exits 0" "$(is "$status" 0)"
check "patches the two posts and nothing else" "$(is "$(patched)" "100 101 ")"
check "leaving the other run's own job check run to that run" \
  "$(contains "$out" "^ci-ok check run 102 carries the external_id 4a53aeee-0d45-5854-8639-11c4a3f4d78f, which is neither a ci-ok-post: marker nor absent, so it is a job's own ci-ok check run, which the run whose job it is concludes itself$")"
check "and counts the two" \
  "$(contains "$out" '^ci-ok on abc123 is success; 2 other check run(s) of that name were set to it$')"

echo "case: a failing attempt"
check_runs "900=in_progress/null" "100=completed/success" "101=completed/success"
run_reconcile failure
check "sets every other ci-ok to failure" "$(is "$(patched)" "100 101 ")"
check "to this job's conclusion" "$(is "$(field .conclusion)" failure)"
check "and counts them" "$(contains "$out" '^ci-ok on abc123 is failure; 2 other check run(s) of that name were set to it$')"

echo "case: a cancelled attempt"
check_runs "900=in_progress/null" "100=completed/success"
run_reconcile cancelled
check "carries job.status through" "$(is "$(field .conclusion)" cancelled)"

echo "case: no other ci-ok on the head"
check_runs "900=in_progress/null"
run_reconcile success
check "exits 0" "$(is "$status" 0)"
check "writes nothing" "$(is "$(calls)" 0)"
check "and says the verdict stands" \
  "$(contains "$out" '^ci-ok on abc123 is success; 0 other check run(s) of that name were set to it$')"

echo "case: GitHub refuses the check-run list"
check_runs "900=in_progress/null" "100=completed/failure"
touch "$work/list-fails"
run_reconcile success
check "exits 0 rather than failing the job" "$(is "$status" 0)"
check "writes nothing" "$(is "$(calls)" 0)"
check "warns, naming this job's verdict" \
  "$(contains "$out" "::warning::GitHub refused this head's ci-ok check runs three times")"

echo "case: GitHub refuses a PATCH"
check_runs "900=in_progress/null" "100=completed/failure"
touch "$work/patch-fails"
run_reconcile success
check "exits 0 rather than failing the job" "$(is "$status" 0)"
check "tried three times" "$(is "$(calls)" 3)"
check "warns, naming the check run and both verdicts" \
  "$(contains "$out" '::warning::GitHub refused the PATCH of ci-ok check run 100 three times, so it stands at failure while this attempt.s verdict is success')"

echo "case: a conclusion that is no job.status"
check_runs "900=in_progress/null" "100=completed/failure"
run_reconcile skipped
check "fails the step" "$(is "$status" 1)"
check "writes nothing" "$(is "$(calls)" 0)"
check "says what it takes" "$(contains "$out" "ci-ok-reconcile.sh takes job.status, which is success, failure or cancelled, not 'skipped'")"

echo "case: an input the step did not set"
check_runs "900=in_progress/null"
set +e
out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r HEAD_SHA=abc123 \
  GITHUB_SERVER_URL=https://github.com GITHUB_RUN_ID=41 CONCLUSION=success \
  "$script_dir/ci-ok-reconcile.sh" 2>&1)
status=$?
set -e
check "fails the step" "$(is "$status" 1)"
check "names the input" "$(contains "$out" 'ci-ok-reconcile.sh needs CHECK_RUN_ID in its environment')"

summary "ci-ok-reconcile.sh leaves one ci-ok verdict on the head"
