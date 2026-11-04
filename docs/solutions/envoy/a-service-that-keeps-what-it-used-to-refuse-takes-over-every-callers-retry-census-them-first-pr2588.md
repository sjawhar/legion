---
title: "A service that keeps what it used to refuse takes over every caller's retry: census them first"
category: envoy
tags:
  - rollout
  - release-order
  - http-status
  - contract-change
date: 2026-11-01
status: active
module: packages/envoy/cmd/listener
---

# A service that keeps what it used to refuse takes over every caller's retry: census them first

Extends docs/solutions/envoy/before-an-old-consumer-reads-a-new-answer-as-a-generic-error-check-its-retry-cap-and-what-waits-behind-it-pr2370.md,
which is the same change in the other direction: a refusal that is new, rather than a refusal that
becomes an acceptance.

- When a service starts keeping what it used to refuse, every caller that owned a retry loop loses
  it silently. The new answer is not an error, so the loop stops and the caller believes the
  message was taken.
- List every caller before you write the code, with what it does with the answer, and decide per
  caller: the service takes the message over, or the caller opts out and keeps its own retry.
- Ground each opt-out in that caller's code, by line. An opt-out justified by a belief about how a
  caller behaves is how a wrong reason spreads: once written in the comment it is copied into the
  contract, the test's doc, the census and the pull request, and every copy reads as evidence.
- Where the two sides ship as separate releases, the order is a constraint no test in the
  repository can hold. Record it at the send site, in the changelog fragment and in the risk list.

## Evidence

Envoy's listener answered `404` for a role nobody held; it now keeps the message and answers
`202 kept` (sjawhar/legion#2588). Two callers showed both halves of the trap:

- A Legion phase worker's stall reminder (`packages/pi-shared/src/phase-reminder.ts`) read any
  non-error as "the architect was told", set `reported = true` and stopped reminding. Its retry
  *was* the reminding, so the kept copy would have waited out the hour and been dropped with
  nobody told. It now publishes `keep: false` and keeps its 404 retry.
- Dispatch's outbox was opted out on the reason that it "redelivers a role-routed event from its
  own receipts". The reviewer disproved it from four call sites: `publishRoute` is called once per
  event, the event is marked published as soon as NATS accepts it, and nothing in that module
  consumes the `no_holder` exception. By then the false reason had been copied into the code
  comment, the envelope contract, the listener's own doc, the test's doc, the census row and the
  pull request's Outcome. The decision was reversed and those events are kept like any other.

The release order was measured rather than assumed: the previous `legion` release reads `202` as
neither success nor `ErrNoHolder`, so its outbox row keeps retrying while Envoy also hands the
message to the role's next holder. The binary that sends `keep: false` ships first, and that order
is in the changelog fragment and in the pull request's risk list.

## Applies to

Any "the service will handle it for you" change: store-and-forward, server-side retry, queueing a
write that used to be refused, an idempotent accept in place of a conflict. The failure is
invisible to every test that exercises only the new happy path, because the caller that stopped
retrying is in another package.
