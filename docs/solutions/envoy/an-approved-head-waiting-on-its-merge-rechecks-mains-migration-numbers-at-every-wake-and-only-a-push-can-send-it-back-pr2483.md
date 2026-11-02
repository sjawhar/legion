---
title: "An approved head waiting on its merge rechecks main's migration numbers at every wake, and only a push can send it back"
category: envoy
tags:
  - migration
  - merge-queue
  - legion-workflow
  - dispatch
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/store
related_issues:
  - "sjawhar/legion#2483"
---

# An approved head waiting on its merge rechecks main's migration numbers at every wake, and only a push can send it back

Extends docs/solutions/envoy/a-migration-number-is-checked-against-mains-highest-at-every-push-and-the-local-harness-database-is-recreated-after-a-renumber-INFRA-367.md.

- That note checks the number before every push and every review round. A head in
  `awaiting_merge` has neither, and two new migration files never conflict as text, so GitHub
  keeps it `MERGEABLE`. Whoever watches the merge (the architect, the deployment lane) compares
  main's highest migration with the branch's on every wake while the head waits.
- A backward move out of `awaiting_merge` is refused, so the remedy is the implementer's push:
  merge main forward, renumber and push. That push moves the head past READY, which withdraws
  READY and starts a full round. Expect it, and say in the round's report that the merge was
  pushed before the task existed.
- Prove a renumber-only round by diffing the two fingerprinted diffs line by line: every changed
  line must be the old number replaced by the new.

## Evidence

- sjawhar/legion#2483 was accepted with `0145_asks_anchor_artifact_id` while main a21100c5 added
  `0145_events_person_words`, `0146` and `0147`. The text merged clean and GitHub showed the pull
  request `MERGEABLE`, so nothing re-armed it. Earlier collisions (0137, 0139, 0141, 0142) each
  came with a README conflict and a `CONFLICTING` state; this one came with none.
- The workflow table builds backward rows only for planning through merging
  (`packages/daemon/internal/workflow/table.go:78`), and `Engine.backward` refuses any move no row
  serves with `BACKWARD_REFUSED` (`engine.go:756`). The push to `0148` moved the head, and
  `withdrawMovedReady` (`ready.go:226`) took the issue back to implementing.
- The fingerprint comparison for that round showed 21 changed lines, each `0145` → `0148`.
