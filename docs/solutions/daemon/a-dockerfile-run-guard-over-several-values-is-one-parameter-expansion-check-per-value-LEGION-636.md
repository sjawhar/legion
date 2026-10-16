---
title: "A Dockerfile RUN guard over several values is one parameter-expansion check per value, never an && list"
category: daemon
tags:
  - worker-image
  - dockerfile
  - set-e
  - errexit
  - build-guard
date: 2026-10-16
status: active
module: packages/daemon/docker/worker.Dockerfile
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
---

# A Dockerfile RUN guard over several values is one parameter-expansion check per value, never an && list

Extends docs/solutions/testing/set-e-exempts-a-negated-command-and-non-final-list-operands-a-refute-accepts-exactly-exit-1.md.

- That document's second shape — under `set -e` only the last operand of an `&&`/`||` list can
  exit the shell — holds in a `RUN set -eu; …` of the worker image as it does in a bash harness:
  `[ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ];` fails the build only when `c` is empty.
- Guard each value on its own with the shell's own refusal, which names the value and exits
  whatever `set -e` thinks: `: "${typescript_version:?typescript pin not read from
  /in/language-servers/package.json}";` — one such line per value. `[ -n "$x" ] || exit 1` per
  value works too; the `:?` form carries the message.
- Prove the guard before pushing by emptying each value in turn, first and middle included:
  `sh -c 'set -eu; a=""; b=x; : "${a:?a not read}"; : "${b:?b}"; echo REACHED'` must print
  nothing but the refusal and exit non-zero; the `&&` shape prints `REACHED`, exit 0.

## Evidence (LEGION-636, sjawhar/legion#1984)

The notices stage's guard on the three language-server pins read from `package.json` was the
`&&` list. The reviewer ran it with the first pin empty: `REACHED`, exit 0, so an unparsed
`typescript` or `typescript-language-server` pin would have shipped `THIRD_PARTY_NOTICES` reading
`TypeScript ,`. Round 1 replaced it with three `: "${x:?…}"` checks; the Worker Image build at
98c4c6e4 ran them green.
