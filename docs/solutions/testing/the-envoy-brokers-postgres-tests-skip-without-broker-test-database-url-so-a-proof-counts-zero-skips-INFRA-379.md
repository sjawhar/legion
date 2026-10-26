---
title: "The envoy broker's Postgres tests skip without BROKER_TEST_DATABASE_URL, so a proof counts zero skips"
category: testing
tags:
  - postgres
  - BROKER_TEST_DATABASE_URL
  - worker-pod
  - go-test
  - skip
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-379"
  - "sjawhar/legion#2194"
---

# The envoy broker's Postgres tests skip without BROKER_TEST_DATABASE_URL, so a proof counts zero skips

Extends docs/solutions/testing/an-embedded-postgres-in-a-worker-pod-the-zonky-jar-unpacked-with-python-gives-the-daemons-database-suites-a-run-LEGION-631.md.

- The broker's database tests read `BROKER_TEST_DATABASE_URL`
  (`packages/envoy/internal/broker/store/storetest/storetest.go`, `serverURL`). Without it they
  skip, not fail. A bare `go test ./cmd/broker` then prints `ok`, which proves only that the
  tests compile.
- Start the zonky Postgres from the LEGION-631 recipe under `$TMPDIR`, never `/tmp`, and export
  `BROKER_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:<port>/postgres?sslmode=disable'`.
- In the proof, run with `-v` and count skips:
  `go test -count=1 -v ./cmd/broker 2>&1 | grep -c -- '--- SKIP'` must print `0`.
- Use the same tests with the variable unset as the control: each one prints
  `--- SKIP: … BROKER_TEST_DATABASE_URL must be set`, which shows that the pass needed the
  database.

## Evidence

INFRA-379 (sjawhar/legion#2194) repaired two broker tests that need Postgres. With the variable
unset, both skipped and the package printed `ok` in 0.03 s. With Postgres 16.4 on
127.0.0.1:54329, both passed (11.58 s, 11.04 s), and the whole package passed with 18 passes and
0 skips in 84 s. The tester repeated this on its own cluster on port 54339.
