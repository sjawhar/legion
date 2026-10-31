---
title: "A pod Postgres for Dispatch's Go tests is initialised with ICU collation, so the rank-order test runs instead of refusing"
category: testing
tags:
  - postgres
  - collation
  - legion-pod
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/api
---

# A pod Postgres for Dispatch's Go tests is initialised with ICU collation, so the rank-order test runs instead of refusing

- When you stand up Postgres in a pod for Dispatch's Go tests (the rootless recipe in
  `docs/solutions/testing/`, "The real Dispatch stack runs inside a Legion pod"), initialise it with
  `initdb -D <data> -U postgres --encoding=UTF8 --locale-provider=icu --icu-locale=en-US --locale=C.UTF-8`,
  not `--locale=C`.
- With `--locale=C` the default collation is byte order, and
  `TestIssueRanksStayOrderedPastLowercaseDigitsAndProductionShapedMoves`
  (`internal/dispatch/api/rank_test.go`) refuses to run (`select 'a' < 'B'` is false), on main as on
  any branch. CI's Postgres image runs it, so a pod run reported as "everything but that test" hides
  a real result.
- Check the database before relying on it: `psql … -Atc "select 'a' < 'B'"` must print `t`.

## Evidence

On sjawhar/legion#2397 the implementer's `--locale=C` database refused the test for several rounds.
A second Postgres in the same pod, initialised with ICU `en-US`, ran it to `--- PASS` on main
2df3353c and on the branch at a529e99b; the harness rebuilt with ICU later ran the whole `api`
package green, that test included.
