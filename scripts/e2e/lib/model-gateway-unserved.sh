#!/usr/bin/env bash
# Says whether the model gateway key command lib/install-model-gateway.sh wrote left an agent
# without a key, and why. Oh My Pi answers every call that gets no key the same way ("No API key
# found for anthropic.", exit 1, and on 18.2.9 an `omp -p` run leaves no session file at all), so
# the run itself cannot say it never got a model turn.
#
#   bash scripts/e2e/lib/model-gateway-unserved.sh --record <file>
#   bash scripts/e2e/lib/model-gateway-unserved.sh --notes <status> <dest> <since> <check>
#
# The key command records every call: time, caller pid, caller working directory, outcome and
# detail, tab-separated, the outcome one of served, timeout (out of time), killed (a signal ended its
# mint) and failed (anything else). An agent's last call decides whether it went without a key: Oh
# My Pi retries a failed key command 30 s later and after a 401, and a relaunched pane calls again.
#
# --record judges one agent run from its MODEL_GATEWAY_CALLS_FILE: a harness that runs one agent per
# run (the skill-scenario rig: one `omp -p` process, which keeps its key for its life) names a file
# of that run's own, which does not exist before the run, so the file is that agent's. It exits 0
# when the file's last call was served or it holds none (no file included), and otherwise prints
# that call after its verdict and exits with it:
#   75  STARVED, not scored: the call ran out of time or a signal ended its mint; rerun the run
#   77  KEY FAILED, not scored: the mint failed for another reason, which a rerun does not fix
# A --record in a directory that does not exist is a harness mistake, refused rather than read as
# served.
#
# --notes is a stage proof's diagnostic for a failed run, and never changes its status. <status> is
# the status the run is exiting with, <dest> the key command's directory this run created
# (install-model-gateway.sh --dest) or empty until it did, since a directory an earlier run left
# holds that run's calls, <since> the time the failing check began (%FT%TZ, UTC, as the record
# writes it) and <check> its name. When <status> is not 0 it prints, for each agent (the calls from
# one working directory) whose last call got no key and came at or after <since>, that call: when,
# from which directory, and why. A run-wide "not scored" could not be honest: in Stage 4b the key
# command's one caller is the operator's controller, while every pod uses its projected token. So
# the notes say only whether starvation could explain the failing check, and a person reads them.
#
# Either form exits 2 on an argument refusal, and 1 on a record line whose outcome it does not know.
set -euo pipefail

me=model-gateway-unserved
refuse() {
  echo "$me: $*" >&2
  exit 2
}
usage="usage: $0 --record <file> | $0 --notes <status> <dest> <since> <check>"
# known OUTCOME refuses a line of $record whose outcome the key command does not write.
known() {
  case "$1" in
  served | timeout | killed | failed) ;;
  *)
    echo "$me: $record holds a line whose outcome is '$1', none of served, timeout, killed and failed" >&2
    exit 1
    ;;
  esac
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
  IFS=$'\t' read -r at pid cwd outcome detail <<<"$last"
  known "$outcome"
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
  printf '  %s pid %s in %s: %s: %s\n' "$at" "$pid" "$cwd" "$outcome" "$detail"
  exit "$status"
  ;;
--notes)
  [ $# = 5 ] || refuse "$usage"
  if ! [[ $2 =~ ^[0-9]+$ ]]; then refuse "--notes $2 is not an exit status"; fi
  [[ $4 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || refuse "--notes <since> $4 is not a UTC time as %FT%TZ"
  if [ "$2" = 0 ] || [ -z "$3" ] || [ ! -e "$3/hawk-token.calls" ]; then exit 0; fi
  record=$3/hawk-token.calls
  declare -A last_outcome=() last_at=() last_line=()
  while IFS=$'\t' read -r at pid cwd outcome detail; do
    known "$outcome"
    last_outcome[$cwd]=$outcome
    last_at[$cwd]=$at
    last_line[$cwd]="  $at in $cwd (pid $pid): $outcome: $detail"
  done <"$record"
  lines=()
  for cwd in "${!last_outcome[@]}"; do
    [ "${last_outcome[$cwd]}" != served ] || continue
    [[ ! ${last_at[$cwd]} < $4 ]] || continue
    lines+=("${last_line[$cwd]}")
  done
  [ "${#lines[@]}" != 0 ] || exit 0
  echo "$me: since check $5 began ($4), ${#lines[@]} agent(s) got no model key on their last call; if the check waited on one of them, that may be why it failed ($3/hawk-token.calls):"
  printf '%s\n' "${lines[@]}" | sort
  ;;
*) refuse "$usage" ;;
esac
