---
title: "A teardown's cancellation boundary is a range of the shared sequence, and the lock, not the read, holds a gate ordered by that sequence"
category: database-issues
tags:
  - postgres
  - advisory-lock
  - commit-order
  - teardown
  - outbox
date: 2026-11-04
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2642"
---

# A teardown's cancellation boundary is a range of the shared sequence, and the lock, not the read, holds a gate ordered by that sequence

- **A teardown that must be cancelled by work arriving beside it records a range of the shared
  sequence, not a point.** Write the first row, the boundary and the last row the teardown itself
  queued, and let a row *outside* that range cancel: below it was in flight when the mark was
  taken, above it was queued since. A single `id < boundary` test can never fire, because every
  row queued later has a higher id than the boundary by construction.
- **A gate read inside a transaction proves nothing when your own write takes its position from a
  sequence another committer also writes.** Take the lock every writer of that lifecycle takes,
  before the read, and say in the comment that the lock and not the read is what holds the gate —
  otherwise a reader sees the status from before the other transaction and still draws its id from
  the sequence after it, which is the one order the gate exists to refuse.
- **A second fact the consumer can check locally is worth more than a tighter central test.** Where
  the central record says "this artifact has been handed out", let the consumer also check that the
  resource in front of it is the one the artifact was made for. The two are independent, and each
  catches what the other cannot see.

## Evidence

In sjawhar/legion#2642 the daemon releases an idle issue pod and cancels that release when a wake
arrives beside it.

- The first version compared `id < boundary_row` and could never cancel: the reviewer showed that
  every row queued after the mark has an id above the boundary (`store/issue_release.go:59` at that
  head). The settled shape is the range `first_row`, `boundary_row`, `last_row`, `started_row` and
  `wake_row` across three migrations (`20261101222956_issue_idle_release.up.sql`,
  `20261102153000_issue_release_rows.up.sql`, `20261102183000_issue_release_wake.up.sql`), with a row outside the range cancelling.
- `CancelIssueRelease` shipped without `treelifecycle.Serialize`, which its three sibling writers
  all take. A park or close is one fact transaction that moves the issue's status and queues its
  rows together, so an unserialized cancel could read the status from before it and take its
  resume's id from after it: a resume standing above a close, whose suspension then finished
  without acting and left the issue's Sandbox with its launcher tokens and `gh` credentials. The
  test holds the park's transaction open to drive that window and fails ten runs of ten without the
  lock.
- The local second fact is `restoreWorkspace` refusing when `workspace.Created(dir)` is not after
  the bundle's own `At` (`launcher/restore.go`): the daemon cannot see which workspace a launcher
  stands in, and the launcher cannot see the store's attempt record.
