---
title: "Recreate template1 under the ICU provider and the pod's Postgres runs the collation test too"
category: testing
tags:
  - worker-pod
  - dispatch-server
  - postgres
  - collation
  - icu
  - proof-environment
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-56"
  - "sjawhar/legion#2158"
---

# Recreate template1 under the ICU provider and the pod's Postgres runs the collation test too

Extends docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md.

That note stops at reading the collation refusal as the environment and pointing at CI. It is
fixable in the pod, in one statement, and then `internal/dispatch/api` is green there whole.

- The refusal is a property of `template1`, not of the database `DISPATCH_TEST_DATABASE_URL`
  names: `storetest.prepare` creates its migrated template with a bare `create database <name>`,
  which clones `template1`, and every test's own database is a clone of that. An `initdb
  --locale=C` cluster — the only locales an issue pod has are `C`, `C.utf8` and `POSIX` — makes
  every one of them byte-ordered.
- The image ships ICU (`libicu76` is already one of the five packages the rootless rig unpacks),
  so Postgres 17 can give `template1` a collation that is not byte order without any OS locale:

  ```sql
  update pg_database set datistemplate = false where datname = 'template1';
  drop database template1;
  create database template1 with template = template0 encoding = 'UTF8'
    locale_provider = icu icu_locale = 'en-US' lc_collate = 'C' lc_ctype = 'C' is_template = true;
  ```

  Confirm it with `select 'a' < 'B'` in a fresh database: `t` is the ordering the test needs,
  `f` is byte order.
- Do this before the run, not after a red: every test database already created keeps the old
  collation.

Related: `docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md`
is the rig this fixes, and its step 2 `initdb --locale=C` is where the byte order comes from.

## Evidence (INFRA-56, sjawhar/legion#2158)

The rig was rebuilt from the INFRA-42 recipe in the issue pod — five Debian trixie packages under
`/legion/scratch/infra56/root`, `initdb --locale=C`, PostgreSQL 17.11 on port 5433 with
`create extension vector`. The first whole-package run was
`go test ./internal/dispatch/api/ -count=1 -timeout 55m` → `FAIL … 835.185s`, its only failure
`TestIssueRanksStayOrderedPastLowercaseDigitsAndProductionShapedMoves`:
`this test's database already orders "a" after "B" (byte order: select 'a' < 'B' is false), so it
cannot tell migration 0049's collate "C" apart from no fix at all`. After the three statements
above, `select 'a' < 'B'` in a fresh database returned `t` and the same command returned
`ok github.com/sjawhar/envoy/internal/dispatch/api 828.915s`, and again `851.152s` at the final
head. The tester inherited that same `template1` — an issue pod's containers share a network
namespace, so one Postgres serves every role of the issue — and ran its own rounds against it.
