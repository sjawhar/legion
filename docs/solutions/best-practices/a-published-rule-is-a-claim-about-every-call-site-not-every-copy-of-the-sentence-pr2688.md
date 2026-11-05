---
title: "A published rule is a claim about every call site, not every copy of the sentence"
category: best-practices
tags:
  - census
  - documentation
  - published-rules
  - test-helpers
date: 2026-11-03
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2688"
---

# A published rule is a claim about every call site, not every copy of the sentence

Extends docs/solutions/best-practices/a-behaviour-fix-is-done-when-the-old-rules-sentence-greps-to-nothing-pr2546.md.

That note greps the old rule's sentence, so that no copy of the words survives the fix. The
mirror case is a change where every copy of the words is right and the rule is still false,
because a second implementation of the behaviour survives beside the new one.

- When you write down a rule about how something is done here, enumerate the call sites of that
  behaviour and classify each one in the change: through the single definition, exempt by design
  with the reason written at the site, or a refusal case. A sentence naming your new helper is a
  claim about all of them.
- A grep for the new helper's name cannot close that claim: it finds your call sites and says
  nothing about the older, looser one. Grep for the behaviour — the route, the query parameter,
  the table — and read every hit.
- Delete the second implementation in the same change, even when the diff has not touched it and
  its callers are somebody else's. "Pre-existing" is not a reason to publish a rule that is false
  at the head you publish it on. Where deleting it would change behaviour, say so in the rule
  instead of writing the rule as though it did not exist.
- Prove the claim by that classification, in the pull request body, and keep each site's own
  assertions when you convert it.

## Evidence

In sjawhar/legion#2688 the change added `awaitSinceRead`, wrote the rule into `docs/testing.md`
("Only a read held past that wait is read again … The helper is `awaitSinceRead`"), and said in
its body that the rule had one definition. Both were false at that head: `awaitGraphEdges`
(`packages/envoy/internal/dispatch/api/references_test.go`, untouched by the change) re-read every
wrong `?since=` answer for up to `eventFeedWait` however fast it came back, and its three callers
were all since reads. Two review passes before that had judged converting them behaviour-changing
and left them alone, which is how the rule came to be published over a second implementation.

The reviewer refused the round on exactly that (review round 1, head `dc05427a`, thread
`PRRT_kwDORFy7ds6y7JpU`). Commit `274fda86` deleted `awaitGraphEdges` and converted its three
callers (`references_test.go` twice, `live_events_horizon_test.go` once), each keeping its own
assertions. The closing proof was the classification, not a grep of the new name: every `?since=`
read of that route in the package is now taken through the helper, asserts zero edges by design,
or is the `400 INVALID_SINCE` case. Measured at that head, the tightening cost nothing: the five
tests that read `?since=` passed 200 of 200 under a transaction held 2 s of every 2.5 s and 200 of
200 unheld.
