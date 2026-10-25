---
title: "Fail one query of a real server by shadowing a function on search_path, not by a seam in the code"
category: testing
tags:
  - postgres
  - search-path
  - failure-injection
  - e2e
  - test-seams
  - t-parallel
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-184"
  - "sjawhar/legion#2157"
---
# Fail one query of a real server by shadowing a function on search_path, not by a seam in the code

To prove that a query's failure costs only what it should, make that one query fail in a server
you did not modify.

- Put `public` before `pg_catalog` on the test database's `search_path` and define a
  `public.<builtin>(…)` that misbehaves for one sentinel value and delegates to
  `pg_catalog.<builtin>` for every other. A row carrying the sentinel then fails, or hangs, in
  exactly the query that calls that builtin, and nothing else in the server changes.
- Pick the builtin the target query already calls (`lower(label)` for a case-insensitive label
  match). No binary is rebuilt, no flag is added, and the production code under test is the
  released one.
- Use it for the failure modes a unit seam cannot stage honestly: a query that raises inside a
  live transaction, and one that exceeds a `statement_timeout` without any Go timer racing the
  database (make the shadow sleep ten times the bound).
- Prefer it to a package-level `var` holding the SQL that a test overwrites. That seam turns a
  production constant into a variable for tests alone and silently forbids `t.Parallel`
  everywhere in the package: one parallel test and the others see the broken SQL. Where such a
  seam already exists, its comment must say both things — that the swap is safe only while no
  test in the package calls `t.Parallel`, and why the package's existing hook is not the seam
  here.
- Keep the sentinel out of the production path: a value no real row carries (`tester-boom`,
  `tester-slow`), so the shadow is inert for every other test on that database.

## Evidence

INFRA-184 needed proof that a failed or hanging `legion_managed_status` walk costs the field and
never the claim. The Go suite covers it with an SQL swap (`SQLSTATE 42P01`) and a `pg_sleep(5)`
against a 500 ms bound (`SQLSTATE 57014`), both of which rely on a package-level `var` and on no
test in `internal/dispatch/api` calling `t.Parallel`.

The tester proved the same behaviour against the real server instead, on a database whose
`search_path` names `public` before `pg_catalog` and whose `public.lower(text)` raises for the
label `tester-boom`, sleeps five seconds for `tester-slow`, and delegates for everything else.
An issue whose first label is one of those makes the walk's
`select … where lower(label) = 'legion'` fail or hang inside the running server, with no Go seam
and no rebuilt binary: the claim came back in about a second against the five-second walk, with
the timeout logged and the field absent, and the issue read back still held the claim.
