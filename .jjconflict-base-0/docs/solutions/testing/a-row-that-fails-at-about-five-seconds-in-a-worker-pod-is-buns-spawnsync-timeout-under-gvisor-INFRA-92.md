---
title: "A row that fails at about five seconds in a worker pod is Bun's spawnSync timeout under gVisor, not a regression"
category: testing
tags:
  - flaky-test
  - bun
  - gvisor
  - issue-pod
  - subprocess-timeout
date: 2026-10-25
status: active
module: packages/claude-envoy
related_issues:
  - "INFRA-92"
  - "sjawhar/legion#2154"
---

# A row that fails at about five seconds in a worker pod is Bun's spawnSync timeout under gVisor, not a regression

`packages/claude-envoy/tests/session-title.test.ts`'s row "queryTmux is bounded even by a tmux
that ignores SIGTERM" fails in a Legion worker pod roughly one full-suite run in three, at about
five seconds against a three-second bound, and is green on CI. Read this before spending a round
on it.

- **The cause is the runtime, and no Legion code is in it.** Bun 1.3.14's `Bun.spawnSync`
  sometimes does not deliver the kill its own `timeout` promises, on linux 4.19.0-gvisor. The
  reproduction names nothing of ours: `Bun.spawnSync({ cmd: ["sleep", "5"], timeout: 1000,
  killSignal: "SIGKILL" })` in a pod returns `1004 ms/SIGKILL`, `1003 ms/SIGKILL`,
  `1004 ms/SIGKILL`, then `5006 ms/exit 0`. The same holds through `/bin/sh -c`, with a
  TERM-trapping child, and with stdout to a file rather than a pipe.
- **CI's runners are the judge of that row.** A pod's red is the host; a red on CI is the code.
- **Do not widen the bound and do not skip the row.** Widening hides the thing it exists to catch
  — that a tmux ignoring `SIGTERM` is cut off at `queryTmux`'s one-second `execFileSync` timeout
  rather than running its five seconds — and a skip is a round spent on a test-only change.
- **Measure the call, not the harness around it.** The row originally timed a whole
  `Bun.spawnSync` of a child that imported the module under test, so Bun's startup and a
  TypeScript import sat inside the bound; a loaded machine reddened it for a reason that was not
  the bound. It now has the child time its own call and print the number. Make a timing row
  measure the thing it names: the first diagnosis of this flake was wrong precisely because the
  measurement was wider than the claim.
- **The product cannot reach it.** `queryTmux` runs only through `tmuxPaneTitle`, which
  `sessionTitle` calls only when `TMUX` and `TMUX_PANE` are both set
  (`packages/claude-envoy/src/session-title.ts`, the `sessionTitle` body). A worker pod — the
  gVisor host — runs no tmux and sets neither, and the tmux runtime's panes are not on gVisor. So
  this is a test that flakes in one environment, not an agent that can hang.

## Evidence

INFRA-92 saw the row fail two of seven full `bun test` runs at one head (5,080 ms and 5,222 ms),
three of eight at the next, and two of six at the last, passing every other run including three
consecutive clean ones. The implementer's first diagnosis — that the bound covered Bun's startup
and the import, so a slow runner reddened it — was reported to the architect and was **wrong**:
after the measurement moved inside the child, the failures persisted at 5,018, 5,019, 5,023,
5,025 and 5,042 ms, which is the stub's whole `sleep 5` with the harness excluded. The bare
`spawnSync` probe above is what settled it.

The project's stream lead decided it is no issue: the product cannot hit it and the root cause is
upstream with Bun. This document is that decision's record, so the next reader neither re-opens it
nor re-measures it.
