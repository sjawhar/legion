---
title: "A writer outside the workflow reads the issue's phase again just before it pushes, waits on every phase the push would undo, held included, and gives each such phase its own reason"
category: daemon
tags:
  - workflow-engine
  - cutover
  - operator-command
  - held
  - push
date: 2026-10-28
status: active
module: packages/daemon
---

# A writer outside the workflow reads the issue's phase again just before it pushes

Extends docs/solutions/daemon/a-rule-on-a-state-that-stands-for-the-head-is-applied-by-every-fact-that-can-make-it-stand-census-the-predicates-writers-LEGION-698.md.

That note says every handler that writes a predicate's input must apply the rule. This note covers
a writer the workflow does not run, such as an operator command or a cutover. Before such a writer
pushes to an issue's branch, under a daemon that carries no push:

1. Read the issue's phase again right before the push, after any slow calls. One read at the
   start of a run is stale by the time the push happens. If the issue left the workflow, push
   nothing.
2. Wait on every phase where a new head undoes work in progress. In testing, the push clears the
   tester's planned red. In reviewing, an approval of a round the reviewer has not completed stops
   standing. In merging, the merger's READY is built at the head it read, and a push past that
   head makes the daemon refuse or void it. In awaiting_merge, the push withdraws READY and sends
   the issue back to implementing.
3. Wait on `held` too. The state does not say which phase the hold came from, and the retry that
   restores that phase would find the push.
4. Give each waiting phase its own reason, checked against that phase's code path, in the code
   comment and the runbook. A reason shared across the set is true of only some of its members.

## Evidence

- sjawhar/legion#2354, `legion handoffs import` (`packages/daemon/internal/handoffimport`):
  - Review round 2 found a `held` issue escaping the deferral, and the phase read only once per
    run.
  - It also found `merging` deferred with awaiting_merge's reason. `withdrawMovedReady` applies in
    awaiting_merge, but merging goes through `voidMovedHeldReady` (`workflow/ready.go`).
- Commit d45c1391 gives each phase its reason in `deferredUnderRecords` and adds `currentPhase`
  for the re-read. It is tested by `TestUnderTheRecordsDaemonHeldAndMergingBranchesAreNotMoved`
  and `TestThePhaseIsReadAgainBeforeTheRemoval`.
