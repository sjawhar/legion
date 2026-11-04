---
title: "A once-per-item side effect is a closure the claim's winner runs"
category: architecture-patterns
tags:
  - metrics
  - logging
  - queue
  - idempotency
date: 2026-11-04
status: active
module: packages/envoy/cmd/listener
related_issues:
  - "sjawhar/legion#2588"
---

# A once-per-item side effect is a closure the claim's winner runs

Applies to any drain several readers may walk: a queue, a lane, a stored subject, a table of
pending rows. The item is claimed by a write that one reader wins; everything the design says
happens "once per item" has to hang off that write.

- **Decide the item and build its side effects, but do not run them.** Have the judgement return
  the effects as a closure — counter, log line, exception published, notice sent — with the
  decision it made.
- **Run the closure only after the write that claims or finishes the item has succeeded.** A
  reader that loses that write reports nothing, because another reader is reporting it.
- This is what makes the numbers true under the drain's own recovery: a reader that dies between
  its claim and its confirm leaves the item for the next walk by design, and any effect that ran
  before the write is then counted, logged or published again on every re-walk.
- The same holds for an item nothing will be forwarded for — one no read can decode, one past its
  window, one a filter refuses. It is finished with a cursor write too, so its effects hang off
  that write, not off the judgement.
- Ask it of every effect, not only of metrics. A log line a human counts, a notice another agent
  acts on and an exception that pages someone are all "once per item" claims.

## Evidence

sjawhar/legion#2588's listener drains a role's kept lane, and several listeners of a deployment
may walk one lane. Review round 1 found the first shape counting a copy past the keep window into
`envoy_role_kept_expired_total` before the cursor write that finished it, so a lost or failed
cursor write had the next walk read the same copy and count it again — the metric reported more
expiries than there were copies, and the same fix closed the duplicate log line beside it.

The settled shape is `judgeKeptRoleCopy` (`cmd/listener/role_keep.go:830`), which returns
`(item, report func(), forwardable bool)` and runs nothing itself: the undecodable copy's `Error`
line and the expired copy's `roleKeptExpired.Inc()` with its `Warn` line are both inside the
returned closure. The walk writes the cursor first and calls `report()` only when that write
landed (`:744-752`), so a walk that lost the cursor to another listener reports nothing. A copy
that will be forwarded takes the same order one step further: claim write, forward, confirm write
(`:755` onward), so the forward is what the claim protects and the confirm is what ends it.
