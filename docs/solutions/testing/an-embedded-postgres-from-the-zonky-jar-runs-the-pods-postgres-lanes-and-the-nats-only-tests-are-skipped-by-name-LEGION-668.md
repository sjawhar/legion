---
title: "An embedded Postgres from the zonky jar runs the pod's Postgres lanes, and the NATS-only tests are skipped by name"
category: testing
tags:
  - worker-pod
  - postgres
  - LEGION_TEST_PG_DSN
  - testcontainers
  - go-test
  - proof-environment
date: 2026-10-10
status: active
module: packages/daemon
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# An embedded Postgres from the zonky jar runs the pod's Postgres lanes, and the NATS-only tests are skipped by name

Extends docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md.

- A worker pod has no Docker and no Postgres, but it has `python3`, `curl` and the network: the
  zonky embedded-postgres jar on Maven Central
  (`io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/16.4.0/…-16.4.0.jar`) holds a
  `postgres-linux-x86_64.txz` that Python's `lzma` + `tarfile` extract (the image has no `xz`);
  `initdb -U postgres --auth=trust`, then `pg_ctl -o "-p <port> -k <dir>" start` with
  `LD_LIBRARY_PATH=<root>/lib`. With `LEGION_TEST_PG_DSN=postgres://postgres@127.0.0.1:<port>/postgres?sslmode=disable`
  the `admit`, `api`, `record`, `workflow`, `workspace`, `store` lanes and the non-NATS halves of
  `intake` and `daemon` run in the pod instead of only in CI. Two roles in one pod share the
  network namespace, so one role's Postgres is reachable by the next; start your own on another
  port rather than depend on it.
- The tests that still need Docker are NATS-backed, and their names do not all say so. The skip
  list that leaves `intake` and `workflow` green:
  `-skip 'Consume|Captured|RateLimited|BackwardMoveNoRow|LateEventOfAnEarlier'`; `internal/daemon`
  runs by `-run` (`ReviewBody|ReviewPermission|Permission|Outbox -skip ScratchDispatch`; the
  scratch Dispatch tests need the `vector` extension the embedded Postgres lacks).
- Two more pod facts: `internal/supervise` type-checks under `CGO_ENABLED=0` (its table tests fail
  on `go tool cgo` otherwise), and `internal/launcher`'s
  `TestAResumeIsToldWhetherItsWorkspaceWasRecreatedSinceItsSessionWasWritten` reads the pane's own
  `LEGION_WORKSPACE` in its no-workspace case: run it with `env -u LEGION_WORKSPACE`.
- `go vet -tags e2e ./...` and an overlay build download modules and rewrite `go.work.sum`; restore
  it before a commit.

## Evidence

LEGION-668's implementer and tester each ran the daemon's Postgres lanes this way (ports 54329 and
54330; `.legion/LEGION-668/implement.json` trickyParts[0], `test.json` localLanes), the tester's
container reaching the implementer's instance. CI's `Tests` run at each head confirmed the lanes
the pod skipped.
