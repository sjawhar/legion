#!/usr/bin/env bash
# The ci-ok conclusion one head carries, read from the check-runs page of that head on stdin:
# `success`, `failure`, another conclusion GitHub gives, or nothing at all for a head that
# carries no completed ci-ok. ci.yaml's `cancel-started` step gathers CI_OK for
# merge-group-cancels.sh's started act with it, one page per head, and
# merge-group-cancels.test.sh runs this same file over pages of its own: the read is here rather
# than in that step so the test exercises the workflow's own reading and not a copy of it.
#
# A head the queue tested can carry more than one ci-ok — `filter=all` returns a failing one from
# the run whose job failed and a passing one from a later run of the same group — and GitHub
# documents no order for the check runs of a commit. A `success` among them wins, since a head
# that carries one is no failed group. Among the rest the newest check run decides, which is the
# one GitHub's merge box and the Legion daemon read (.github/scripts/ci-ok-report.sh's header
# says why two jobs can both post). A check run that has not completed carries a null conclusion,
# which is no conclusion here: a head whose only ci-ok is still running carries none, and dooms
# nothing.
#
# Run from anywhere:
#   gh api "repos/$GITHUB_REPOSITORY/commits/$sha/check-runs?check_name=ci-ok&filter=all&per_page=100" |
#     .github/scripts/ci-ok-conclusion.sh
# Its tests are in merge-group-cancels.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

jq -r 'if any(.check_runs[]?; .conclusion == "success") then "success"
       else ([.check_runs[]?] | sort_by(.id) | last | .conclusion // empty) end'
