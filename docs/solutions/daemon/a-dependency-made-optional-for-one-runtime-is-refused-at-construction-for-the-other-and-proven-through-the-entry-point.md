---
title: "A dependency made optional for one runtime is refused at construction for the other, and proven through the real entry point, not the test helper"
category: daemon
tags:
  - dependency-injection
  - optional-dependency
  - construction-time-refusal
  - composition-root
  - entry-point-test
  - test-helper-blind-spot
  - call-site-audit
  - workspace-removal
  - negative-control
date: 2026-09-15
status: active
module: packages/daemon/src/daemon
problem_type: defect
severity: medium
related_issues:
  - "LEGION-163"
  - "sjawhar/legion#1107"
  - "LEGION-104"
  - "sjawhar/legion#1031"
  - "LEGION-25"
symptoms:
  - "failed to remove the workspace of <KEY> (...) at the close of tree <KEY>: Error: runtime owns workspace cleanup but ProcessManager has no command runner"
  - "a merged feature does nothing in production while every test of it is green"
  - "a later PR made a dependency optional and the one production call site dropped it with no compile error and no test failure"
  - "jj workspace list -R <clone> still lists a done issue's workspace after its tree closed"
applies_when:
  - You make a constructor dependency optional because one runtime, backend, or configuration never uses it
  - A feature's production dependency is supplied only by a test helper that always injects it
  - You add a construction-time guard to a class that many test rigs construct directly
  - You are writing the negative control for a fix whose whole point is that a failure now happens earlier
---

# A dependency made optional for one runtime is refused at construction for the other, and proven through the real entry point

## What happened

LEGION-104 (`removeTreeWorkspaces`) taught `ProcessManager` to remove a finished issue's jj
workspace when its tree closes under the tmux runtime. The jj commands run through
`ProcessManagerDeps.run`, the daemon-host command runner. Every one of the hundreds of daemon
tests builds `ProcessManager` through `processes.test.ts`'s `manager()` helper, which always
injected `run`. The tests were green; the feature was reviewed, tested, merged.

PR #1031 (LEGION-25, the Kubernetes runtime) then made `run` optional — correct, Kubernetes
retains its tree volume and never runs a host jj command — and in the same change the one
production call site, `new ProcessManager({...})` in `startDaemonLocked` (`index.ts`), stopped
passing `run: runner`. Nothing objected: not the compiler (the field is optional), not a test
(the helper still injected it), not review (the diff was a Kubernetes change). From that deploy
on, every tmux tree close reached `workspaceCommandRunner` with `run` undefined and logged

```
[legion] failed to remove the workspace of LEGION-6 (…/workspaces/sjawhar/legion/legion-6) at the close of tree LEGION-6: Error: runtime owns workspace cleanup but ProcessManager has no command runner
```

