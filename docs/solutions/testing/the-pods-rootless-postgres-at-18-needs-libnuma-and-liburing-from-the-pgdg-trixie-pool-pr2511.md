---
title: "The pod's rootless Postgres at 18 needs libnuma and liburing, from the PGDG trixie pool"
category: testing
tags:
  - legion-pod
  - postgres
  - pgvector
  - harness
  - io-uring
date: 2026-10-29
status: active
module: packages/dispatch/e2e
---

# The pod's rootless Postgres at 18 needs libnuma and liburing, from the PGDG trixie pool

Extends docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md.

That document's package list stands up PostgreSQL **17** rootless, and names `libicu76` as the
non-obvious one because the image ships no ICU and the `postgres` binary links it. Production and
CI run **18**
(docs/solutions/testing/a-throwaway-postgres-for-a-lexeme-check-runs-productions-major-because-16-and-18-stem-english-differently-pr2476.md),
so a harness built from that list is a major behind what it is meant to stand in for. Two things
change at 18.

- **Two more libraries.** The 18.6 `postgres` binary links `libnuma.so.1` and `liburing.so.2` for
  its io_uring asynchronous I/O. Unpack `libnuma1` and `liburing2` from the Debian trixie pool
  beside `libicu76`. A missing one fails the same way ICU does: a loader error naming neither
  Postgres nor the library's purpose. `ldd` on the unpacked binary, grepped for `not found`,
  answers which are still missing before `initdb` is ever run.
- **Take 18 from the PGDG trixie pool, not Debian's.** Debian's own `postgresql-18` is a sid
  build whose dependencies are `libc6 (>= 2.43)` and `libicu78`; the worker image is trixie with
  glibc 2.41. The `…pgdg13+1` builds of `postgresql-18`, `postgresql-client-18`, `libpq5` and
  `postgresql-18-pgvector` run on it unchanged.

Keep the rest of the extended recipe: `initdb` with the ICU provider
(docs/solutions/testing/a-pod-postgres-for-dispatch-tests-is-initialised-with-icu-collation-so-the-rank-order-test-runs-pr2397.md),
the `pg_isready` guard on the start rather than `pg_ctl status`
(docs/solutions/testing/the-shared-harness-script-starts-postgres-only-when-its-port-is-silent-and-no-spec-changes-while-a-run-loads-INFRA-304.md),
and everything on the issue volume.

One step none of those documents names: build `packages/contracts/dist` before a Playwright run.
The specs import `@legion/contracts`, and Playwright's Node resolution takes the package's
`default` export condition — its built `dist/` — where Bun takes the TypeScript source. Without it
the run dies at `Cannot find module '…/@legion/contracts/dist/index.js'` before any row executes.
CI has the same step in the dispatch job.

## Evidence

sjawhar/legion#2511. The harness built for the change ran PostgreSQL 18.6 with pgvector 0.8.7
rootless in an issue pod, under `/legion/harness`, and served every browser proof of three
implementing rounds. Seven packages were unpacked: `postgresql-18`, `postgresql-client-18`,
`libpq5` and `postgresql-18-pgvector` at `18.6-3.pgdg13+1` and `0.8.7-1.pgdg13+1` from PGDG, and
`libicu76`, `libnuma1` and `liburing2` from Debian trixie. Before the last two, `ldd` on the
`postgres` binary reported `libnuma.so.1` and `liburing.so.2` not found; with all seven it
reported none. The database answered `180006|t` for
`select current_setting('server_version_num'), 'a' < 'B'`, and `create extension vector` carried.
