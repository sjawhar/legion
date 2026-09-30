#!/usr/bin/env bash
# Stand-in `legion` on a worker scenario's PATH. `handoff write|read|complete` run the real
# TypeScript CLI, so handoff validation and the grant redemption against the daemon stand-in are
# real; `gh` and `threads` go to gh-standin.ts, which records and answers from fixtures; every other
# subcommand is the real TypeScript CLI's (a TypeScript pane has no `legion push`). Each call is
# recorded to $SKILL_SCENARIO_RUN/calls.jsonl.
set -euo pipefail
here=$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd -P)
cli=$(cd "$here/../../../daemon/src/cli" && pwd -P)/index.ts
record() {
  jq -cn --arg at "$(date -u +%FT%T.%3NZ)" --arg stdin "${1-}" \
    '{at:$at, as:"legion", argv:$ARGS.positional, stdin:$stdin}' --args -- "${@:2}" \
    >>"$SKILL_SCENARIO_RUN/calls.jsonl"
}
case "${1:-}" in
handoff)
  body=
  if [ "${2:-}" = write ] && [ ! -t 0 ]; then body=$(cat); fi
  record "$body" "$@"
  if [ "${2:-}" = write ]; then printf '%s' "$body" | exec bun "$cli" "$@"; fi
  exec bun "$cli" "$@"
  ;;
gh)
  shift
  [ "${1:-}" = -- ] && shift
  exec bun "$here/gh-standin.ts" gh "$@"
  ;;
threads) exec bun "$here/gh-standin.ts" legion "$@" ;;
*)
  record "" "$@"
  exec bun "$cli" "$@"
  ;;
esac
