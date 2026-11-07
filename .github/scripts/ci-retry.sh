#!/usr/bin/env bash
# The one retry every download in this repository's CI goes through: a workflow step, a composite
# action, or a script under .github/scripts/. No site holds a copy of this loop
# (dispatch://INFRA-1545, R2) — a site that needs a retry sources this file and calls the function
# it defines. A Dockerfile is the one exception, and not because of this file: it can carry no new
# file above a layer whose order it sets on purpose, so each of its network RUN lines opens with
# one canonical inline definition of the same loop instead.
#
# This file is SOURCED, never run. It defines one function, `retry_fetch`, and does nothing else:
# no option is set, no variable is left behind, nothing is printed. From a workflow `run:`, whose
# working directory is the repository root:
#
#     . .github/scripts/ci-retry.sh
#     retry_fetch "the Bun toolchain" 180 -- bun install --frozen-lockfile
#
# From a composite action, whose `run:` also starts at the caller's workspace rather than at the
# action's own directory, so the path to this file goes through $GITHUB_ACTION_PATH:
#
#     . "$GITHUB_ACTION_PATH/../../scripts/ci-retry.sh"
#     retry_fetch "changie" 60 -- curl -fsSLo changie.tar.gz "$url"
#
# A sourced file that ran `set -euo pipefail` would set those options on the step that sourced it,
# and a step that never asked for them would then end at its first failing command. So this one
# sets nothing, and `retry_fetch` is written to be correct both under a caller that has errexit
# and pipefail on — a failing attempt must not kill that caller before the retry happens — and
# under one that has neither. The shebang is here for shellcheck and for the reader; nothing
# execs this file.

# retry_fetch <what> <per-attempt bound in seconds> -- <command...>
#
# Runs <command...> under `timeout <bound>` until it succeeds, at most five times, sleeping 10,
# 20, 40 and 80 seconds between the attempts. The fifth attempt therefore starts 150 seconds after
# the first on the sleeps alone, which meets R1's floor — at least five attempts, the last starting
# at least 120 seconds after the first — whatever each attempt itself costs. A successful attempt
# returns at once and sleeps nothing, so the cost of this wrapper on a healthy download is the
# `timeout` process and nothing else.
#
# Each attempt is bounded because the fault this answers is not only a refusal: a host that accepts
# the connection and then sends nothing would hold one attempt for as long as the job's own budget,
# and the retries would never happen. The bound is per attempt, measured from that attempt's start.
#
# <what> is a short phrase naming what is being downloaded ("the Bun toolchain", "changie"); it
# opens every line this function prints, so a log says which download is retrying. <command...> is
# executed by `timeout` directly, so it must be a program with its arguments — not a shell
# function, a pipeline, or a redirection.
#
# Every line this function writes goes to standard error, and the command's own output is left
# untouched on standard output, so a caller may capture it:
#
#     names=$(retry_fetch "the run's artifact list" 30 -- gh api "$route" --jq '.[].name')
#
# The runner reads a step's standard error for workflow commands as it reads its standard output,
# so the `::error::` line below still becomes the step's annotation.
#
# On exhaustion it prints one `::error::` line, which GitHub turns into the step's annotation, and
# returns the last attempt's exit status.
retry_fetch() {
  local what bound attempts attempt status left noun refusal

  if [ "$#" -lt 3 ]; then
    echo "::error::retry_fetch takes <what> <per-attempt bound in seconds> -- <command...>, and got $# argument(s)" >&2
    return 2
  fi
  what=$1
  bound=$2
  shift 2
  if [ "$1" != "--" ]; then
    echo "::error::retry_fetch: no -- separator before the command; it takes <what> <per-attempt bound in seconds> -- <command...>" >&2
    return 2
  fi
  shift
  refusal="::error::retry_fetch: the per-attempt bound is a positive whole number of seconds, not '$bound'"
  case $bound in
    '' | *[!0-9]*)
      echo "$refusal" >&2
      return 2
      ;;
  esac
  if [ "$bound" -eq 0 ]; then
    echo "$refusal" >&2
    return 2
  fi
  if [ "$#" -eq 0 ]; then
    echo "::error::retry_fetch: nothing to run after --; it takes <what> <per-attempt bound in seconds> -- <command...>" >&2
    return 2
  fi

  attempts=5
  attempt=1
  while :; do
    # `|| status=$?` rather than `if ! timeout ...`: under a caller's errexit a bare failing
    # command would end the step here, before this function could retry anything.
    status=0
    timeout "$bound" "$@" || status=$?
    if [ "$status" -eq 0 ]; then
      return 0
    fi
    left=$((attempts - attempt))
    if [ "$left" -eq 1 ]; then noun=attempt; else noun=attempts; fi
    # 124 is the status GNU coreutils' `timeout` returns when it kills the command, and it is the
    # only status this says reached the bound. BusyBox's `timeout`, which the Alpine-based images
    # run, exits 143 instead — the signal, not GNU's distinct code — and 143 is also what any
    # command killed by SIGTERM from elsewhere exits with, so neither it nor any other status can
    # be read as this bound's own kill.
    if [ "$status" -eq 124 ]; then
      echo "$what: attempt $attempt of $attempts reached its ${bound}s bound (exit 124); $left $noun left" >&2
    else
      echo "$what: attempt $attempt of $attempts failed (exit $status); $left $noun left" >&2
    fi
    if [ "$attempt" -ge "$attempts" ]; then
      echo "::error::$what failed to download after $attempts attempts (last exit $status)" >&2
      return "$status"
    fi
    sleep "$((10 << (attempt - 1)))"
    attempt=$((attempt + 1))
  done
}
