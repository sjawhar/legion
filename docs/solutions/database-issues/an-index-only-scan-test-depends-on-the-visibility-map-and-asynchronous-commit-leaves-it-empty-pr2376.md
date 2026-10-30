---
title: "An index-only scan test depends on the visibility map, and asynchronous commit leaves it empty"
category: database-issues
tags:
  - query-plans
  - index-only-scan
  - visibility-map
  - test-database
date: 2026-10-30
status: active
module: packages/envoy
---

# An index-only scan test depends on the visibility map, and asynchronous commit leaves it empty

- A plan test that asserts `Index Only Scan` also asserts that `VACUUM` marked the table's pages
  all-visible. Postgres costs an index-only scan by the share of all-visible pages, so with none it
  picks a bitmap heap scan on the same index, and the test fails as if the index or the query were
  wrong.
- Run such tests on a database with `synchronous_commit = on`, the server default. With
  `synchronous_commit = off` (or `fsync = off`) on the server, a `VACUUM` right after the seed can
  set no page all-visible: `pg_class.relallvisible` stays 0.
- When the plan comes out as a bitmap heap scan of the index you expected, read
  `select relallvisible, relpages from pg_class where relname = '<table>'` after the test's
  `vacuum analyze` before you change the index or the query. 0 means the database, not the code.
- A test Postgres you start for speed (a cluster Job, a container) gets its speed from
  `shared_buffers` and `max_connections`, never from turning off durable commit.

## Evidence

`packages/envoy/internal/dispatch/sessionanswers/sessionanswers_test.go` seeds 4,000 message
deliveries, runs `vacuum analyze message_deliveries` (line 39), and requires every read of the table
to be an `Index Only Scan` of `message_deliveries_session_created` (line 97). On 2026-10-29 it
failed on a pgvector Postgres 18.6 started with `-c fsync=off -c synchronous_commit=off`: both the
custom and the generic plan were a Bitmap Heap Scan over a Bitmap Index Scan of that index. The same
seed and `vacuum` on that server left `relallvisible 0` of 34 pages. With
`set synchronous_commit = on` in the session, the same `vacuum` set 34 of 34. With the server
restarted without the two settings, the test passed unchanged.

Why the pages stay unmarked is an inference, not measured here: under asynchronous commit the
seeding transaction's commit may not be flushed when `VACUUM` reaches its pages, so `VACUUM` cannot
yet mark them all-visible.
