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
  - "LEGION-374"
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

- **Size a read from the count the read actually pays.** `Keys()` streams one header-only message
  per *subject* (markers included) and returns one entry per *key*; a per-key `Get` pays one round
  trip per key. Say which one a number is before using it to size anything.
- **Confirm a retention hypothesis against the values, not the subject count.** One
  `WatchAll(nats.MetaOnly())` pass reports each entry's `Operation()` and `Created()`, which
  separates live keys from markers and dates the markers, in one streamed pass rather than a read
  per subject.
- **Markers can be swept alone, and `MaxAge` is not how.** A KV `MaxAge` expires values as well as
  markers, so it is not a way to trim tombstones from a bucket whose claims must outlive it.
  `KeyValue.PurgeDeletes` is: it watches the bucket, then purges the subject of each delete or
  purge marker and leaves live keys untouched (nats.go v1.50.0 `kv.go:802-872`), and
  `nats kv compact <bucket>` calls it. It keeps markers newer than 30 minutes unless
  `DeleteMarkersOlderThan` says otherwise, and for a marker past that threshold it purges the
  whole subject, so a key re-created between the watch and the purge goes with it. Measured on a
  scratch bucket of 3 live keys and 20 markers: `subjects=23 msgs=23 live keys=3` before,
  `subjects=3 msgs=3 live keys=3` after `PurgeDeletes(DeleteMarkersOlderThan(-1))`, every live
  claim still at its original revision. `SubjectDeleteMarkerTTL` (nats.go `jsm.go:253`) is a
  stream setting for the markers the server itself adds, not a per-message TTL, and it applies to
  streams created with it.
- **Read the bucket in one pass anyway.** The key count then stops mattering and the marker count
  costs bandwidth rather than round trips: `roleRevisions`
  (`packages/envoy/internal/store/kv.go`) takes one watch over existing keys, as
  `internal/kvwatch` does for the interest, session and CI caches. Timed on loopback against that
  function, best of five: 9 claims 0.87 ms, 9 claims and 1,000 markers 4.9 ms, 9 claims and 5,000
  markers 17.2 ms. Linear and cheap, but not free, and markers accrue with nothing expiring them —
  which is what `PurgeDeletes` is for.
- **A scan can end early, and one of the two ways looks like success.** nats.go sends a nil entry
  when it has delivered every existing key, and its idle timer sends the *same* nil when nothing
  arrived within the JetStream `MaxWait`, reporting the timeout only on `Error()`
  (`kv.go:1145-1156`). A reader that treats the nil as "done" returns a silently short snapshot on
  a stalled link that then recovers. Check `Error()` at the marker, and treat a closed updates
  channel with no marker (`kv.go:1170`) as the other early end. `nats.KeyValue.Keys()` has the
  same timer, so listing keys and reading each one back has the bug too — it is not specific to
  keeping the watch.
- **For `envoy_interests`, the markers are now collected, and that is the standing answer.** Every
  listener runs one pass after its interest cache's first warm-up and then every five minutes
  (`Registry.CollectInterestMarkers`, `packages/envoy/internal/store/interest_markers.go`): it
  takes the lowest revision a MetaOnly scan of the bucket delivers as a PUT and purges the stream
  below it, which removes every marker under the live keys in one request. That floor comes from
  the stream rather than the cache, and `PurgeDeletes` above is deliberately not what runs: it
  sets no sequence bound, replays the bucket with values, and purges a whole subject, so a key
  re-created between its watch and its purge goes with the marker (LEGION-360 declined that race).
  The pass adds one NATS capability, publish on `$JS.API.STREAM.PURGE.KV_envoy_interests`, read
  off a real server's trace rather than off a call site
  (`TestThePurgeSendsTheStreamPurgeSubjectItsGrantMustAllow`); a user without it leaves the markers
  and keeps the listener serving. `envoy_roles` is left alone: its scan costs 0.1-0.28 s, its live
  claims are not heartbeat-renewed, so a floor purge would reach few of its markers, and
  `envoy_sessions` has a 5 m `MaxAge` and needs nothing. `packages/envoy/AGENTS.md` (Operational
  notes) states the floor rule, the refusals and the one window that cannot be guarded.