— once per close, never retried (LEGION-104's own rule), the workspace and its commits left in
place. The feature had shipped and done nothing in production, and the only thing that noticed
was the production check.

## The shape, so you recognise it next time

1. A feature's production dependency is wired **only** in the tests' construction helper. The
   entry point had it too, at the time — but no test ever proved the entry point had it.
2. A later change makes that dependency **optional** because a second configuration genuinely
   does not need it. Making a field optional does not remove the requirement for the
   configurations that still have it; it removes the compiler's ability to notice a dropped one.
3. The production call site drops the argument. With the type now optional and the helper still
   injecting, the drop is invisible to every existing signal.
4. The failure surfaces at **first use**, hours after boot, in a code path that logs and
   continues — the worst place for it.

Type-level discrimination (a `Runtime` union whose tmux member requires `run`) was considered and
rejected as disproportionate for one conditional field (`Runtime` stays one interface with a
boolean `removesWorkspacesOnTreeClose`; the spec's non-goals). When the requirement depends on a
runtime *value* and a type split is too much machinery, the boring alternative below is enough.

## The fix pattern

**Refuse at construction, name the condition and the runtime.** First statement of the
`ProcessManager` constructor:

```ts
if (deps.runtime.removesWorkspacesOnTreeClose && deps.run === undefined) {
  throw new Error(
    `ProcessManager needs a command runner: the ${deps.config.runtime.name} runtime removes workspaces at tree close`
  );
}
```

`Runtime` has no name field; `deps.config.runtime.name` (`"tmux" | "kubernetes"`) is what the
operator selected and what the entry point chose the runtime from, so the message names that.
Throwing before any timer or watchdog is built leaves nothing to dispose. The first-use throw
in `workspaceCommandRunner` stays as the last line of defence — the guard adds a check, it does
not move one.

**Prove it through the real entry point, not the helper.** The regression test that would have
caught #1031 boots `startDaemon` — the production composition root, with only the runner, bus,
clock, and timers injected — loads a state holding a `done` root that is `lingering` past its
deadline with no locator, captures the daemon's single `setInterval` (the linger sweep), fires it
once, and asserts the removal ran: `commands` holds
`/tools/jj workspace forget widgets-42 --ignore-working-copy -R <clone>`, `console.error` holds
`[legion] removed the workspace of WIDGETS-42 … abandoned 1 commit(s) nothing else reached`, no
`failed to remove` line, and the workspace directory is gone. Two fixture facts that cost time:
`createDaemonRunner` rewrites `jj` to the environment's absolute path (`/tools/jj` in
`index.test.ts`) before the injected runner sees the command, so match that; and
`removeIssueWorkspace` asks jj only when `<clone>/.jj` exists, so create it. A test built through
`manager()` cannot prove this — the helper *is* the thing that hid the gap. Keep at least one
test per composition root that constructs the system the way production does, and when a
dependency changes shape, ask "does the helper's default still match what production supplies,
**including what production omits**?"

The constructor's own contract gets a unit test beside the feature it protects: the tmux-shaped
construction without `run` throws the exact message; a `FakeRuntime({ removesWorkspacesOnTreeClose:
false })` without `run` constructs — the Kubernetes companion, so the guard never over-refuses.
`manager()` gained an `omitRun` flag for it, because the helper destructures `run` out of its
options before spreading overrides, so `run: undefined` in overrides is not enough.

## The blast radius of a construction-time guard, and the audit that finds it

The guard refuses every construction shaped like the pre-fix entry point — which is its job, and
which includes **test rigs**. The plan's survey grepped `new ProcessManager(` inside
`processes.test.ts` only; the first push's `Tests` job failed 10 tests in four *other* files that
build a real `TmuxRuntime` `ProcessManager` directly without `run` (`processes.thermo-ops.test.ts`,
`real-deployment-instructions-e2e.test.ts`, `real-prompt-delivery-e2e.test.ts`,
`real-shutdown-e2e.test.ts`), every one with the new message. Three of the four are
`LEGION_E2E=1`-gated and skipped locally, so a green local run said nothing about them
(`../legion/proof-without-the-rig-what-the-daemon-suite-and-ci-caught-and-the-one-gap-they-left.md`).
The fix was one line each — hand `ProcessManager` the runner the rig already hands `TmuxRuntime`.

Before pushing a construction-time refusal on a widely-constructed class:

```bash
grep -rn 'new ProcessManager(' packages/          # every construction site, not the canonical test file
LEGION_E2E=1 LEGION_TMUX_LIVE=1 bun test <each file the grep names that is env-gated>
```

This is `../testing/call-site-audit-for-test-isolation.md`'s rule — the audit unit is every call
to the function that creates the system under test — applied to a guard instead of an isolation
fix. The rigs that failed were shaped exactly like the broken entry point; the guard proved the
audit was incomplete the same way it would have proved #1031 was.

## Two negative controls, and why the second is the better one

The RED run on the unfixed entry point is the first: the new `startDaemon` test fails on the
`failed to remove` filter, printing the production error verbatim
(`… Error: runtime owns workspace cleanup but ProcessManager has no command runner`). Run it
**before** the fix and quote it — that line is the negative control the PR body and the handoff
need, and it is the same string the production log carried, which is what let the reviewer tie
the test to the incident.

The tester's control is stronger and is the one to write once a construction-time guard exists:
delete `run: runner,` from `index.ts` at the PR head and re-run the same test. It still fails —
but `startDaemon` now rejects at `new ProcessManager` (`startDaemonLocked`, `index.ts`) with the
guard's message **before** `legion daemon listening` is ever printed: the API never binds, no tree
close runs. That shows the failure *moved* from the first tree close to boot, which is the whole
claim of the fix. A negative control for a fail-fast change should demonstrate where the failure
now lands, not only that the test goes red.

## Related

- `optional-dep-fallback-and-early-return-guard.md` — the sibling optional-dependency shape,
  where the dependency has a real default and the pitfall is a dead truthiness guard; here the
  dependency has no default under one runtime and the pitfall is the requirement going unchecked.
- `../testing/call-site-audit-for-test-isolation.md` — grep every construction site, not the
  canonical helper; the discipline this retro's CI round re-learned.
- `../testing/fixtures-derive-what-production-derives.md` — the value-mismatch cousin: helpers
  that diverge from production in what they compute, rather than in what they supply.
- `launch-hold-serve-state-spawn-nothing-until-proven.md` — the daemon's other boot-time
  refusals; a guard that fires at construction belongs to the same family as a probe that fails
  at the launch hold.
- `two-documented-models-one-predicate-and-what-a-removed-tree-leaves-behind.md` — LEGION-104,
  the feature this wiring gap silenced.
