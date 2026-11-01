---
title: "A raise-only setting treats the value meaning unlimited as the largest"
category: envoy
tags:
  - nats
  - jetstream
  - kv
  - stream-config
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2404"
---

# A raise-only setting treats the value meaning unlimited as the largest

## The rule

1. When a start raises a stream or bucket setting it finds lower than it needs, and never lowers
   one, check first whether the value it read means "no limit". JetStream writes "keep forever" as
   `MaxAge: 0`, and several limits use 0 or -1 for "none". A plain `found >= wanted` reads that as
   the smallest value, so the start lowers the setting an operator chose on purpose.
2. Write the comparison once, in one read-compare-update helper that every raise uses. Give it the
   "enough" test and the change as functions. The update must write back the configuration as
   read, with only that one field changed.
3. Test the no-limit case on a real server: a bucket created with the no-limit value must keep it
   after the raise.

## Evidence

- `packages/envoy/internal/bus/kv.go:77` (`RaiseMaxAge`): its "enough" test is now
  `config.MaxAge == 0 || config.MaxAge >= age`. Before round 5 of #2404 it was
  `config.MaxAge >= age`, which lowered an unlimited `envoy_listener` bucket to 20 minutes, against
  its own doc ("an operator's longer age stands").
- `kv.go:97` (`raiseBucketSetting`) is the one helper, shared with `raiseReplicas`.
- `internal/bus/kv_max_age_test.go:44` (`TestRaiseMaxAgeLeavesABucketWithNoAgeLimitAlone`) failed
  first: "the bucket keeps a value for 20m0s after the raise, want the unlimited age (0) the
  operator set".
