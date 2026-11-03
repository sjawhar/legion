---
title: "Expiry on a deadline timer keeps every read judging the deadline, and drops a revision fence only a TTL after the key left"
category: envoy
tags:
  - cache
  - expiry
  - deadline-heap
  - locking
  - revision-fence
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2377"
---

# Expiry on a deadline timer keeps every read judging the deadline, and drops a revision fence only a TTL after the key left

When you replace prune-on-read (every read takes the write lock and walks the map) with a deadline
heap and one timer, so that reads take only the shared lock:

- Keep every read judging the entry's deadline itself. The timer is an optimisation and may not
  have fired yet, so a past-deadline entry still in the map reads as absent.
- Make a derived answer the same before and after the removal. A read of an expired entry that is
  not yet removed computes the very record the removal will write (one helper both call), so the
  answer does not jump at the timer.
- Let a timer item act only on the entry it was armed for: compare the entry's current deadline
  with the item's. A write that replaced the entry moved the deadline, and then nothing happens.
- Let a retention deadline (the last-seen record and the revision fence it guards) only move
  later, never earlier.
- Drop a key's revision fence only once at least one TTL has passed since the key left the map, and
  refuse to insert an already-expired put for a key the map does not hold. Together they make a late
  delivery of a revision older than the dropped fence read as expired, so it cannot bring the key
  back. Without the fence drop, the fence map grows by one entry for every key ever seen.
- Keep at most one heap item per (key, kind), updated in place with `heap.Fix` through an index map.
  Use `time.AfterFunc`, which runs no goroutine and so needs nothing tied to the watcher's
  lifecycle. A struct-literal instance should work with no heap until its first schedule.
- A gauge that only counts takes the shared lock and counts in place; it does not copy and sort the
  whole list.

## Evidence

- `packages/envoy/internal/session/expiry.go`: `scheduleLocked`, `setExpiryLocked` (a last-seen
  deadline only moves later), `armExpiryLocked` and `expireDue` (acts only when the cached entry's
  `expiresAt` equals the item's deadline; drops the fence only for a key not cached).
- `packages/envoy/internal/session/registry.go`: `Get`, `List`, `LastSeen` and `LiveCount` take
  `RLock` and change nothing. `LastSeen` answers an expired entry from `lastSeenRecord`, the record
  removal writes. `retention()` is never below the TTL, and `cacheSessionLocked` does not insert an
  already-expired put for an uncached key.
- `packages/envoy/internal/session/expiry_test.go` covers concurrent reads beside writes and the
  timer under `-race`, a write landing between a deadline and its callback, the fence leaving with
  its last-seen record, a stale replay staying absent, and the fence map staying bounded.
- Go benchmarks in sjawhar/legion#2377: `Registry/Get` at 3,000 sessions went from 122 µs to
  0.2 µs. The interest reaper's pass, which calls `Get` once per interest, went from 304 ms to 54 µs.
