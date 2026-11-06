---
title: "A cross-check of a search is written from the production loop, and says which side it bounds"
category: testing
tags:
  - cross-check
  - search
  - negative-control
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/leasetime
related_issues:
  - "sjawhar/legion#2705"
---

# A cross-check of a search is written from the production loop, and says which side it bounds

A clever search is checked by a stupid one. The check is worth nothing when it inherits the
clever search's assumptions, which is what happens when it is written from the model instead of
from the code the model predicts.

- Derive the second computation from the production code — the loop, the handler, the statement —
  and not from the implementation under test or from its documentation. Another language or
  another shape helps; reading the first implementation first does not.
- Say in the test which side the check bounds. A grid or a sample can only miss a worse case,
  never invent one, so it checks the search from below: no walk it finds may beat the search's
  answer. It is no evidence that the search's answer is reachable — a separate assertion, that
  the search's own answer is a real walk, carries that half.
- Search a small instance whole and sample the large ones, with the seed fixed in the test, and
  put the extreme cases in beside the samples.
- A tester verifying a derived figure owes the same thing: a pass written from the production
  loop, not a re-run of the implementer's.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454). Round 2's tester verdict rested on a search that
took each refused renewal to fail at one of the two ends of its client timeout — the same blind
spot as the round-2 model it was checking — so it confirmed a model that accepted leases which
lapse, and the review, not the tester, found the defect.

Round 3's tester wrote a forward pass in Python from `cmd/agent-secrets/main.go`'s `renewLoop` and
`internal/broker/helper/server.go`'s `renewLoop`, with each refused renewal's failure instant
continuous in `[0, 30 s]`. It reproduced both of the review's traced lapses to the digit (a 100 s
lease at a 10 s sweep renews at 103.333333332 s; a 355 s lease at a 60 s sweep at 358.333333332 s)
and agreed with the seven floors the binary prints, and 1.1 million sampled forward walks found
nothing later than that pass.

In tree, `TestNoOutageRenewsLaterThanWorstForFound`
(`packages/envoy/internal/broker/leasetime/leasetime_test.go`) is the permanent form: a one-second
sweep searched whole on a one-second grid over every refused renewal's failure instant, and ten
thousand seeded walks per lease at the longer sweeps, beside the two walks at the ends of the
range. Its comment states the direction — "A grid can only miss a worse instant, never invent one,
so it checks the pass from one side, the side that matters" — and the other side is carried by
`WorstFor` returning a real walk the pinned rows replay.
