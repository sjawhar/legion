---
title: "A route two limiters can refuse is tested on the field that names which one"
category: testing
tags:
  - rate-limit
  - http
  - assertions
date: 2026-11-04
status: active
module: packages/envoy/cmd/broker
related_issues:
  - "sjawhar/legion#2705"
---

# A route two limiters can refuse is tested on the field that names which one

When a route is held to more than one bucket — a per-caller one and a fleet-wide one — both
refuse with the same status and the same body, and a row that counts acceptances cannot tell you
which bucket it measured. Measure the wrong one and the row goes green at any sizing.

- Find the one observable that distinguishes them and assert its exact value. A retry hint derived
  from the rate works when the two rates are a fixed factor apart by construction, so the hint
  names the bucket.
- Fail on any other value, with a message saying what a different value means: that another bucket
  refused, and that every assertion after it is measuring that bucket. A row that tolerates the
  other value is measuring whichever one happened to run out first.
- Where no field distinguishes them, add one or drive the buckets apart in the fixture; do not
  infer the bucket from a count.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454). The broker's reread route is held to a per-address
bucket and a shared fleet-wide bucket, both answering `429` with the same body.
`cmd/broker/multiprocess_test.go`'s `rereadCaller` records the reason: "the `Retry-After` is the
one thing in a refusal that says WHICH of the two reread buckets refused it: the shared bucket's
rate and the per-address bucket's are always a factor of two apart (`api.RereadLimitFor`), so the
header names the bucket."

The fleet rows assert that header exactly and fail otherwise: "refused the reread after its burst
with `Retry-After: %d`; want %d, the %s of the shared bucket a %d-pod fleet derives — another
figure is another bucket refusing, and the window below would then measure that one." With that in
place the row can say what it means to say: at three pods the refusal carries `Retry-After: 3`,
and once the third pod stops it falls back to `Retry-After: 1`.
