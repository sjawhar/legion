---
title: "A supervisor trigger that replaces a process gates the launch before its own side effects, and retries from the orphan sweep"
category: daemon
tags:
  - supervise
  - relaunch
  - checkLaunch
  - orphan-sweep
  - idempotent-trigger
  - stale-plugin
date: 2026-10-13
status: active
module: packages/daemon/internal/supervise
related_issues:
  - "LEGION-164"
  - "sjawhar/legion#1932"
---

# A supervisor trigger that replaces a process gates the launch before its own side effects, and retries from the orphan sweep

Extends docs/solutions/daemon/launch-hold-serve-state-spawn-nothing-until-proven.md.

- A machine action that relaunches a live process (`interrupted` → `launch`) calls
  `m.checkLaunch(ctx)` first, before its log line, before any mark it persists on the claim, and
  before `interrupted` takes the turn's task back — the convention `relaunchAfterFailure` already
  follows ("checked before the charge, so it costs none"). `launch` returns `checkLaunch`'s
  refusal before `letGo`, and `Handle` rolls nothing back, so anything written ahead of that call
  survives a refused attempt: a `relaunching` line that lies, a mark that routes the next
  registration to the charged branch, a task re-prompted to a process that never went away. Skip
  the gate only on the arm that launches nothing (a held suspension, which `endHeld` suspends).
- A trigger posted once at boot needs a periodic re-post, or a refusal (a tree cleanup reserved,
  a moved epoch, a transient store error) strands the claim in the state the trigger meant to
  leave: ride `reconcileOrphans`, the one periodic pass that already reads every claim, and let
  the action's own re-check (the fact still true? the state still live?) make a re-post for a claim
  a prior attempt moved on a no-op. `StaleAddress` is not that pass — the runtime's `Observe` loop
  emits it at `Timeouts.Probe` — so do not describe the sweep as its cadence.
- Say the residual cost in the PR body: while the refusal lasts, each sweep logs one
  `supervise: an event failed …` per stuck claim and nothing else; the same pass carries the
  reason (`tree cleanup is reserved`) that explains it.
- An API refusal keyed on such a trigger's state must be true for as long as the caller can
  authenticate. `persist` clears the capability hash the moment the relaunch moves the claim out
  of a live state, so a predicate on a mark the relaunch sets is unreachable; key it on the fact
  the trigger acts on (the recorded contract differs, in a live state, on a daemon that can
  relaunch) — exactly the window between the discovery and the relaunch — and keep that window
  honest with the retry above.

## Evidence

LEGION-164's stale-plugin relaunch (`internal/supervise/plugin.go` `refreshPlugin`) was written as
`repoint`'s twin and posted once from the boot pass. Review round 1 reproduced a `CheckLaunch`
refusal leaving `state=ready hashLen=10 pluginContract=0 Resume=0` — live, stale, and refused
`409 … the daemon is relaunching this process` on every call until the next restart; fixed by
`refreshStalePlugins(claims, "sweep")` from `reconcileOrphans` (`internal/daemon/daemon.go`).
Review round 2 found the refused attempt had still written the Warn, set `PluginRelaunchFor` and
interrupted the turn; fixed by the gate before all three. The tester reproduced both on the real
store with the tree's cleanup reserved in `tree_lifecycles`: while refused, no Warn,
`pluginRelaunchFor 0`, the delivery confirmed and uninterrupted; once cleared, one Resume at
generation 2, one `trigger=sweep` line, the task taken back
(`TestAStalePluginRelaunchTheLaunchPathRefusesIsRetriedAtTheNextSweep`,
`TestAStalePluginRefusedByTheLaunchPathChangesNothing`). The API predicate is `staleClaim` in
`internal/api/claims.go`; its first draft keyed on `PluginRelaunchFor == DaemonAPIVersion` and was
unreachable in production for the hash reason above.
