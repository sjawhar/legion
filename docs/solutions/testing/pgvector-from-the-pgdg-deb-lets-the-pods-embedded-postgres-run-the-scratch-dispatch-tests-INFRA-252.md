---
title: "pgvector from the PGDG deb lets the pod's embedded Postgres run the scratch-Dispatch tests"
category: testing
tags:
  - worker-pod
  - postgres
  - pgvector
  - scratch-dispatch
  - go-test
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-252"
  - "sjawhar/legion#2226"
---

# pgvector from the PGDG deb lets the pod's embedded Postgres run the scratch-Dispatch tests

Extends docs/solutions/testing/an-embedded-postgres-from-the-zonky-jar-runs-the-pods-postgres-lanes-and-the-nats-only-tests-are-skipped-by-name-LEGION-668.md.

- The `*AgainstScratchDispatch` tests in `internal/daemon` need no skip in the pod: they fail only
  because Dispatch's migration 0072 runs `create extension vector`. Unpack
  `postgresql-16-pgvector_<version>-1.pgdg22.04+1_amd64.deb` from
  `https://apt.postgresql.org/pub/repos/apt/pool/main/p/pgvector/` with Python (the `.deb` is an
  `ar` archive; `data.tar.xz` opens with `lzma` and `tarfile`). Copy `vector.so` into the zonky
  tree's `lib/postgresql/` and every file under `.../extension/` into
  `share/postgresql/extension/`. No restart is needed; `create extension vector` then succeeds.
- Those tests create and drop their own databases through the `postgres` admin database, so give
  them a DSN whose host the embedded server answers; point `LEGION_TEST_NATS_URL` and
  `LEGION_TEST_NATS_MONITOR_URL` at a local `nats-server -js`.
- The extension lives in the zonky tree, which a pod replacement keeps when it is under `$TMPDIR`,
  but the data directory may come back with other permissions: `chmod 0700` it before `pg_ctl
  start`, or re-run `initdb` if it is gone, and unpack the extension again if `lib/postgresql`
  lost it.

## Evidence

On sjawhar/legion#2226 main's `TestBlockedIssuesAreNotStartedAgainstScratchDispatch` was red in CI
only. Without pgvector it failed in the pod at Dispatch's start (`migration 0072_embeddings_core.up.sql:
ERROR: extension "vector" is not available`). With the deb unpacked it reproduced CI's step-10
failure, a probe with the adoption sweep disabled turned it green, and the fix was proved there
before the push.
