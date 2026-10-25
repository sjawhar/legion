#!/usr/bin/env bash
# Says whether the model-route key command lib/install-key-file-model-route.sh wrote left an agent
# without a key, and why. Oh My Pi answers every call that gets no key the same way ("No API key
# found for anthropic.", exit 1, and on 18.2.9 an `omp -p` run leaves no session file at all), so
# the run itself cannot say it never got a model turn.
#
#   bash scripts/e2e/lib/model-gateway-unserved.sh --notes <dest> <since> <check>
#   bash scripts/e2e/lib/model-gateway-unserved.sh --fresh <evidence-dir>
#
# The record. The key command appends one line per call an agent makes (the installer's preflight
# is none), to <dest>/<key-command-name>.calls (the one such file a route's own --dest ever holds).
# Six tab-separated fields, none empty:
#   time     when the call ended, %FT%TZ, UTC
#   pid      its parent: a helper Oh My Pi starts for each call, so it names no agent
#   cwd      the directory Oh My Pi ran it in, physically; on the Go tmux runtime every agent of an
#            issue shares the issue's
#   agent    LEGION_ROLE/LEGION_GENERATION when the caller's environment sets both (a Legion pane);
#            LEGION_ROLE alone when it sets no generation (the operator's controller, which
#            `legion controller start` runs with LEGION_ROLE=controller: `controller`, unless it was
#            started from a shell that already carried a LEGION_GENERATION); - when it sets no role
#            (a harness's `omp -p`)
#   outcome  served, or failed (the key file could not be read, or was empty)
#   detail   why: the path it read from for served, the key command's own reason otherwise
# An agent is its cwd and agent field together.
#
# --notes is what a stage proof's EXIT trap runs once it has decided the run failed; it never changes
# the run's status. <dest> is the route's call-record directory (install-key-file-model-route.sh
# --dest; the key command itself lives under the route's own --key-dir), <since>
# the time the failing check began (%FT%TZ, UTC, as the record writes it) and <check> its name. It
# prints each agent that could have failed the check for want of a key: when it last got none, from
# where, and why.
#   - An agent whose last call got no key is still without one, and is listed whenever that call
#     came, marked when it came before the check began: it had no key when the check asked it.
#   - An agent served again after its last failed call recovered, and is listed, with when it was
#     first served again, when that failure came at or after <since> less the 30 s Oh My Pi waits
#     before it runs a failed key command again (resolve-config-value.ts COMMAND_FAILURE_RETRY_MS,
#     30 s in the Oh My Pi .omp-pin names).
#     Inside that wait a request fails with no key and runs no command, so it leaves no line, and
#     the agent's first calls in the check can fail on a failure just before it.
#   - An agent served again before that window held a key through the whole check, and is left out.
# It says nothing when no agent is listed, or when <dest> holds no record yet.
#
# --fresh is the stage proofs' refusal of a reused evidence directory, the first thing each does:
# it exits 0 when <evidence-dir> does not exist or is an empty directory it can list, and otherwise
# prints why the run cannot use it and exits 1, for the stage to fail with. Any content is an
# earlier run's evidence, whether or not that run got as far as installing its key command, and a
# directory it cannot list may hold some.
#
# Every form exits 2 on an argument refusal, and --notes 1 on a record line it cannot read.
set -euo pipefail

me=model-gateway-unserved
# Oh My Pi's COMMAND_FAILURE_RETRY_MS, in seconds (30 in the Oh My Pi .omp-pin names).
retry_s=30
refuse() {
  echo "$me: $*" >&2
  exit 2
}
usage="usage: $0 --notes <dest> <since> <check> | $0 --fresh <evidence-dir>"
# known OUTCOME DETAIL refuses a line of $record the key command does not write: an outcome it does
# not know, or a short line, whose fields have moved.
known() {
  case "$1" in
  served | failed) ;;
  *)
    echo "$me: $record holds a line whose outcome is '$1', neither served nor failed (or a line of fewer than six fields)" >&2
    exit 1
    ;;
  esac
  [ -n "$2" ] || {
    echo "$me: $record holds a line with no detail, which the key command never writes" >&2
    exit 1
  }
}
# render AT PID CWD AGENT OUTCOME DETAIL prints one record line as every form shows it.
render() {
  if [ "$4" = - ]; then
    printf '  %s in %s (pid %s): %s: %s' "$1" "$3" "$2" "$5" "$6"
  else
    printf '  %s %s in %s (pid %s): %s: %s' "$1" "$4" "$3" "$2" "$5" "$6"
  fi
}

case "${1:-}" in
--notes)
  [ $# = 4 ] || refuse "$usage"
  [[ $3 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || refuse "--notes <since> $3 is not a UTC time as %FT%TZ"
  # <dest> holds the route's own call record, anthropic-key.calls: a route that never reached its
  # first call leaves none yet, read the same as an empty one below.
  calls=("$2"/*.calls)
  case "${#calls[@]}" in
  1) [ -e "${calls[0]}" ] && record=${calls[0]} || exit 0 ;;
  *) refuse "--notes $2 holds more than one *.calls file (${calls[*]}); it is not one route's own --dest" ;;
  esac
  lookback=$(date -u -d "$3 $retry_s seconds ago" +%FT%TZ)
  # Per agent: its last outcome, its last failure (time and line), and its first served call after
  # that failure.
  declare -A last_outcome=() starve_at=() starve_line=() served_after=()
  while IFS=$'\t' read -r at pid cwd agent outcome detail; do
    known "$outcome" "$detail"
    key="$cwd $agent"
    last_outcome[$key]=$outcome
    if [ "$outcome" = served ]; then
      [ -z "${starve_at[$key]:-}" ] || [ -n "${served_after[$key]:-}" ] || served_after[$key]=$at
    else
      starve_at[$key]=$at
      starve_line[$key]=$(render "$at" "$pid" "$cwd" "$agent" "$outcome" "$detail")
      served_after[$key]=
    fi
  done <"$record"
  lines=()
  without=0
  for key in "${!starve_at[@]}"; do
    before=
    [[ ! ${starve_at[$key]} < $3 ]] || before=" (before check $4 began)"
    if [ "${last_outcome[$key]}" = served ]; then
      [[ ! ${starve_at[$key]} < $lookback ]] || continue
      lines+=("${starve_line[$key]}$before; served again at ${served_after[$key]}")
    else
      lines+=("${starve_line[$key]}$before")
      without=$((without + 1))
    fi
  done
  [ "${#lines[@]}" != 0 ] || exit 0
  echo "$me: ${#lines[@]} agent(s) may have failed check $4 for want of a model key: $without still without one on their last call, and $((${#lines[@]} - without)) served again after a failure from $lookback on ($retry_s s before the check began); if the check waited on one of them, that may be why it failed ($record):"
  printf '%s\n' "${lines[@]}" | sort
  ;;
--fresh)
  [ $# = 2 ] || refuse "$usage"
  [ -e "$2" ] || exit 0
  if [ -d "$2" ]; then
    if ! entries=$(ls -A -- "$2" 2>&1); then
      echo "$2 cannot be listed ($entries), so this run cannot tell whether it holds an earlier run's evidence; give this run an evidence directory of its own"
      exit 1
    fi
    [ -n "$entries" ] || exit 0
  fi
  echo "$2 is not an empty directory: it holds an earlier run's evidence, which this run must neither write into nor read as its own; give this run an evidence directory of its own"
  exit 1
  ;;
*) refuse "$usage" ;;
esac
