---
title: "Unsubscribes Must Update the Interest Cache"
category: envoy
tags:
  - envoy
  - routing
  - nats
  - go
  - cache-consistency
date: 2026-09-07
status: active
module: envoy
symptoms:
  - "an unsubscribed topic is delivered after the next heartbeat"
  - "registry and durable KV interest state disagree"
---

# Unsubscribes Must Update the Interest Cache

## Symptom

A session can unsubscribe from a topic and still receive matching notifications after its next
heartbeat re-registers the remaining subscriptions.

## Mechanism

`Registry.Upsert` merges heartbeat topics with the interest registry's in-memory cache before
writing the result to JetStream KV. If `Registry.Remove` only updates KV, its local cache keeps
the removed topic. The next heartbeat reads that stale cached interest and merges the removed
topic back into the durable entry.

A malformed value delivered by a KV watcher has the same routing risk: ignoring the decode error
leaves the last valid cached route active even though the durable value is no longer usable.

## Fix

After a successful partial removal, `Registry.Remove` writes the remaining interest through to
its cache. After removing every topic, it removes the cache entry. This keeps the cache and KV
consistent before the asynchronous watcher receives the mutation.

KV watchers evict a cache entry when decoding its value fails and emit a warning with the key and
revision. Matching then treats the route as absent rather than delivering using stale state.

Role claims use the same ordering principle: publish the new interest, atomically update the role
KV entry with its observed revision, then remove the old holder's interest. If the atomic role
write fails, remove the new interest as compensation. If old-holder cleanup fails after the role
write, return the error while retaining the authoritative new role holder.
