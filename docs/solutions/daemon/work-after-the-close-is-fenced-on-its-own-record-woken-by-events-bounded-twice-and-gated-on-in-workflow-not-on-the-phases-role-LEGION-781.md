---
title: "Work after the close is fenced on its own record, woken by events, bounded twice, and gated on InWorkflow, never on the phase's role"
category: daemon
tags:
  - workflow-engine
  - production-check
  - linger
  - outbox-fence
  - reopened-root
  - untrusted-input
  - predicate-writers
date: 2026-10-21
status: active
module: packages/daemon/internal/workflow
related_issues:
  - "LEGION-781"
  - "sjawhar/legion#2059"
---

# Work after the close is fenced on its own record, woken by events, bounded twice, and gated on InWorkflow, never on the phase's role

Work the daemon runs for an issue after its merge closed it (the production check: the
implementer's looks at whether production carries the merge) runs where no phase, generation or
tree fence holds any more. These are the rules that made it safe.

- **Fence it on its own record, not on the issue's phase or generation.** The check's identity is
  its `OpenedAt` (clock truncated to the microsecond, Postgres's precision), and every row and fact
  of a look carries it (`record.SuperviseRequest.CheckOpenedAt`, `CheckLook`): a row of a check a
  later merge replaced, of a look that already ended, or of a closed check finishes without
  acting (`daemon/outbox.go` `checkLookHolds`). A phase start's generation fence says nothing here:
  the issue is `done` and its tree may linger.
- **Wake it by events, keep one pending cause, never start a look into an open one.** A wake
  (`issue.deployed`, the daemon's boot on a build, Dispatch's `server.started`, the Dispatch build
  read at boot) that arrives while a look is open or the issue runs again is kept as the check's
  one pending cause and starts the next look when the current one ends; the same build woke it
  once (`check.go` `wake`).
- **Bound every look and the whole check, and let the bound end a silent look as a pending
  report would.** `check_look` ends a look nobody reported (suspend the implementer, keep session
  and volume, the pod goes); `check_bound` starts the last look and a `pending` there closes the
  check as `not_run`. The implementer's failed claim ends a look the same way — only while the
  issue is out of the workflow.
- **"Runs again" is `InWorkflow(issue)`: status in the workflow and phase short of `done`.**
  Never `RoleFor(phase) != ""`: that answers "which role works this phase" and is empty for
  `admitted` and `held`, exactly where a root reopened inside its linger stands (`admit.readmit`),
  so a look would resume the implementer and relaunch the Sandbox under the run. Apply the
  predicate at every site that starts, ends or closes a look and in the outbox's start row.
- **Seeing the same merge again opens nothing.** A done child a person parks and sets done again
  leaves twice more; `openCheck` keeps the check it has for the same pull request (repo, number,
  the merge where both are known), open or recorded, and only a rerun's other pull request
  replaces it.
- **A value from a bus subject any client may publish on is held to a shape where every source
  meets the sink.** `server.started`'s commit, the daemon's revision and Dispatch's `/healthz`
  commit all pass `compareBuilds`; a build outside `^[0-9a-f]{7,40}$` is refused there, logged by
  cause and length, before it reaches the implement App's GitHub compare URL (also
  `url.PathEscape`d) or the implementer's task text.

## Evidence

- LEGION-781 replaced the daemon's post-merge `production_check` phase, which held an issue's
  pod (3 CPU, 19 GiB) and its tree's slot for days waiting on a deploy and a sign-off, with
  `accepting` before the merge and the check after the close. Stage 4b at ab4eccee: the close at
  03:23:52Z opened the check with look 1 in the implementer's live session, the `pending` report
  at 03:25:18Z suspended it and the pod was gone at 03:25:35Z; the bound (`production_check_hours:
  0.25`) started look 2 in a fresh pod at 03:38:52Z, and its `pending` closed the check `not_run`
  (`dispatch://LEGION-781/artifact/stage4b-2059-ab4eccee-tgz`).
- The reviewer's round 1 found the "runs again" predicate `RoleFor(issue.Phase) != ""` at four
  sites, `claimFailed` ending a look before its out-of-workflow gate, and `openCheck` upserting
  over a recorded check; `TestAReopenedRootsCheckStartsNoLookWhileItRunsAgain`,
  `TestACheckStartOfAnIssueRunningAgainFinishesWithoutActing` and
  `TestADoneChildSetDoneAgainKeepsItsChecks` are each red with the fix disabled
  (`packages/daemon/internal/workflow/check_test.go`, `daemon/outbox_check_test.go`).
- The same review found `server.started`'s `commit` reaching `"/compare/"+merge+"..."+build`
  and the task text unvalidated, the listener taking a `source: dispatch` publish from any
  client (`packages/envoy/cmd/listener/api.go`); `TestServerStartedComparesOnlyTheBuiltFromRepositorysChecks`
  sends `main#../../x`, `b00f`, upper-case hex, 41 hex digits, `deadbeef?per_page=100` and a
  newline, each queuing no row. A well-formed forged start still queues one compare per open
  check per publish; that quota amplification is dispatch://LEGION-809, left out of this change.
