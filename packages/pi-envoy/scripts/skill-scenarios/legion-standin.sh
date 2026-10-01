#!/usr/bin/env bash
# Stand-in `legion` on a worker scenario's PATH. `gh` and `threads` go to gh-standin.ts, which
# records and answers from fixtures; every other subcommand is the real TypeScript CLI's (a
# TypeScript pane has no `legion push`), so handoff validation and the grant redemption against the
# daemon stand-in are real. Each of those calls is recorded to $SKILL_SCENARIO_RUN/calls.jsonl with
# the time it started and the status it exited with.
set -euo pipefail
here=$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd -P)
cli=$(cd "$here/../../../daemon/src/cli" && pwd -P)/index.ts
case "${1:-}" in
gh)
  shift
  [ "${1:-}" = -- ] && shift
  exec bun "$here/gh-standin.ts" gh "$@"
  ;;
threads) exec bun "$here/gh-standin.ts" legion "$@" ;;
*)
  at=$(date -u +%FT%T.%3NZ)
  status=0
  bun "$cli" "$@" || status=$?
  jq -cn --arg at "$at" --argjson exit "$status" '{at:$at, as:"legion", argv:$ARGS.positional, exit:$exit}' \
    --args -- "$@" >>"$SKILL_SCENARIO_RUN/calls.jsonl"
  exit "$status"
  ;;
esac
