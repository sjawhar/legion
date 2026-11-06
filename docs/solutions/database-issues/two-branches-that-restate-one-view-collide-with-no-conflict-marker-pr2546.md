---
title: "Two branches that restate one view collide with no conflict marker, and the later timestamp wins"
category: database-issues
tags:
  - postgres
  - migrations
  - view
  - forward-merge
  - graph_edges
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/store/migrations
related_issues:
  - "sjawhar/legion#2546"
---

# Two branches that restate one view collide with no conflict marker, and the later timestamp wins

A migration that restates a whole database object — a view, a function, an enum's values — is a
write to a single-writer resource shared by every open branch, not a file a text merge can join.
Two branches each add their own file, both files merge cleanly, and at runtime only the one that
sorts last exists.

- Before you add a migration that restates such an object, search the base **and** every open pull
  request for another migration restating it. Timestamped versions make this likelier, not safer:
  they sort by when the file was written, so the branch that merges first can still sort first.
- At a forward merge, derive the object from the merged migration **order**, never from the merged
  text. Whichever file sorts last is the whole definition; the other branch's arms are gone, and
  nothing in the diff says so.
- Repair it with one more migration that states the object once from both arm lists, with a header
  naming the two it reconciles. Do not renumber or edit either original: a database that already
  ran one of them would not run it again.
- Apply the repair in both orders — the order a deployed database reached and the order a fresh
  one takes — and compare `pg_get_viewdef('<object>')` between them. Equal definitions are the
  proof; a passing migration run is not.
- Keep one end-state test per arm, which migrates to head and asserts that arm's rows. That test
  is the only thing that detects a silent replacement later.

## Evidence

`20261101143211_ask_blocks` (this pull request) and `20261101144855_block_pointer_links`
(sjawhar/legion#2394) each restated `graph_edges` from `0142`'s arms. Both merged with no conflict
marker in any file. Because timestamps sort `…144855` after `…143211`, main's view replaced every
ask arm this branch had added, and `?to=<ask part>` would have answered nothing on a database that
ran both.

The repair is
`packages/envoy/internal/dispatch/store/migrations/20261101183525_graph_edges_ask_parts_and_block_pointers.up.sql`,
whose header states the collision and which holds every arm of both: #2394's
`where to_types is null or block_ref_listed(to_types, to_id)` filter beside this branch's
`ask_block` `part_of` and link arms and its split of `0142`'s `replies_to` arm by the `ask/`
prefix. It creates nothing and rewrites no row.

The view has been restated this way six times (`0037`, `0038`, `0094`, `0121`, `0142`, and these),
so the collision was available at each of them.

`packages/envoy/internal/dispatch/store/graph_edges_test.go:186` and `:282` migrate to head and
assert their own arms; they are what would have caught the loss had the merge shipped.
