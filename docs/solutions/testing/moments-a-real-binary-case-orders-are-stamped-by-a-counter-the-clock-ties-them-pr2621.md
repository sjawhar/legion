---
title: "Moments a real-binary case orders are stamped by a counter; a millisecond clock ties them"
category: testing
tags:
  - bun-test
  - flake
  - call-order
  - real-binary
  - pi-legion
date: 2026-11-01
status: active
module: pi-legion
related_issues:
  - "sjawhar/legion#2621"
---

# Moments a real-binary case orders are stamped by a counter; a millisecond clock ties them

## The smell

A case that drives the real Oh My Pi against a stand-in gateway records when each thing happened
with `Date.now()` and asserts the order between two of them:

```ts
const releasedAt = subagent.reply([{ type: "text", text: SUBAGENT_REPORT }]);
expect(read.arrivedAt).toBeLessThan(releasedAt);
```

`read.arrivedAt` is set in the stand-in's request handler; `releasedAt` a few statements later.
Nothing waits between them but the host's own work, which takes well under a millisecond, so both
stamps are the same number and `toBeLessThan` fails on an order that is right.
`legion-background-subagent-omp.test.ts` failed that way in the `pi-legion` job of
[#2391](https://github.com/sjawhar/legion/pull/2391) — `Expected: < 1793505745827, Received:
1793505745827` — and `pi-legion` is an input of the required `ci-ok` check, so the tie takes any
pull request red. Repeated at two cores, the case failed 6 times in 200 runs.

## The pattern

One counter, in the process that observes every moment, and one stamp that carries both readings:

```ts
interface Moment {
  readonly at: number;    // the clock, for durations and bounds
  readonly order: number; // the counter, for orderings
}

let moments = 0;
function moment(): Moment {
  moments += 1;
  return { at: Date.now(), order: moments };
}
```

Every observer mints through it — the stand-in's handler, its reply, the publisher, the frame
reader — so `order` reflects the order the process saw them and two moments can never share one.
Orderings assert `order`; a duration or a bound keeps `at`:

```ts
expect(subagent.arrived.order).toBeLessThan(ended.order);
expect(read.arrived.at - published.at).toBeLessThan(MESSAGE_BOUND_MS);
```

200 runs at two cores: 6 failures before, 0 after.

## What not to do

- Do not relax the tie to `toBeLessThanOrEqual`. Equal stamps are not an order the case means to
  allow; they are the clock failing to resolve one. The relaxed check also passes on the inverted
  order whenever that inversion is fast.
- Do not add a sleep to separate the moments. It hides the ordering the case is about behind a
  wait the product does not do.
- Do not reach for a higher-resolution clock (`performance.now`, `Bun.nanoseconds`) as the fix.
  It makes a tie rarer rather than impossible, and a counter is simpler than either.

## Related

- [prove-call-order-with-a-recorded-sequence-not-microtask-counting](prove-call-order-with-a-recorded-sequence-not-microtask-counting.md)
  — the same rule for a mocked collaborator: one recorded sequence, never a clock or a tick count.
- [socket-tests-observe-the-peer-not-the-clock](socket-tests-observe-the-peer-not-the-clock.md)
