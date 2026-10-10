---
title: "A predicate on a body's trailing footer traces every hop's cap first: Envoy cuts a review body at 2048 runes"
category: daemon
tags:
  - envoy-normalizer
  - body-truncated
  - legion-footer
  - review-attribution
  - intake
  - plan-review
date: 2026-10-10
status: active
module: packages/daemon/internal/intake
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A predicate on a body's trailing footer traces every hop's cap first: Envoy cuts a review body at 2048 runes

- Before a daemon predicate keys on text at the end of a body — a Legion footer, a trailer, a
  sentinel — walk the body's path from GitHub to the consumer and read every hop's cap. Envoy's
  normalizer caps the `body` of every comment and review event at 2048 runes
  (`packages/envoy/internal/contracts/normalize.go`, `capBody` and `addCappedBody`) and marks
  `body_truncated: "true"`; `legion_footer: "true"` says only that a footer existed in the
  uncapped body, never which one or what it carried.
- A positive match on the capped body (set aside unless the footer names X) silently rejects
  every long body the legitimate writer produces. Decode `body_truncated` onto the fact and
  restore the full body before the match: intake's `ConsumerSpec.ReviewBody` reads
  `GET /repos/{owner}/{repo}/pulls/{n}/reviews/{id}` for a truncated review-App review before the
  fact enters its transaction, and a body it cannot restore (no review id) stays set aside with
  `body_truncated=true` in the log line.
- A plan that designs such a predicate names the cap and the restore; a planner who did not read
  `packages/envoy` cannot see it from `packages/daemon` alone, so the implementer's first read of
  a footer predicate is the normalizer.

## Evidence

Plan D1 (dispatch://LEGION-668/artifact/plan-md) specified the reviewer-session match on the
review's footer without the cap. The three real `legion-reviewer[bot]` reviews on
sjawhar/legion-smoke#522 run 3597, 1858 and 3272 runes; the tester's run of `LegionSession()`
over them capped as Envoy caps them answered the session for the 1858-rune one and `""` for the
other two, whose footers start past rune 2048 (`.legion/LEGION-668/test.json`, proof[1]). The
restore is `packages/daemon/internal/intake/consume.go` (`resolveReviewBody`) and
`packages/daemon/internal/daemon/review_body.go`; the architect approved it as a correctness fix
inside D1 (Envoy e25917608591a8fc36f824d4ed809f3d).
