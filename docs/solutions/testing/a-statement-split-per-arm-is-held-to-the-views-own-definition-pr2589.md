---
title: "A statement split per arm is held to the view's own definition"
category: testing
tags:
  - postgres
  - views
  - equality-tests
  - migrations
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/refs
related_issues:
  - "sjawhar/legion#2589"
---

# A statement split per arm is held to the view's own definition

When you replace a read of a whole view with per-arm reads, the arms become a list your code carries and the database can outgrow.

- Commit a test that parses the view's own definition out of the database and fails when the view holds an arm the code does not declare.

- Put the remedy in the failure message: which table to declare the arm in, where to add it in the test, and where to fill it in the fixture.

- Make the runtime refuse a kind it has no declared arms for, rather than counting it from whatever arms it does have:
a kind counted from part of its arms is silently low.

## Evidence

In sjawhar/legion#2589 the backlink count unions the arms of `graph_edges` that can point at the kind it is asked for,
and `TestTheGraphViewGainsNoArmTheBacklinkCountDoesNotKnow`
(`packages/envoy/internal/dispatch/refs/backlink_counts_test.go:729`)
reads `select pg_get_viewdef('graph_edges'::regclass, true)`
(`:732-733`),
parses it back into one descriptor per arm
(`parseGraphViewArms`, `:807-859`)
and holds the result to the arm list the count knows
(`graphViewArms`, `:690-721`).
Its failure message names the remedy:
"Declare each new arm in backlinkArms (or, when its to_kind is a column, in mentionBacklinkArms), add it to graphViewArms in this test, and fill it in newBacklinkFixture."
(`:740-743`).

The runtime refuses a kind it has no arms for:
"A kind that is no key of the arm table is refused before any SQL is built: the count has no arms to read for it, and would otherwise be silently low",
failing with `count %s backlinks: no arms declared for that node kind`
(`packages/envoy/internal/dispatch/refs/backlink_counts.go:38-42`),
which `TestBacklinkCountsRefuseANodeKindWithNoDeclaredArms`
(`packages/envoy/internal/dispatch/refs/backlink_counts_test.go:560`)
holds for every kind the build resolves.

It is not a hypothetical.
The branch's per-arm count was written on 2026-10-31 (commit `6431f3084`),
and on 2026-11-02 main landed commit `b284275e2`, which restates the view with an ask's parts:
"each current part's part_of edge to its ask, each link a current part holds, and the split of 0142's comments.replies_to_block arm by the node id's prefix"
(`packages/envoy/internal/dispatch/store/migrations/20261101183525_graph_edges_ask_parts_and_block_pointers.up.sql:1-15`).
It reached the branch through a forward merge, and the pull request body records the merged head taking those arms:
the backlink pair over an issue's asks reads 2.276 s → 3.53 ms "now that the seed writes an ask's parts … and the view's three new arms hold rows",
and "the per-arm backlink count is held to main's whole statement over the restated view by the equality test, for every node kind and every exclusion".
