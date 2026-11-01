---
title: "A retrying sender multiplies every log line an alarm counts, so count a delivery's first failure and prefix the repeats"
category: envoy
tags:
  - alarms
  - metric-filter
  - webhook
  - redelivery
date: 2026-10-29
status: active
module: packages/envoy
---

# A retrying sender multiplies every log line an alarm counts, so count a delivery's first failure and prefix the repeats

- Before you make something retry a path that logs, find what counts those lines
  (`packages/envoy/infra/aws/rendering/metricfilters.go`). A 5-minute alarm with `OkActions` flips
  ALARM/OK once per spaced retry, so a delivery retried hourly for three days pages about 74 times.
- Remember a delivery's first failure in the process, and log every later failure of the same
  delivery behind a prefix the filter does not take (`redelivery <delivery>: <same line>`). Keep
  the text, so the operator still sees every attempt; one helper writes both lines, so they cannot
  drift. Keep the memory process-local and bounded (entries capped, aged out at the retry window),
  so a new task or a restart pages once again.
- The listener's filters are positional, space-delimited patterns: a prefix that moves the source
  word out of third place is what stops the count. Check each pattern by its word positions.
- Never cut a key to fit a bound: two values sharing the kept bytes would share one key, and the
  second's first failure would be logged as the first's repeat, which no alarm counts. Refuse to
  remember a value past the bound (fail toward the counted line). Where a value must fit a bound
  and stay distinct (a NATS subject), cut it and append a hash of the whole value, with a separator
  the value can never contain (`~`, which git refuses in a ref).

## Evidence

- sjawhar/legion#2478 made Dispatch's redelivery sweep retry any failed delivery for 72 hours
  (about 76 requests). Review finding 3 showed the `webhook-refused` and `publish-failure` alarms
  would flip about 74 times per refused delivery. Fixed in b7a9efb5 and 19a25586:
  `packages/envoy/internal/webhook/github_failures.go` (first failure per `X-GitHub-Delivery`, 4,096
  entries, 72 h, a key over 64 bytes or empty never remembered), `failureLine` in `webhook.go`.
  Live: one GUID posted twice and another once logged counted, `redelivery …`, counted.
- The push subject cut: `packages/contracts/src/subject.ts` and its Go twin from `gen-go.ts` keep
  what fits of the ref and end it in `~` plus 16 hex of the ref's SHA-256.
