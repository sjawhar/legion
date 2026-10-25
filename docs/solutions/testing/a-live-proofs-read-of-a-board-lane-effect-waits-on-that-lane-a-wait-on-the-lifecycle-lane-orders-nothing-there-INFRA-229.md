---
title: "A live proof's read of a board-lane effect waits on that lane: a wait on the lifecycle lane orders nothing there"
category: testing
tags:
  - live-proof
  - outbox
  - testwait
  - flake
  - daemon-tests
date: 2026-10-25
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "INFRA-229"
  - "sjawhar/legion#2180"
---

# A live proof's read of a board-lane effect waits on that lane: a wait on the lifecycle lane orders nothing there

Extends docs/solutions/testing/a-checkpoint-compares-live-state-to-a-comparand-read-at-the-assertion-and-waits-only-for-what-the-current-producer-can-deliver-LEGION-578.md.

- The daemon's outbox runs an issue's rows on two lanes side by side (`record/outbox.go`,
  `laneKinds`). The lifecycle lane runs `supervise`, `issue_suspend`, `linger_close`,
  `workspace_remove`, `gate_seed`, `issue_branch`, `auto_merge_disable`, `ready_merge` and the
  `check_*` rows. The board lane
  runs `dispatch_status`, `dispatch_message`, `notice`, `controller_notice` and
  `merge_queue_publish`.
- Read every board-lane effect (a Dispatch status write, a message, a notice) through its own
  `testwait.Eventually`, even after waiting for a lifecycle effect of the same fact (a suspension,
  or the projected phase). The waits before it say nothing about the board lane.
- A claim that something did not happen on the board lane, or happened exactly N times, is held
  over a settle window and is never read once.

## Evidence

- `merged_root_waits_live_test.go` step 4 waited for the root's architect to be suspended (the
  lifecycle lane), then read `r.board.wrote(r.parent, "done")` once. The reviewer flagged that a
  correct daemon could fail the step on timing. The fix wraps that read in
  `testwait.Eventually`, as step 6 already did for `retro`.
- Step 5 holds "the restart sends no second notice" by polling for 3 s (`mergedRootWaitsSettle`)
  rather than reading once.
