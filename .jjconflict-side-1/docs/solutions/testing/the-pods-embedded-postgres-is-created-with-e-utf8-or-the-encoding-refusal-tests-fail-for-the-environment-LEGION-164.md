---
title: "The pod's embedded Postgres is created with -E UTF8, or the encoding-refusal tests fail for the environment"
category: testing
tags:
  - worker-pod
  - postgres
  - initdb
  - UTF8
  - SQL_ASCII
  - LEGION_TEST_PG_DSN
date: 2026-10-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-164"
  - "sjawhar/legion#1932"
---

# The pod's embedded Postgres is created with -E UTF8, or the encoding-refusal tests fail for the environment

Extends docs/solutions/testing/an-embedded-postgres-from-the-zonky-jar-runs-the-pods-postgres-lanes-and-the-nats-only-tests-are-skipped-by-name-LEGION-668.md.

- Run the recipe's `initdb` as `initdb -D <data> -U postgres --auth=trust -E UTF8` (the earlier
  LEGION-631 note has the flag; the LEGION-668 recipe dropped it). In the pod `initdb` picks locale
  `C` and so encoding `SQL_ASCII`, which stores any byte sequence; a test that asserts the
  server's refusal of invalid UTF-8 (`internal/ompsessions`: `Import` of `{0xff, 0xfe}` and a NUL
  byte must fail with `invalid byte sequence` and write no row) then fails for the environment,
  and would pass for the wrong reason on a branch that broke the refusal.
- A lane that fails in the pod on an encoding error is checked against `initdb.log`'s
  `The default database encoding has accordingly been set to` line before it is reported as the
  branch's.

## Evidence

LEGION-164's implementer created its instance without `-E` and handed `internal/ompsessions`' two
UTF-8 cases to CI as an environment failure (`TestImportRefusesContentNoTextValueHolds`); the
tester's instance on another port was created `UTF8` and ran them green in the same pod.
