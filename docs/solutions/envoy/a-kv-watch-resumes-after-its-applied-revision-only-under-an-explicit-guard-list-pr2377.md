---
title: "A KV watch resumes after its applied revision only under an explicit guard list"
category: envoy
tags:
  - kv-watch
  - jetstream
  - cache
  - resume
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2377"
---

# A KV watch resumes after its applied revision only under an explicit guard list

When a cache that follows a JetStream KV bucket is rewatched (after a reconnect or a rebuild),
resume after the last revision it applied instead of replaying the bucket, but only when every
guard below holds. Otherwise read the bucket in full.

- **The process kept its revision.** A first start always reads in full: no applied revision
  survives the process.
- **The last generation completed.** It reached its end-of-scan marker without nats.go's idle
  timer. A scan that timed out, or one still running, is read again in full, so nothing depends on
  the order in which a scan delivers keys.
- **The stream is the same one.** Its creation time is unchanged; a recreated bucket resets the
  cache and is read in full.
- **The sequence space did not go backwards.** The applied revision is at or below the stream's
  `LastSeq`. A JetStream restore of an older snapshot keeps the creation time and reuses sequence
  numbers, which a resume would skip.
- **A full read starts the applied revision again.** Otherwise a revision above a restored stream's
  last sequence forces a full read at every later rewatch.
- **The cache was not emptied meanwhile.** Read the resume point before the new consumer exists,
  then check a reset counter again at the switch. If another watch emptied the cache for a recreated
  bucket in between, discard the resuming watcher and read in full.
- **Every store fences by revision.** The watcher being replaced can apply entries after the resume
  point was read, and the new one delivers them again. That is harmless only when applying an
  already-applied revision changes nothing. Check this in each store before you resume it.

A resume cannot see a delete marker purged, or a value `MaxAge` expired, while the watcher was away.
Neither can the same-bucket full `WatchAll` it replaces, which does no reset either, so a resume is
no blinder than the replay. Evicting keys a read did not see is a separate sweep.

## Evidence

- `packages/envoy/internal/kvwatch/kvwatch.go`: `resumeFrom` holds the guard list. `watch` resets
  `hw` on a full read, compares the `resets` count at the switch, and falls back to a full read.
- `packages/envoy/internal/kvwatch/resume_test.go` covers each guard: a rewatch with no writes
  applies 0 entries; a lost connection with 2 puts, 1 delete and 1 purge applies exactly those 4; a
  recreated bucket; a restored older snapshot (`LastSeq` below the applied revision); a first scan
  that timed out.
- Benchmarks in sjawhar/legion#2377: a rewatch of 25,000 CI records went from 3.2 s and 207 MB to
  about 5 ms and 117 KB. In the outage-load reproduction, a nats-server restart's warm-up went from
  reading every key to `resumed` with 0-8 entries.
