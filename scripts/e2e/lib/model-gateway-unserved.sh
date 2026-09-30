#!/usr/bin/env bash
# Says whether the model gateway key command lib/install-model-gateway.sh wrote left any call
# without a key, and why, so a run whose agent never got a model turn is not scored as a pass or a
# failure of the thing under test. Oh My Pi answers every such call the same way ("No API key found
# for anthropic", exit 1, a transcript with no message entry), whatever the cause.
#
#   bash scripts/e2e/lib/model-gateway-unserved.sh <dest> [<cwd>]
#   bash scripts/e2e/lib/model-gateway-unserved.sh --record <file>
#
# <dest> is the key command's directory (install-model-gateway.sh --dest), whose
# hawk-token.unserved holds every call's line; <cwd>, when given, keeps only the calls made from
# that working directory, which is where Oh My Pi runs a `!command`: an agent's own. --record reads
# one file of the same lines instead, the MODEL_GATEWAY_UNSERVED_FILE a harness named in one
# agent's environment, where no file means the key command served that agent every time. A line
# is one call the command served no key (time, caller pid, caller working directory, reason,
# detail, tab-separated). It prints each kept line after a verdict, and exits:
#   0   every call was served; prints nothing
#   75  STARVED: each unserved call ran out of time (the mint outlasted its budget, or the call's
#       wait for another call's mint did); the run is no verdict on its subject, rerun it
#   77  KEY FAILED: a mint ended without a key before its time ran out (the login missing, locked or
#       refused); a rerun does not fix it, and it outranks a starve
#   2   an argument refusal, a <dest> that holds no key command, or a --record whose directory
#       does not exist
#   1   the record holds a line it cannot read
set -euo pipefail

me=model-gateway-unserved
refuse() {
  echo "$me: $*" >&2
  exit 2
}
[ $# -ge 1 ] && [ $# -le 2 ] || refuse "usage: $0 <dest> [<cwd>] | $0 --record <file>"
want=
if [ "$1" = --record ]; then
  [ $# = 2 ] || refuse "--record needs a value: the MODEL_GATEWAY_UNSERVED_FILE the agent was given"
  record=$2
  [ -d "$(dirname -- "$record")" ] || refuse "--record $record is in no existing directory"
else
  [ -f "$1/hawk-token" ] || refuse "$1 holds no hawk-token key command (lib/install-model-gateway.sh --dest)"
  record=$1/hawk-token.unserved
  [ $# -lt 2 ] || want=$(realpath -m -- "$2")
fi
[ -e "$record" ] || exit 0

timeouts=0
failures=0
lines=()
while IFS=$'\t' read -r at pid cwd reason detail; do
  [ -z "$want" ] || [ "$cwd" = "$want" ] || continue
  case "$reason" in
  timeout) timeouts=$((timeouts + 1)) ;;
  failed) failures=$((failures + 1)) ;;
  *)
    echo "$me: $record holds a line whose reason is '$reason', neither timeout nor failed" >&2
    exit 1
    ;;
  esac
  lines+=("  $at pid $pid in $cwd: $reason: $detail")
done <"$record"

if [ "$failures" -gt 0 ]; then
  echo "$me: KEY FAILED, not scored: the model gateway key command could not mint a key for $failures call(s), and ran out of time for $timeouts; fix the operator's hawk login (hawk-token's stderr is in the key command's hawk-token.log) and rerun:"
  printf '%s\n' "${lines[@]}"
  exit 77
fi
if [ "$timeouts" -gt 0 ]; then
  echo "$me: STARVED, not scored: the model gateway key command ran out of time for $timeouts call(s), so those agents started with no key; the run is no verdict on its subject, rerun it:"
  printf '%s\n' "${lines[@]}"
  exit 75
fi
