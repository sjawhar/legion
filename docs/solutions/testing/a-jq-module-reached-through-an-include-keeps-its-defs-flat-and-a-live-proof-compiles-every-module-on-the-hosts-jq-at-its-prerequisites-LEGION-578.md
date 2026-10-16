---
title: "A jq module reached through an include keeps its defs flat, and a live proof compiles every module on the host's jq at its prerequisites"
category: testing
tags:
  - jq
  - jq-modules
  - live-proof
  - stage4b
  - prerequisites
date: 2026-10-16
status: active
module: scripts/e2e/lib
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1866"
---

# A jq module reached through an include keeps its defs flat, and a live proof compiles every module on the host's jq at its prerequisites

- jq 1.8.2 aborts at compile — `src/compile.c: nesting_level: Assertion … failed` or
  `expand_call_arglist: Assertion '0 && "Unknown function type"' failed`, exit 134 — on a def of a
  module reached through an include (main → `include "a"`, `a` → `include "b"`) when that def calls
  another def of its own module (`b`'s `bash_command` → `call_arguments` → `device_arguments`). A
  leaf def of `b`, a def of `a` calling a def of `a`, and the same chain from the main program all
  compile; jq 1.6 and 1.7.1 compile every shape. So a module another module includes keeps each def
  it exports a leaf, spelling a shared sub-expression inline rather than through a def of its own.
- A module that includes nothing may chain its own defs freely; the day it gains an include, the
  rule above applies to it, and nothing in the repository's unit tests catches that unless they run
  under the live runner's jq.
- CI's jq is not the runner's: the test job's `jq` and a devbox's differ by minor version, and an
  issue pod's harness bash offers `jq` as a shell builtin. Run the e2e library's tests under every jq
  the proof may meet (1.6, 1.7.1 and 1.8.2 at this writing) before a live run, and never read a
  green unit suite as a compile proof for the runner.
- A live proof compiles and runs every jq module it will load, over a checked-in fixture, at its
  `prerequisites`, failing there with `jq --version` in the message. A compile abort that first
  surfaces at the checkpoint that loads the module costs the run's whole lead-in (an hour at
  `repository-tools`; three at `controller`). The guard covers only the modules it names, so a new
  module joins it the day it is added.

## Evidence

sjawhar/legion#1866 (LEGION-578): the first Stage 4b run at e5bfb6c8 passed `preflight`, `boot`,
`issue-independence` and `repository-configuration`, then died an hour in at `repository-tools`
with `jq: src/compile.c:1158: expand_call_arglist: Assertion '0 && "Unknown function type"' failed`,
because the runner host's jq is 1.8.2 and `lib/stage4b-tools.jq` (included by the main program)
includes `lib/omp-tool-calls.jq`, whose `call_arguments` called `device_arguments`. The library's 14
unit tests were green in the pod (jq 1.7.1) and in CI. Bisection in the pod under 1.8.2 confirmed
the shape: a one-level include with a closure parameter compiled (M1), a zero-arity def chain
reached through the nested include aborted (Y1), the same chain from the main program compiled
(Y6). The fix (ec9f48dc) folded `device_arguments` into `call_arguments` and added the
`prerequisites` self-check (`session_jq … 'include "stage4b-tools"; tool_ran("eval") and
tool_result_answered("lsp"; "hover"; "func") and (tool_ran("notebook") | not)'`), which the next
run reported as `jq-1.8.2 runs lib/stage4b-tools.jq`; the old library under 1.8.2 fails that check
with exit 134. The tester reproduced the abort as a negative control from a copy of the old chain.
The round's second module, `lib/controller-ticks.jq`, includes nothing and chains its own defs
(allowed), and is not yet in the guard — the fast-follow this retro names.
