---
title: "A merge where both sides moved a count to the same value is silent, so recompute every count the two changes add to"
category: legion
tags:
  - forward-merge
  - constants
  - connection-pool
  - prose
date: 2026-11-02
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A merge where both sides moved a count to the same value is silent, so recompute every count the two changes add to

Extends docs/solutions/legion/a-fingerprint-can-go-backwards-and-a-file-both-sides-deleted-from-needs-a-symbol-grep-pr2541.md.

- When the branch and main each add to the same total, they often write the same new number. The
  merge then takes that number with no conflict, and the total is wrong by one side's addition.
- After a merge, list each count the branch changed (a pool total, a pod's connection limit, a
  sentence in a guide that states the number, a changelog line). Recompute each from its parts at
  the merge head. A test that compares the constant with the code's sum catches the constant, but
  nothing checks the sentence.
- Grep each symbol the branch removed through main's new files too. Main can carry removed code
  into a file the branch never touched. The merge brings it in cleanly, and it runs again.

## Evidence

- sjawhar/legion#2444's compaction pool raised Dispatch's connections per process from 90 to 92.
  Main's #2395 then raised them from 90 to 92 for its outbox pool and listener.
  `infra/gcp/dispatch.go` merged at 92 with no conflict, though the real total was 94.
  `TestDispatchConnectionsPerPodIsTheServers`, which compares the constant with
  `store.ConnectionsPerProcess`, fails on that 92. `running-dispatch.md`'s
  "That is 92 connections" and the changelog fragment's "to 92" had no test, and ea04bced and
  ed2dd500 corrected them by hand.
- #2395 added `outbox/lease.go`, whose `Outbox.Run` started `compactDaily`, the daily compaction
  #2444 removes. That line merged cleanly, and ea04bced removed it together with the conflicting
  fields in `publisher.go`.
