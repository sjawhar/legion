#!/usr/bin/env bash
# Says whether the model gateway key command lib/install-model-gateway.sh wrote left an agent
# without a key, and why, so a run in which an agent never got a model turn is scored neither as a
# pass nor as a failure of the thing under test. Oh My Pi answers every call that gets no key the
# same way ("No API key found for anthropic.", exit 1, and no session transcript at all or one
# holding no assistant message), whatever the cause.
#
#   bash scripts/e2e/lib/model-gateway-unserved.sh --record <file>
#   bash scripts/e2e/lib/model-gateway-unserved.sh --run-exit <status> <dest> <since>
#
# The key command records every call: time, caller pid, caller working directory, outcome and
# detail, tab-separated, the outcome one of served, timeout, killed and failed. An agent is the
# calls made from one working directory, which is where Oh My Pi runs a `!command`, and it is left
# without a key when its last call was not served: Oh My Pi retries a failed key command 30 s later
# and on a 401, and a pane relaunched after a starve calls again, so an agent served since then has
# recovered. The verdict, printed with each such agent's last call, is
#   STARVED, not scored (75)    each agent left without a key ran out of time (timeout) or had its
#                               mint ended by a signal (killed): rerun the run
#   KEY FAILED, not scored (77) an agent's last mint failed for another reason: a rerun does not fix
#                               it, so it outranks a starve
#
# --record reads one agent's MODEL_GATEWAY_CALLS_FILE, for a harness that scores each agent's run
# from its own directory, and exits 0 when its last call was served or it holds none (no file
# included), else with the verdict's status.
#
# --run-exit is a stage proof's EXIT trap. <status> is the status the run is exiting with, <dest>
# the key command's directory this run created (install-model-gateway.sh --dest), or empty until
# it did, since a directory an earlier run left holds that run's calls, and <since> the time the
# failing check began, as the record writes it (%FT%TZ, UTC). It exits with the status the run ends
# with: <status> itself, unless <status> is 1 (a failed run) and an agent's last call, made at or
# after <since>, got no key, when it prints the verdict and exits with its status instead. A check
# that fails for its own reason after every agent was served ends 1. A trap that ends the run itself
# calls it as `… || exit "$?"`: a bare exit in a trap keeps the status the trap began with.
#
# Either form exits 2 on an argument refusal (a --record in a directory that does not exist
# included), and 1 on a record line whose outcome it does not know.
set -euo pipefail

me=model-gateway-unserved
refuse() {
  echo "$me: $*" >&2
  exit 2
}
usage="usage: $0 --record <file> | $0 --run-exit <status> <dest> <since>"

# judge RECORD SINCE prints the verdict and each unserved agent's last call made at or after SINCE
# (every call when SINCE is empty), and sets verdict_status to 0 (none), 75 or 77.
judge() {
  local at pid cwd outcome detail starved=0 failed=0 lines=()
  local -A last_outcome=() last_at=() last_line=()
  verdict_status=0
  [ -e "$1" ] || return 0
  while IFS=$'\t' read -r at pid cwd outcome detail; do
    case "$outcome" in
    served | timeout | killed | failed) ;;
    *)
      echo "$me: $1 holds a line whose outcome is '$outcome', none of served, timeout, killed and failed" >&2
      exit 1
      ;;
    esac
    last_outcome[$cwd]=$outcome
    last_at[$cwd]=$at
    last_line[$cwd]="  $at pid $pid in $cwd: $outcome: $detail"
  done <"$1"
  for cwd in "${!last_outcome[@]}"; do
    [ "${last_outcome[$cwd]}" != served ] || continue
    [[ -z $2 || ! ${last_at[$cwd]} < $2 ]] || continue
    if [ "${last_outcome[$cwd]}" = failed ]; then failed=$((failed + 1)); else starved=$((starved + 1)); fi
    lines+=("${last_line[$cwd]}")
  done
  if [ "$failed" -gt 0 ]; then
    echo "$me: KEY FAILED, not scored: the model gateway key command left $failed agent(s) without a key for a reason a rerun does not fix, and $starved more starved; each line says why:"
    verdict_status=77
  elif [ "$starved" -gt 0 ]; then
    echo "$me: STARVED, not scored: the model gateway key command left $starved agent(s) without a key, out of time or with their mint killed; the run is no verdict on its subject, rerun it:"
    verdict_status=75
  fi
  [ "${#lines[@]}" = 0 ] || printf '%s\n' "${lines[@]}" | sort
}

case "${1:-}" in
--record)
  [ $# = 2 ] || refuse "$usage"
  [ -d "$(dirname -- "$2")" ] || refuse "--record $2 is in no existing directory"
  judge "$2" ""
  exit "$verdict_status"
  ;;
--run-exit)
  [ $# = 4 ] || refuse "$usage"
  if ! [[ $2 =~ ^[0-9]+$ ]] || [ "$2" -gt 255 ]; then refuse "--run-exit $2 is not an exit status"; fi
  [[ $4 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || refuse "--run-exit <since> $4 is not a UTC time as %FT%TZ"
  if [ "$2" != 1 ] || [ -z "$3" ]; then exit "$2"; fi
  judge "$3/hawk-token.calls" "$4"
  [ "$verdict_status" = 0 ] || exit "$verdict_status"
  exit 1
  ;;
*) refuse "$usage" ;;
esac
