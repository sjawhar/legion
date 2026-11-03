---
title: "Two log lines order an event only when each one's side of the commit is named"
category: testing
tags:
  - log-assertions
  - transaction
  - daemon-runtime
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2727"
---

# Two log lines order an event only when each one's side of the commit is named

Extends docs/solutions/testing/a-status-endpoint-names-a-new-leader-before-that-leader-logs-it-so-the-log-is-a-condition-of-the-wait-pr2639.md,
which covers a log line that trails an endpoint. Here the lateness has a named cause in the tree,
which is what makes the order evidence rather than timing.

- **In the Legion daemon a line written through `intake.OnCommit` appears only after its
  transaction commits; a line written inline appears inside it.** So the relative order of two
  such lines carries a fact that neither the final state nor either line alone carries: which side
  of the commit the work fell on.
- **Say which side each line is on, in the test, next to the assertion.** Without that sentence
  the ordering assertion is folklore, and the next reader deletes it or inverts it.
- **Let the state assertion carry the claim and the log carry only the order.** This does not
  contradict
  docs/solutions/testing/measure-the-mechanism-not-the-symptom-count-the-work-the-system-does-not-do-INFRA-228.md,
  which says a log line is a proxy that drifts: the proxy here is anchored to a code fact rather
  than to a duration, and the thing it proves — the order — is not available from the state at
  all, which is read separately in the same test.
- **An absence of a line within a window is the strongest half.** "No line of this kind stands
  between these two" is the assertion a reordering defeats, where "the right line exists" passes
  either way.

## Evidence

On sjawhar/legion#2727 the proof of the admission fix had to show that no slot was taken *before*
the hold's release, on the real daemon runtime. `a.logSlot` defers through `intake.OnCommit`
(`packages/daemon/internal/admit/admit.go:1053-1054`) while `release` logs its own line inline
inside its transaction (`:1515`), so the roots a release admits are always logged after the
release line. `TestAHeldRootKeepsItsPlaceOnTheRuntimeUntilItsOwnReleaseAdmitsIt`
(`packages/daemon/internal/daemon/hold_release_test.go`) scans the captured log once, records that
reasoning in the comment beside the ordering check, and reads the slots table with `tickSlotted`
separately. Its three operator-visible lines are the ones the production check of this issue reads
from `kubectl logs`:

```
msg="admission reconcile: holding roots for the Dispatch consumer to reach a stream position" count=1 target=1000 ack_floor=0
msg="admission: the Dispatch consumer has caught up; releasing held roots" count=1 ack_floor=0 idle=true admitted=1
msg="admission: slot taken" issue=LEGION-HELD slots=1 cap=1
```

The assertion that fails on the pre-fix code is the absence one — no `admission: slot taken`
between the first line and the second. With the bug, that line is there and names the lower-ranked
root.

A log sink a test shares with a daemon goroutine must be mutex-guarded, or CI's `-race` lane on
the package reports a data race; the package's own `syncBuffer`
(`packages/daemon/internal/daemon/controller_watch_test.go`) is that sink, and this test reuses it
rather than adding a third copy.
