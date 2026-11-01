---
title: "A PATH-searching wrapper must reject a shim that hands the call back, and boot paths must kill bounded process groups"
category: testing
tags:
  - jj
  - path
  - shim
  - mise
  - e2e
  - process-group
  - timeout
date: 2026-10-13
status: active
module: scripts/e2e/stage2-tmux-supervision.sh, packages/pi-legion/src/jj-attribution.ts
related_issues:
  - "LEGION-707"
  - "sjawhar/legion#1934"
problem_type: test_failure
component: testing
symptoms:
  - "stage2-tmux-supervision.sh times out at architect-registers-and-is-ready on main and every branch"
  - "jj config path --user never returns from a pane with a relocated XDG_CONFIG_HOME"
  - "orphaned jj loops consume a CPU core after their parent has exited"
root_cause: shim_recursion
resolution_type: code_fix
severity: high
---

# A PATH-searching wrapper must reject a shim that hands the call back

## Problem

The Stage 2 proof timed out at `architect-registers-and-is-ready` before the architect could
announce readiness. The failure was independent of the Legion branch: in a pane, the relocated
`XDG_CONFIG_HOME` hid mise's configuration, so mise had no selected `jj` version. Its `jj` shim
then delegated to the first other `jj` on `PATH`, a user-level `jj` wrapper. That wrapper searched
`PATH` for the first `jj` that was not itself and delegated straight back to mise's shim.

Both shims used `exec`, so the loop stayed in one PID and created no child to reap. The root
bootstrap in `packages/pi-legion/src/jj-attribution.ts` invoked `jj config path --user` to build
its attribution configuration chain. An unbounded invocation held session startup until the
handler timed out, so the readiness claim was never sent. Earlier runs also left loops alive after
their parents exited; each consumed one core for days.

## Root cause

Searching `PATH` for “the real binary” is not enough to distinguish a binary from a shim. A shim
can be a valid `PATH` candidate yet delegate to the wrapper that selected it. Excluding only the
current wrapper detects neither a two-shim cycle nor another path spelling of the same shim.

Relocating XDG configuration made this loop reproducible, but it was not the root cause. Any
configuration state that makes a version-manager shim fall back to a wrapper that selects that
same shim creates the cycle.

## Solution

A path-searching wrapper needs a process-local delegation chain. Before it delegates, record the
candidate under the current PID; when it re-enters, skip every candidate already handed the call
back. Compare candidates by file identity as well as path spelling. If no candidate remains, exit
with an actionable failure instead of retrying a shim.

```bash
for candidate in "${candidates[@]}"; do
  if delegated_by_this_pid "$candidate"; then
    continue
  fi
  remember_delegation_for_this_pid "$candidate"
  exec "$candidate" "$@"
done

exit 127 # name the candidate that handed control back
```

The PID scope matters: a nested, legitimate `jj` invocation must not inherit a parent process's
rejection list and lose the only reachable real binary.

The wrapper is the primary repair. The companion bounded-startup shape in
`packages/pi-legion/src/jj-attribution.ts` (the change reviewed in
[sjawhar/legion#1934](https://github.com/sjawhar/legion/pull/1934)) is the containment boundary:
run the lookup in its own process group, race it against a bootstrap-safe bound, and kill the
whole group when the bound expires. On failure, attribution degrades to no trailer rather than
blocking root bootstrap.

## Prevention

- Test a fake version-manager shim that hands control back to the wrapper. Prove that the wrapper
  reaches a real binary when one exists and fails promptly when every candidate hands control
  back.
- Run that test with a relocated `XDG_CONFIG_HOME`; a normal login shell can hide this class of
  loop by selecting a configured version.
- Every boot-path subprocess needs an explicit bound and process-group cleanup. Killing only the
  leader leaves an `exec` loop or a forked helper behind.
- Treat an abandoned high-CPU process as evidence that a timeout must terminate the entire process
  group, not merely stop waiting for its leader.
