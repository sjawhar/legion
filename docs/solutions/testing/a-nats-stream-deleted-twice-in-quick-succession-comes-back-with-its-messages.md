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
  - "error creating store for stream"
root_cause: race_condition
resolution_type: test_fix
severity: medium
---

# A NATS Stream Deleted Twice in Quick Succession Comes Back With Its Messages

## Problem

The `internal/session` tests shared one NATS container, and each test deleted the `envoy_sessions`
bucket and let `OpenSessionRegistry` create it again. On 2026-09-30 CI's
`TestSessionList_CacheOnlyAfterNATSShutdown` listed `ses_wt`, a key
`TestSessionPut_WriteThroughVisibleImmediately` wrote two tests earlier, and passed on a re-run. The
warm-up lines of that run show the "fresh" buckets holding other tests' keys and delete markers: by
log order, `TestSessionDelete_WriteThroughRemovesImmediately`, which writes one key and deletes it,
logged `entries=1 delete_markers=2`.

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

The window is usually milliseconds, but it can last until the server restarts. When the background
`RemoveAll` fails, the goroutine retries it for one second and then gives up (`filestore.go`
8764-8781), and `.<stream>` directories are cleared only when the server starts (`jetstream.go`
1223-1226). Until then, every delete of that name fails its rename the same way.

No watcher carries anything between tests. Each registry's watcher applies only to its own cache,
and a finished test's watcher ended when its connection closed; the new test's own watcher faithfully
mirrors a bucket the server had filled with the old stream's messages.

`error creating store for stream` is a second race in the same cleanup, and one delete is enough
for it. After `fileStore.Delete` returns, `stream.stop` removes the account's `streams` directory
and then the account's directory from another goroutine (`server/stream.go` 5286-5293 at v2.10.29,
unchanged through v2.15.0). `os.Remove` only removes an empty directory, so the goroutine removes
anything only when the deleted stream was the account's last and `.<stream>` is already gone. A
create that arrives before it runs can lose `streams` between `os.MkdirAll` finding it and making
`streams/<stream>` inside it. The server logs `Stream create failed for '<account> > <stream>':
could not create storage directory - mkdir .../streams/<stream>: no such file or directory` and
answers `error creating store for stream`. It hit `internal/store` on 2026-09-23, while its tests
reset one shared account by deleting both of its buckets, and `internal/kvwatch`'s
`TestAWatchOfTheOldStreamDoesNotReplaceANewerOne` on 2026-10-03, which deletes and recreates the
only bucket on a server of its own. On a loaded devbox it did not reproduce at the server's own
timing: no failure in 6,600 creates, nor in 556 runs of that test. A v2.10.29 built from the
module cache with a random delay of up to 2 ms before that goroutine's `os.Remove`, and up to 1 ms
inside the create's `os.MkdirAll`, failed 312 of 1,000 creates; with one more stream kept in the
account it failed none of 1,000.

## Reproducing the double delete without load

Plant the state the background removal leaves: a non-empty
`<store>/jetstream/<account>/streams/.KV_<bucket>` directory. With it in place, create the bucket,
put a key, delete the bucket (success; the stream directory is still on disk), create it again, and
the key is there. Without it, the recreated bucket is empty. Against a host `nats-server -js -sd
<store>` built from the module cache, the three session tests then fail five runs out of five with
CI's message. Loading the machine only widens the window.

On a devbox the `go` on `PATH` is a mise shim that sets `GOBIN` to the shared toolchain's `bin`, so
`GOBIN=/tmp/x go install github.com/nats-io/nats-server/v2@v2.10.29` puts the binary where every
session using that toolchain finds it. Call the real binary instead, which `mise which go` prints
and which honours `GOBIN`:
`GOBIN=/tmp/x "$(mise which go)" install github.com/nats-io/nats-server/v2@v2.10.29`.

## Fix

Never delete a stream name in a place a later test creates it again. `testnats.URL(t)` hands each
test the shared server as the user of a JetStream account no earlier test used, and nats-server keeps
each account's streams in a directory of its own (`<store>/jetstream/<account>/streams`), so one
test's deletes cannot reach another's creates under the same name. An account is never handed out
twice. The race is unchanged inside one account, so each subtest gets an account of its own too.
Every package whose tests share a server takes it through `testnats.URL`: the session, interest and
CI registries, the listener, `internal/bus`, `internal/kvwatch`, `internal/dispatch/redeliver` and
`internal/integration`. A delete a test makes of its own stream is the first of that name in its
account, so it renames as it should.

That keeps another test's deletes away, not a test's own. A test that deletes a bucket and creates
it again in the same account meets the single-delete race whenever that bucket was the account's
only stream, and no API says when the server's cleanup goroutine has run, so it creates the bucket
again with `testnats.RecreateKeyValue`. That retries the create on exactly `error creating store
for stream` and fails the test on any other answer.

The Legion daemon's `packages/daemon/internal/testnats` keeps one account and empties it instead:
its `URL(t)` deletes every stream on the shared server before handing it to a test, so the reset's
last delete always empties the account's `streams` directory, and the test's first create meets the
single-delete race. The daemon's tests create their streams with that package's
`testnats.CreateStream`, which retries on exactly the same answer. It is a copy, not a shared
helper: each module's `testnats` is an `internal` package the other module cannot import, and the
two speak different clients (nats.go's `JetStreamContext` in Envoy, its `jetstream` package in the
daemon) whose `APIError`s are different types. Against the widened v2.10.29 above, 1,000 resets each
followed by a bare create failed 73 creates, every one `error creating store for stream`; through
`CreateStream`'s retry none of 1,000 failed, 89 of them passing on a later attempt. On the stock
`nats:2.10` the daemon's helper runs, the bare create failed none of 2,000.

One test still deletes a name twice, and the double-delete race cannot fail it:
`internal/kvwatch`'s `TestARewatchOntoABucketRestoredWithAnOlderStreamRefillsTheCache` deletes
`kvwatch-test` twice on a server of its own and restores a stream snapshot right after the second
delete. The restore removes any directory left at the stream's name before it moves the snapshot in
(`server/stream.go` 5880-5883 at v2.10.29), so what a failed rename leaves never reaches the
restored bucket. On a `nats:2.10` container with a non-empty `.KV_kvwatch-test` planted, both
deletes failed their rename (the live bucket's warm-up read the first bucket's key too, which the
test does not check) and the test passed. The same test creating the bucket again in place of the
restore failed with the live bucket's key still in it (`live-key=true`).

## In production

No Envoy or Legion daemon code deletes a stream: a listener's rebuild opens a bucket and never
creates one, and the only creates are a start's `bus.EnsureKeyValue` of a bucket that is missing.
The single-delete race cannot reach them either, since the deployed account always holds
`ENVOY_NOTIFICATIONS` beside its buckets, so deleting one bucket never empties its `streams`
directory. An operator who deletes a bucket under running listeners reaches the double-delete race
by deleting it twice in quick succession. #1620's deep review drove that against a single v2.10.29
server with the listener's own calls (a replicated bucket was not tried): about 20 s after the
delete the session watcher stops,
registrations fail and lookups answer from the last cache; the self-health check cannot reopen a
missing bucket, so the listener restarts itself after three failed 30 s rebuilds
(`cmd/listener/main.go` 890-911) and the restart creates the bucket again. With a pending removal in
place, a resurrected session came back into a running listener's cache after the rebuild and stayed
until its original write time plus the bucket's TTL (`expiryFor` uses the entry's `Created`,
`internal/session/registry.go` 200-206), five minutes at the listener's default. An operator wiping
the session bucket twice in a row can therefore have the wipe undone for up to one TTL.
