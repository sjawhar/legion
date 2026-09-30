#!/usr/bin/env bash
# Says whether the model gateway key command lib/install-model-gateway.sh wrote left an agent
# without a key, and why. Oh My Pi answers every call that gets no key the same way ("No API key
# found for anthropic.", exit 1, and on 18.2.9 an `omp -p` run leaves no session file at all), so
# the run itself cannot say it never got a model turn.
#
#   bash scripts/e2e/lib/model-gateway-unserved.sh --record <file>
#   bash scripts/e2e/lib/model-gateway-unserved.sh --notes <dest> <since> <check>
#   bash scripts/e2e/lib/model-gateway-unserved.sh --fresh <evidence-dir>
#
# The record. The key command appends one line per call an agent makes (the installer's preflight
# is none), to <dest>/hawk-token.calls and, when the caller's environment names one, to its
# MODEL_GATEWAY_CALLS_FILE. Six tab-separated fields, none empty:
#   time     when the call ended, %FT%TZ, UTC
#   pid      its parent: a helper Oh My Pi starts for each call, so it names no agent
#   cwd      the directory Oh My Pi ran it in, physically; on the Go tmux runtime every agent of an
#            issue shares the issue's
#   agent    LEGION_ROLE/LEGION_GENERATION when the caller's environment sets both (a Legion pane);
#            LEGION_ROLE alone when it sets no generation (the operator's controller, which
#            `legion controller start` runs with LEGION_ROLE=controller: `controller`, unless it was
#            started from a shell that already carried a LEGION_GENERATION); - when it sets no role
#            (a harness's `omp -p`)
#   outcome  served; timeout (the call's own deadline, its wait behind another call's mint, or
#            hawk-token saying it spent its whole budget); killed (a signal ended the mint while it
#            had time left); failed (anything else)
#   detail   why: the kept key or the mint's time for served, hawk-token's last stderr line or the
#            key command's own reason otherwise
# An agent is its cwd and agent field together.
#
# --record judges one agent run from its MODEL_GATEWAY_CALLS_FILE: a harness that runs one agent per
# run (the skill-scenario rig: one `omp -p` process, which keeps its key for its life) names a file
# of that run's own, which does not exist before the run, so the file is that agent's and its last
# call decides. It exits 0 when that call was served or the file holds none (no file included), and
# otherwise prints the call after its verdict and exits with it:
#   75  STARVED, not scored: the call ran out of time or a signal ended its mint; rerun the run
#   77  KEY FAILED, not scored: the mint failed for another reason, which a rerun does not fix
# A --record in a directory that does not exist is a harness mistake, refused rather than read as
# served.
#
# --notes is what a stage proof's EXIT trap runs once it has decided the run failed; it never changes
# the run's status. <dest> is the key command's directory (install-model-gateway.sh --dest), <since>
# the time the failing check began (%FT%TZ, UTC, as the record writes it) and <check> its name. It
# prints each agent that could have failed the check for want of a key: when it last got none, from
# where, and why.
#   - An agent whose last call got no key is still without one, and is listed whenever that call
#     came, marked when it came before the check began: it had no key when the check asked it.
#   - An agent served again after its last starve recovered, and is listed, with when it was first
#     served again, when that starve came at or after <since> less the 30 s Oh My Pi's 18.2.9 waits
#     before it runs a failed key command again (resolve-config-value.ts COMMAND_FAILURE_RETRY_MS).
#     Inside that wait a request fails with no key and runs no command, so it leaves no line, and
#     the agent's first calls in the check can fail on a starve just before it.
#   - An agent served again before that window held a key through the whole check, and is left out.
# It says nothing when no agent is listed, or when <dest> holds no record yet.
#
# --fresh is the stage proofs' refusal of a reused evidence directory: it exits 0 when
# <evidence-dir>/model-gateway does not exist, and otherwise prints why the run cannot use it and
# exits 1, for the stage to fail with.
#
# Every form exits 2 on an argument refusal, and --record and --notes 1 on a record line they
# cannot read.
set -euo pipefail

me=model-gateway-unserved
# Oh My Pi 18.2.9's COMMAND_FAILURE_RETRY_MS, in seconds.
retry_s=30
refuse() {
  echo "$me: $*" >&2
  exit 2
}
usage="usage: $0 --record <file> | $0 --notes <dest> <since> <check> | $0 --fresh <evidence-dir>"
# known OUTCOME DETAIL refuses a line of $record the key command does not write: an outcome it does
# not know, or a short line, whose fields have moved.
known() {
  case "$1" in
  served | timeout | killed | failed) ;;
  *)
    echo "$me: $record holds a line whose outcome is '$1', none of served, timeout, killed and failed (or a line of fewer than six fields)" >&2
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
--record)
  [ $# = 2 ] || refuse "$usage"
  [ -d "$(dirname -- "$2")" ] || refuse "--record $2 is in no existing directory"
  record=$2
  [ -e "$record" ] || exit 0
  last=
  while IFS= read -r line; do last=$line; done <"$record"
  [ -n "$last" ] || exit 0
  IFS=$'\t' read -r at pid cwd agent outcome detail <<<"$last"
  known "$outcome" "$detail"
  case "$outcome" in
  served) exit 0 ;;
  failed)
    echo "$me: KEY FAILED, not scored: the model gateway key command left this run's agent without a key for a reason a rerun does not fix:"
    status=77
    ;;
  *)
    echo "$me: STARVED, not scored: the model gateway key command left this run's agent without a key, out of time or with its mint killed; rerun it:"
    status=75
    ;;
  esac
  render "$at" "$pid" "$cwd" "$agent" "$outcome" "$detail"
  echo
  exit "$status"
  ;;
--notes)
  [ $# = 4 ] || refuse "$usage"
  [[ $3 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || refuse "--notes <since> $3 is not a UTC time as %FT%TZ"
  record=$2/hawk-token.calls
  [ -e "$record" ] || exit 0
  lookback=$(date -u -d "$3 $retry_s seconds ago" +%FT%TZ)
  # Per agent: its last outcome, its last starve (time and line), and its first served call after
  # that starve.
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
  echo "$me: ${#lines[@]} agent(s) may have failed check $4 for want of a model key: $without still without one on their last call, and $((${#lines[@]} - without)) served again after a starve from $lookback on ($retry_s s before the check began); if the check waited on one of them, that may be why it failed ($record):"
  printf '%s\n' "${lines[@]}" | sort
  ;;
--fresh)
  [ $# = 2 ] || refuse "$usage"
  [ -e "$2/model-gateway" ] || exit 0
  echo "$2/model-gateway is an earlier run's key command, and its calls are not this run's: give this run an evidence directory of its own"
  exit 1
  ;;
*) refuse "$usage" ;;
esac
