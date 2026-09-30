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
# The key command records every agent's call: time, caller pid, caller working directory, outcome
# and detail, tab-separated, the outcome one of served, timeout (out of time), killed (a signal ended
# its mint) and failed (anything else). Oh My Pi retries a failed key command 30 s later and after
# a 401, and a relaunched pane calls again.
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
# prints each agent (the calls from one working directory) that got no key at or after <since>:
# when, from which directory, and why. One whose last call got no key is still without one. One
# served again later is listed too, with when: it recovered, but spent the 30 s Oh My Pi waits
# before retrying, which can time out a check that waited on it. A starve before <since> is not
# the failing check's, whether the agent was served again before the check began or made no call
# during it, so it is left out; so is every call an earlier run left, which predates <since>. It says
# nothing when no agent went without a key in that window, or when <dest> holds no record yet.
#
# --fresh is the stage proofs' refusal of a reused evidence directory: it exits 0 when
# <evidence-dir>/model-gateway does not exist, and otherwise prints why the run cannot use it and
# exits 1, for the stage to fail with.
#
# Every form exits 2 on an argument refusal, and --record and --notes 1 on a record line whose
# outcome they do not know.
set -euo pipefail

me=model-gateway-unserved
refuse() {
  echo "$me: $*" >&2
  exit 2
}
usage="usage: $0 --record <file> | $0 --notes <dest> <since> <check> | $0 --fresh <evidence-dir>"
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
# render AT PID CWD OUTCOME DETAIL prints one record line as both forms show it.
render() { printf '  %s in %s (pid %s): %s: %s' "$1" "$3" "$2" "$4" "$5"; }

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
  render "$at" "$pid" "$cwd" "$outcome" "$detail"
  echo
  exit "$status"
  ;;
--notes)
  [ $# = 4 ] || refuse "$usage"
  [[ $3 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || refuse "--notes <since> $3 is not a UTC time as %FT%TZ"
  record=$2/hawk-token.calls
  [ -e "$record" ] || exit 0
  declare -A last_outcome=() last_at=() starve_at=() starve_line=()
  while IFS=$'\t' read -r at pid cwd outcome detail; do
    known "$outcome"
    last_outcome[$cwd]=$outcome
    last_at[$cwd]=$at
    if [ "$outcome" != served ]; then
      starve_at[$cwd]=$at
      starve_line[$cwd]=$(render "$at" "$pid" "$cwd" "$outcome" "$detail")
    fi
  done <"$record"
  lines=()
  without=0
  for cwd in "${!starve_at[@]}"; do
    [[ ! ${starve_at[$cwd]} < $3 ]] || continue
    if [ "${last_outcome[$cwd]}" = served ]; then
      lines+=("${starve_line[$cwd]}; served again at ${last_at[$cwd]}")
    else
      lines+=("${starve_line[$cwd]}")
      without=$((without + 1))
    fi
  done
  [ "${#lines[@]}" != 0 ] || exit 0
  echo "$me: since check $4 began ($3), ${#lines[@]} agent(s) got no model key, $without of them still without one on their last call; if the check waited on one of them, that may be why it failed ($record):"
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
