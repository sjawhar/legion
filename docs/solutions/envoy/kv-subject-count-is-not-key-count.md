---
title: "A JetStream KV's subject count is not its key count"
category: envoy
tags:
  - nats
  - jetstream
  - kv
  - startup-latency
  - measurement
date: 2026-09-28
status: active
module: envoy
related_issues:
  - "LEGION-360"
symptoms:
  - "nats stream info shows far more subjects than nats kv ls shows keys"
  - "sizing a startup cost from the bucket's message count"
  - "KV bucket grows without bound and looks like a retention leak"
  - "kv.Keys() returns a handful of keys on a bucket with hundreds of subjects"
---

# A JetStream KV's Subject Count Is Not Its Key Count

A limits-retention KV bucket keeps a **delete marker** on the subject of every key ever deleted,
and nothing expires them when the bucket has no `MaxAge`. `nats stream info KV_<bucket>` counts
those subjects; `nats kv ls` and `nats.KeyValue.Keys()` hide them. The two numbers diverge without
limit on a bucket whose keys are short-lived.

Production's `envoy_roles`, read on 2026-09-28:

```
$ nats --server nats://envoy-nats:4222 stream info KV_envoy_roles
             Messages: 767
   Number of Subjects: 767

$ nats --server nats://envoy-nats:4222 kv ls envoy_roles | wc -l
9
```

767 subjects, 9 keys. The other 758 are delete markers, the oldest from 2026-09-09. A `kv get` on
one answers `nats: key not found`.

## Why it matters

LEGION-360 was filed as "the bucket holds 766 role claims, so claims for finished trees are never
deleted — fix the retention". Both halves were wrong, and the number came from `stream info`.
Claim retention was working: all 9 live claims had a live holder in `envoy_sessions`, and every
claim whose tree finished had been deleted. What is left is the markers, which are intrinsic to
the bucket's retention, not a leak in the code that writes it.

The performance half was real for a different reason. `store.Open` read the bucket with
`Keys()` + one `Get` per key, so its cost scaled with the **key** count, which is small in
production. It would have scaled to 35 s only on a deployment that really held 766 live claims.

## Rules

- **Size a read from the count the read actually pays.** `Keys()` streams one metadata entry per
  *subject* (markers included) and returns one entry per *key*; a per-key `Get` pays one round
  trip per key. Say which one a number is before using it to size anything.
- **Confirm a retention hypothesis against the values, not the subject count.** One
  `WatchAll(nats.MetaOnly())` pass reports each entry's `Operation()` and `Created()`, which
  separates live keys from markers and dates the markers, in one round trip.
- **A marker cannot be swept away separately.** A KV `MaxAge` expires values as well as markers,
  so it is not a way to trim tombstones from a bucket whose claims must outlive it.
  `nats-server` 2.11's per-message TTL (`SubjectDeleteMarkerTTL`) is the only mechanism that
  targets markers alone, and it applies to buckets created with it.
- **Read the bucket in one pass anyway.** Both the marker count and the key count then stop
  mattering: `roleRevisions` (`packages/envoy/internal/store/kv.go`) takes one watch over existing
  keys, as `internal/kvwatch` does for the interest, session and CI caches.
