---
title: "A red Go lane on the pod's shared embedded Postgres may be stale rows from an earlier run: re-run on a fresh database before reading it as yours"
category: testing
tags:
  - worker-pod
  - embedded-postgres
  - LEGION_TEST_PG_DSN
  - go-test
  - red-on-main
date: 2026-10-21
status: active
module: packages/daemon
related_issues:
  - "LEGION-791"
  - "sjawhar/legion#2096"
---

# A red Go lane on the pod's shared embedded Postgres may be stale rows from an earlier run: re-run on a fresh database before reading it as yours

Extends docs/solutions/testing/an-embedded-postgres-from-the-zonky-jar-runs-the-pods-postgres-lanes-and-the-nats-only-tests-are-skipped-by-name-LEGION-668.md.

- The pod's embedded Postgres is one database every role's runs share, and the daemon's `issues`
  table is keyed by issue key alone: a row an earlier run left (an `LEGION-1` with an empty
  repository) fails a later run's state tests with `record issue LEGION-1: it is recorded on @
  …, and an issue's repository and default branch never change` on pristine `main` too.
- Before blaming the branch, create a database of your own and re-run the lane there:
  `CREATE DATABASE <issue>_fresh` through a throwaway pgx program (the zonky jar ships no `psql`),
  then `LEGION_TEST_PG_DSN=postgres://postgres@127.0.0.1:<port>/<issue>_fresh?sslmode=disable`.
  Name the result either way in the handoff.

## Evidence

sjawhar/legion#2096 (LEGION-791): `internal/daemon -run State` failed two tests on the shared
`postgres` database and passed all eight on `legion791_fresh`; the same two failed on a `git
archive` of `main` against the shared database. CI's `Tests` run confirmed the lane green at every
head.
