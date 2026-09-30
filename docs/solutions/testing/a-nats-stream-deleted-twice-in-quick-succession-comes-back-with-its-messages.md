---
title: "A NATS stream deleted twice in quick succession comes back with its messages"
category: testing
tags:
  - nats
  - jetstream
  - kv
  - flaky-test
  - go
date: 2026-09-30
status: active
module: envoy
problem_type: test_failure
component: testing
symptoms:
  - "A test on a shared NATS server lists a key that a test two tests earlier wrote"
  - "A cache warm-up on a bucket a test just deleted and created again reports entries and delete_markers it never wrote"
  - "TestSessionList_CacheOnlyAfterNATSShutdown: expected ses_cache from cache after shutdown, got [... ses_wt ...]"
root_cause: race_condition
resolution_type: test_fix
severity: medium
---

# A NATS Stream Deleted Twice in Quick Succession Comes Back With Its Messages

## Problem

The `internal/session` tests share one NATS container, and each test used to delete the
`envoy_sessions` bucket and let `OpenSessionRegistry` create it again. On 2026-09-30 CI's
`TestSessionList_CacheOnlyAfterNATSShutdown` listed `ses_wt`, a key
`TestSessionPut_WriteThroughVisibleImmediately` wrote two tests earlier, and passed on a re-run.
The warm-up lines of that run show the "fresh" buckets holding other tests' keys and delete
markers: by log order, `TestSessionDelete_WriteThroughRemovesImmediately`, which writes one key and
deletes it, logged `entries=1 delete_markers=2`.

## Cause

nats-server does not always delete a file-store stream when it says it has. At v2.10.29
(`server/filestore.go` `fileStore.Delete`, lines 8753-8781) a delete removes the stream's meta file,
renames the stream's directory to `.<stream>` beside it, and removes that directory from a
background goroutine that waits on the server's disk-IO semaphore. When a second delete of the same
name comes before that goroutine has run, `.<stream>` still exists, the rename fails, and
`fileStore.Delete` returns the error to `stream.stop`, which ignores it (`server/stream.go`
5279-5283, "Ignore errors"). The API answers success and the account lists no stream, but the
stream's message blocks stay in its directory, and the next create of that name recovers them
(`newFileStoreWithCreated`, `recoverFullState` / `recoverMsgs`, lines 443-458). v2.15.0's `Delete`
renames the same way.

No watcher carries anything between tests. Each registry's watcher applies only to its own cache,
and a finished test's watcher ended when its connection closed; the new test's own watcher faithfully
mirrors a bucket the server had filled with the old stream's messages.

## Reproducing it without load

Plant the state the background removal leaves: a non-empty `<store>/jetstream/$G/streams/.KV_<bucket>`
directory. With it in place, create the bucket, put a key, delete the bucket (success; the stream
directory is still on disk), create it again, and the key is there. Without it, the recreated bucket
is empty. Against a host `nats-server -js -sd <store>` built from the module cache, the three session
tests then fail five runs out of five with CI's message. Loading the machine only widens the window.

## Fix

Never delete and recreate a stream name on a shared server. The session tests open their
registries on a bucket named for the test (`setupNATS(t).OpenRegistry`, `envoy_sessions_<n>`), as
`internal/store` (`testBuckets`) and `internal/cistore` (`testBucket`) already did for the other
symptom of the same race ("error creating store for stream"). `setupNATS`'s cleanup fails a test
that opened the production-named bucket on the shared server, so a test that bypasses the helper
fails on its first run rather than flaking.

## Where the same race still applies

- `cmd/listener/main_test.go` `resetListenerTestState` deletes `session.SessionBucket` before each
  test, and its tests open the registry on that name.
- `internal/testnats` `URL` deletes every stream on the shared server before each test, and its
  callers (`internal/bus`, `internal/kvwatch`, `internal/redeliver`, `internal/integration`) create the
  same stream names again in the next test.
