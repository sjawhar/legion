#!/usr/bin/env bash
# The one retry ladder the ci-ok scripts beside this file use for a GitHub call: the jobs list of
# ci-ok-report.sh, the check-run post of post-check-run.sh, and the check-run list and each PATCH
# of ci-ok-reconcile.sh. GitHub refuses a call now and then — a 502, a secondary rate limit — and
# a single refusal must decide nothing here: ci-ok is the check main's ruleset requires, and the
# scripts that post it act on what one API call answered.
#
# Sourced, never run: `source "$(dirname "${BASH_SOURCE[0]}")/gh-read.sh"`.
#
# gh_read <command>...: runs the command, and on a failure runs it again twice, GH_RETRY_SECONDS
# (10 by default; the tests set 0) and then twice as many seconds apart. It prints the stdout of
# the attempt that succeeded and nothing else, so a `--paginate` read that failed halfway leaves
# no half page in the caller's hands; every attempt's stderr goes to the caller's stderr, where
# the job log holds it. It returns 0 for the attempt that succeeded and non-zero when all three
# were refused, which is the caller's to answer: a refused jobs list leaves ci-ok-report.sh with
# no result to name for the jobs that have not ended, a refused post leaves no ci-ok at all, and
# a refused PATCH leaves a stale ci-ok on the head.
gh_read() {
  local attempt output
  for attempt in 1 2 3; do
    if output=$("$@"); then
      printf '%s' "$output"
      return 0
    fi
    [ "$attempt" -eq 3 ] || sleep $((attempt * ${GH_RETRY_SECONDS:-10}))
  done
  return 1
}
