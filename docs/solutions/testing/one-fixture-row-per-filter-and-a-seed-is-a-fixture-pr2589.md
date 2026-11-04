---
title: "One fixture row per filter, and a seed is a fixture"
category: testing
tags:
  - fixtures
  - equality-tests
  - postgres
  - benchmarks
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/refs
related_issues:
  - "sjawhar/legion#2589"
---

# One fixture row per filter, and a seed is a fixture

An equality or plan test proves a filter only where its fixture holds a row at the filtered position.

- Write one row per filter the statement carries, at the position that filter removes,
and one row the filter keeps, so the two answers differ when the filter goes.

- Show each filter's removal failing the test before you call the test a proof of it.

- Fail the run when the fixture fills an arm with nothing,
rather than letting an empty arm pass as an agreement.

- Treat a measurement seed as a fixture too.
An arm whose rows the seed never writes is measured at zero,
and a seeded row that carries less state than a written one makes a route measurement meaningless,
so a measurement of a write route creates its own subject through the route's own API.

## Evidence

In sjawhar/legion#2589 the backlink count was split into one arm per kind, and its equality test is held to a fixture written row by row
(`TestBacklinkCountsReadArmByArmAgreeWithTheView`, `packages/envoy/internal/dispatch/refs/backlink_counts_test.go:498`).

Each item kind holds a block a revision removed, which keeps its row with no position and keeps its reference:
a comment's `b3` (`packages/envoy/internal/dispatch/refs/backlink_counts_test.go:183-199`),
a message's `b2` (`:219-233`),
and an ask's part `b3` (`:268-290`).
The citing comment then mentions both a current and a removed block of each item kind
(`:299-321`), so an edge the view counts under a block can be seen to be picked up by the item the block has left.

Before those rows existed, three filters could each be deleted with the equality test still green:
the item-block `position is not null`
(`packages/envoy/internal/dispatch/refs/backlink_counts.go:141` and `:313`),
the two `ask/%` prefix tests that tell an ask's part from a comment's block
(`packages/envoy/internal/dispatch/refs/backlink_counts.go:262` and `:265`),
and the `ask part_of` arm's own filter
(`packages/envoy/internal/dispatch/refs/backlink_counts.go:225`).
The pull request body records the repair:
"the equality fixture now writes an edge at a removed block of an ask, a comment and a message, and counts each block kind over the other item's block,
so the item-block position filter, the two `ask/%` prefix tests and the `ask part_of` arm's own filter each fail the test when removed".

The emptiness guard is in the helper that picks the targets:
a kind the view points at with no row fails with "no arm of the view points at a %s node: the fixture leaves that kind's arms empty"
(`packages/envoy/internal/dispatch/refs/backlink_counts_test.go:465`).

The seed half is the same rule on the measurement database.
The backlink pair over an issue's asks only became a real reading once the seed wrote an ask's parts:
the body gives 2.276 s → 3.53 ms "now that the seed writes an ask's parts (226,667 current parts and 50,000 `ask_block_refs` rows per database) and the view's three new arms hold rows".
A seeded document carries no editor state either —
"the seed writes `artifact_versions`, the block index and the reference rows, and it does not build the ygo document state that a version written through the API carries",
so "a write-route measurement therefore creates its own document through the API on the seeded database and writes over that"
(`packages/envoy/cmd/dispatch/README.md:516-523`).

It stands beside
docs/solutions/testing/a-control-that-could-not-have-come-out-the-other-way-is-not-a-control-pr2675.md
and
docs/solutions/testing/a-test-whose-instrument-is-the-mechanism-you-delete-goes-vacuously-green-pr2674.md.
