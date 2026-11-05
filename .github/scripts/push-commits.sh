#!/usr/bin/env bash
# The commits a push carried, as main-bypass-check.sh takes them in COMMITS: `[{id, message}]`,
# oldest first, read from the push payload GITHUB_EVENT_PATH names.
# main-bypass-check.yaml's job gathers COMMITS with it, and main-bypass-check.test.sh runs this
# same file over payloads of its own: the read is here rather than in that step so the test
# exercises the workflow's own reading and not a copy of it.
#
# The payload's `commits` array holds the push's commits, each with its whole message. It is read
# from the event file, not from an env: of its own, because GitHub documents the array as holding
# up to 2048 commits (docs.github.com, "Webhook events and payloads", push), and 2048 whole
# messages pass the 128 KiB Linux allows one environment string, which would kill the step at
# exec before it read anything. A payload whose `commits` is not an array is one this read cannot
# make: it ends here, loudly, rather than handing the decision an empty push and going green
# having started nothing, which is the one outcome that backstop exists to prevent. GitHub
# documents `commits` as present on every push, so only a payload this job was never meant to
# read reaches that error. An empty array is a shape of its own and no error: a push that adds no
# commit, as a force-push to a commit already on main is, carries nothing to test, and
# main-bypass-check.sh says so and starts nothing.
#
# Its inputs, from the step's env:
#   GITHUB_EVENT_PATH  the push payload's file, as Actions sets it.
#
# Run from anywhere: GITHUB_EVENT_PATH=... .github/scripts/push-commits.sh
# Its tests are in main-bypass-check.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

if [ -z "${GITHUB_EVENT_PATH:-}" ]; then
  echo "::error::push-commits.sh reads the push payload, so it needs GITHUB_EVENT_PATH in its environment"
  exit 1
fi

jq -c 'if (.commits | type) == "array" then [.commits[] | {id, message}]
       else error("the push payload has no .commits array: .commits is \(.commits | type)") end' \
  "$GITHUB_EVENT_PATH"
