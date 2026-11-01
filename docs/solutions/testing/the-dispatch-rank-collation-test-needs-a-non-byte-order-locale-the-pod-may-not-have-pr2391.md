---
title: "The Dispatch rank-collation test needs a non-byte-order locale the pod may not have"
category: testing
tags:
  - postgres
  - collation
  - locale
  - environment-failure
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# The Dispatch rank-collation test needs a non-byte-order locale the pod may not have

Extends docs/solutions/testing/the-pods-embedded-postgres-is-created-with-e-utf8-or-the-encoding-refusal-tests-fail-for-the-environment-LEGION-164.md.

- A Dispatch test cluster needs two settings, and each one fails different tests. With encoding
  `SQL_ASCII`, api's `TestEventPayloadsAreServedAsStoredForEveryEventKind` fails with
  `unsupported Unicode escape sequence`, and store's
  `TestScannedTimestampsCarryUTCWhateverTheHostZone` fails with `client_encoding=UTF8`. With a
  byte-order collation, `TestIssueRanksStayOrderedPastLowercaseDigitsAndProductionShapedMoves`
  refuses to run (`api/rank_test.go`: `select 'a' < 'B'` must be true).
- `-E UTF8 --locale=C.UTF-8` fixes the encoding and still gives byte order. Before you blame a
  branch for the rank test, run `locale -a` and `select 'a' < 'B'`. When the pod has only `C`,
  `C.utf8` and `POSIX`, and the server has no ICU, the rank test cannot pass in that pod. Report it
  as the environment's, name it, and read it green in CI, whose database has a non-byte-order
  collation.

## Evidence

Pull request sjawhar/legion#2391, forward merges f6aae9bade24 and 98b5c5ffb297, on a pgserver
0.1.4 Postgres 16 cluster. The cluster was first initialised without `-E` (`SQL_ASCII`, collation
`C`), and the two encoding tests failed. After a re-initialisation with `-E UTF8 --locale=C.UTF-8`
both passed, and the rank test still refused. `locale -a` printed only `C`, `C.utf8` and `POSIX`,
and `create database … locale_provider icu` answered `ICU is not supported in this build`. CI's
lanes that run the api package were green at f6aae9bade24.
