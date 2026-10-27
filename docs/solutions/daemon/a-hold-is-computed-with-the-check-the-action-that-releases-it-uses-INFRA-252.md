---
title: "A hold is computed with the check the action that releases it uses"
category: daemon
tags:
  - admission
  - hold
  - release_children
  - live-tree
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-252"
  - "sjawhar/legion#2226"
---

# A hold is computed with the check the action that releases it uses

- When the daemon holds an item until some action takes it (a root kept out of the waiting line
  until `release_children` adopts it), compute the hold from the same walk and the same refusal
  check that action uses, as one shared function, never a copy. Every held item must be one the
  action would accept; an item it would refuse must not be held, or nothing ever frees it.
- When the hold filters a set that another predicate reads (the waiting line, which defines which
  trees are live), split out the unfiltered set and derive the predicate from that one set for
  every reader, the hold included. Filtering the set in place gives two definitions of the same
  word, and an item under a held root falls between them.
- List each way the hold can end (the release, the target leaving, a person moving something on
  the chain) and give each a test; a hold with an exit nobody wrote is a stranded item.

## Evidence

On sjawhar/legion#2226 each of these was a review finding before it was fixed:

- The hold first stopped at the first recorded ancestor while `release_children` walks the whole
  Dispatch chain (`api/architect.go` `releasableChild`), so a root under a parked root took a slot
  the release could still have reached. The hold now stores the whole chain (`chainOf`, read in
  `daemon/repositories.go` `withAncestry`, line 770) and `record.Waiting` holds while any live root
  is on it (`record/project.go:48`).
- The hold first ignored the release's refusal of a root that ran as a tree of its own, so such a
  root was held and every release refused it. `workflow/adopt.go` now gates both on one function,
  `RanAsRoot` (line 61), read by `AdoptionRefusal` (line 25) and by `Ancestry` (line 92).
- `Waiting` first computed liveness before dropping held roots while `TreeLive` computed it after,
  so a todo child of a held root took its own slot. `line` (line 66) is now the unfiltered set and
  `LiveRoots` (line 107) the one live-tree definition `Waiting`, `TreeLive` and the sweep all read.
