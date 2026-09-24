---
title: "A loaded devbox stretches every wall-clock budget in the envoy and pi-envoy suites: find what the budget covers"
category: testing
tags:
  - flaky-tests
  - load-testing
  - postgres
  - go-test
  - bun-test
  - storetest
date: 2026-09-24
status: active
module: packages/envoy/internal/dispatch/store/storetest
symptoms:
  - "`panic: test timed out after 10m0s` in `internal/dispatch/api` on a devbox, green on CI and on a rerun"
  - "`timed out waiting for mark updates persisted` in a docs compaction test"
  - "`durable document = \"…HUMAN WRITE\\n\", want \"keep\"` in `…TableAnchorCheck/unconditional_baseline`"
  - "`start NATS: … create container: … context deadline exceeded` at 30 s in natstail, or `start NATS JetStream: …` in daemon-go's intake, admit or workflow"
  - "pi-envoy `this test timed out after 5000ms` in a test that runs jj, then `# Unhandled error between tests` from a jj helper with an empty stderr (`jj config list failed: `)"
---

# A loaded devbox stretches every wall-clock budget: find what the budget covers

## Context

The devbox runs many agents at once. At 1-minute load 120-210 on 32 cores, CPU pressure `some` was
about 64% and IO pressure about 45%. A `jj git init` then took 1.0-1.6 s of wall time for 10 ms of
CPU, `docker create` took 1-6 s, and about 500 serial durable appends took up to 37 s. CI's 4-vCPU
runner sees none of this, so every failure below is green on CI and on a rerun.

Most failures were a budget that covered more than the thing it was meant to bound, and the fix
took that extra work out of the budget. The lock probe was different: it was missing a filter.
This change raises no test budget; #1264 replaced the compaction waits' 5 s with a one-minute
drain. The one timeout that changed is the documented dispatch checks recipe,
whose `-timeout 60s` could not fit `internal/dispatch/api` (71 s on CI) and now uses go test's
default.

## What each budget actually covered

- **go test's 10-minute package timeout covered `DROP DATABASE`.** `storetest` gives each test a
  database cloned from a migrated template. On Postgres 16, a clone (`WAL_LOG` strategy) forces no
  checkpoint. A `DROP DATABASE` forces an immediate checkpoint and waits for it: the server log
  gains one `checkpoint starting: immediate force wait` per drop. Every package testing against
  the server queues on its one checkpointer. Alone, `internal/dispatch/api` spent 202 of its
  305 s creating and dropping 364 databases, with 7.9 s of CPU. `storetest` now drops each
  database in the background after its test ends and `Main` waits for the drops. Drops in flight
  at the same time share a checkpoint. Side by side on one server, api went from 406.6 s to
  227.6 s and docs from 214.5 s to 109.7 s. The admin pool that carries the drops is capped at
  4 connections: its default is the CPU count, and the seven packages that call `storetest.Main`
  at 32 connections each exceed the server's `max_connections` of 100.
- **A 5 s wait covered about 500 durable appends.** The docs compaction tests append each browser
  mark in its own commit behind the room lock. A fixed 5 s for all of them is a throughput budget.
  `waitForPersistedUpdates` waits for the service's durable-append queue to drain, bounded by a
  minute, then checks the count. The minute is still a budget, about 8 appends a second for 500
  marks, but it is twelve times the old 5 s and above the 37 s the slowest loaded run took.
- **A lock probe counted another database's sessions.** `pg_stat_activity` is server-wide. A
  probe for "my edit is now waiting on the lock" must filter on `datname = current_database()`.
  Otherwise another test process's waiting session answers it early, and the race the test sets up
  never happens.
- **A test's 30 s context covered Docker's container create.** natstail's test and daemon-go's
  `testnats.JetStream` both did; under load a create took 17-43 s. Start the container without a
  deadline (`context.Background()` or `t.Context()`), as the other envoy packages and daemon-go's
  own `workflowNATS` do, and bound only the behavior under test or the readiness wait after it.
- **Nothing reaps a test container but the test.** The envoy-go recipe runs with
  `TESTCONTAINERS_RYUK_DISABLED=true`, so every shared container needs its own `TestMain`
  teardown, and a started container needs its cleanup registered before the start's error is
  checked (#1300).
- **Bun's 5 s per-test timeout covered `jj` processes.** At first it was six `jj git init` runs, one
  per role. The per-role tests now share one workspace. A test that checks jj config still runs
  four jj processes, `jj git init` among them. When bun times a test out it kills the test's
  dangling subprocess, so that process's stderr is empty, so the config helpers report
  `jj config list failed: ` or `jj config set failed: ` with nothing after it; the template's init
  names the exit code and signal instead. `legion.test.ts` runs one `jj git init` in `beforeAll` and copies that
  repository per workspace, so only the jj calls a test exists to check count against its
  budget. `.jj/repo/store/git_target` is relative, so a copy is a valid repository.

## Reproducing on purpose

The machine's own load reproduces these only some of the time. Force the stretched work instead,
and run the same forcing against the tree before and after the fix:

- a `doc_updates` trigger that runs `pg_sleep(0.02)` per insert (and, for the stall case, sleeps
  an hour once the count reaches a threshold);
- a decoy session in another database: `begin; lock table asks in access exclusive mode;
  select pg_sleep(600)` in one session and `select id from asks` in another;
- a unix-socket proxy in front of `/var/run/docker.sock` that holds `POST …/containers/create` for
  31 s, used through `DOCKER_HOST`;
- a `jj` earlier on `PATH` that runs `sleep 1` and then execs the real one;
- several test binaries of one package at once against one Postgres, for the package timeout.

Record `/proc/loadavg` with every run. A comparison is fair only when main and the fix run at the
same moment on the same server.

## Related

- `await-the-event-not-a-tick-budget.md`: the same rule for the TypeScript daemon's tests.
- `widen-the-contender-count-before-calling-a-race-unreproducible.md`: the load-recipe discipline.
