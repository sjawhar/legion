---
title: "Every copy of a ygo room is built by one constructor, with the remote-only format cleanup off"
category: envoy
tags:
  - dispatch
  - ygo
  - yjs
  - replay
  - format-cleanup
date: 2026-11-01
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2496"
---

# Every copy of a ygo room is built by one constructor, with the remote-only format cleanup off

- A copy that reproduces a room's state by applying its updates (the replica, a write's fork, a stored-history fold, a room load, a checker's oracle) must apply them as the room did. ygo's `ApplyUpdateV1` is a remote apply, and it runs a format cleanup the room never ran on its own local commits, so the copy can hold formatting the room does not.
- Build every such copy with `newDocumentCopy` (`packages/envoy/internal/dispatch/docs/tree.go`), which sets `crdt.WithFormatCleanup(false)`. Never call `crdt.New` directly for a document that replays the room's history: a copy built another way runs the cleanup again.
- Pin any new copy with a test that writes formatted text through the room's own commits (`Server.Apply`) and through a browser. The test asserts that the copy, or the fold of the stored updates, renders the room's markdown. It must also fail with the cleanup turned back on in `newDocumentCopy`.
- When a dependency bump changes how a remote apply behaves, read every copy's constructor before you merge it. A green suite at the new pin proves nothing unless some test applies updates the room's own local commits made.

## Evidence

- At ygo `v1.54.1-sami.1`, which a forward merge of main brought into #2496, a stored history no longer replayed. The room's local commits left format markers, and every replay through `ApplyUpdateV1` cleaned them up. Review round 4 measured 1243 runs out of 3000 where the replica's state differed from the room. main's #2604 (ygo `v1.54.1-sami.3`) added the option, and the option covered the replica, forks, folds and the stored-order checker at once, because all of them already went through `newDocumentCopy` (`storedorder.go:267`).
- `TestStoredOrderFormattedWritesReplayToEveryVersion` (`storedorder_format_test.go:21`, commit e99cd22c) passes at the fixed pin. With the cleanup on, it fails: "the last version holds `seed\n\n*xy*\n\n*xy*\n`, want the room's `seed\n\n*x*y\n\n*xy*\n`".
