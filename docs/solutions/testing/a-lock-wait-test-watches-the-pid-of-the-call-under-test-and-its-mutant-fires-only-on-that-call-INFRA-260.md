---
title: "A lock-wait test watches the pid of the call under test, and its mutant fires only on that call"
category: testing
tags:
  - postgres
  - pg-locks
  - lock-wait
  - negative-control
  - mutation-testing
  - dispatch-server
  - document-settlement
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-260"
  - "sjawhar/legion#2217"
---

# A lock-wait test watches the pid of the call under test, and its mutant fires only on that call

A Dispatch test that asserts "the call waited behind my lock" must check the call's own backend,
not a count of every backend behind the lock. Its negative control must break only the call under
test, never the fixture that sets the test up.

- **Watch one pid, not a count.** Run the call on a store of its own with one connection: copy
  `deps.Store.Pool.Config()`, set `MaxConns = 1`, `MinConns = 0` and `MinIdleConns = 0`, open it
  with `pgxpool.NewWithConfig`, wrap it with `store.NewPool`, and give only the call those deps.
  Acquire and release that connection once to read its `PgConn().PID()`. Then poll through the
  holder's own transaction:
  `pid = $1 and not granted and pg_backend_pid() = any(pg_blocking_pids(pid))`. This needs the
  call to run one statement at a time on `deps.Store`. If it reaches the database another way,
  the poll never sees its pid and the test fails. A wrong pid cannot make the test pass by
  mistake.
- **Expect a background waiter.** Every issue create schedules settlement of the issue's spec
  document (`api/issue_create.go:401`). Settlement locks the issue row with
  `for no key update` (`docs/service.go:254-256`) and reads `asks`. The API test server runs it
  20 ms later (`api/server_test.go:151-152`). A lock on `issues` or `asks` taken just after a
  create therefore gets a waiter that is not the code under test. If a count is enough for the
  test, keep settlement out of the window with a long `settle` (`api/server_test.go:61-64`), or
  wait until no settlement is owed before taking the lock (`cmd/dispatch/shutdown_test.go:129-130`).
  `storetest.WaitForLockWaiters` counts every backend behind the holder, so it has this weakness.
- **Fire the early-answer mutant only on the probe call.** Key it on a value that only the call
  under test carries, here the `askID`s `locked-probe` and `locked-project-probe`. A mutant that
  also slows the fixture's own calls changes the fixture's timing, and can hide the weakness it is
  meant to expose.
- **Show the mutant discriminates.** Run it against the old guard and the new one, and record
  both: the old one must pass it and the new one must fail it. "Allowance 1 ns" and "run the step
  without the allowance" are useful controls, but they change the real query. Neither checks that
  the waiter is the real query.
- **Add an "active but not blocked" mutant.** Run `select pg_sleep(2)` on the call's own
  connection, then return. The guard must fail it, because a busy backend is not a backend that
  waits behind the lock.

Related: `docs/solutions/testing/bash-harness-cases-that-pass-for-the-wrong-reason.md` (the same
family of failure in a shell harness);
`docs/solutions/testing/a-test-only-knob-widens-the-value-production-runs-pin-it-at-the-construction-a-deployment-reaches-INFRA-56.md`
(the allowance knob these tests set).

## Evidence (INFRA-260, sjawhar/legion#2217)

INFRA-260 replaced the 800 ms stopwatch in
`TestComputeAndPersistSuggestionsRespectsDeadlineUnderLock` and
`TestComputeAndPersistSuggestionsRespectsDeadlineWhenProjectLookupIsLocked`
(`packages/envoy/internal/dispatch/api/suggestions_test.go`) with the helper
`suggestionsBehindLock`. Round 1's helper polled
`count(*) … where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))`. Its controls
passed: allowance 1 ns failed both tests, and the two unbounded-context mutants hung until
`go test -timeout`. The tester's early-answer mutant also failed both tests. It slept 2 s in every
`computeAndPersistSuggestions` call, including the fixture's issue create, so settlement finished
before the test took its lock.

In review, a `go test -overlay` mutant that fired only for the probe `askID`s (sleep 2 s, return
`Missing`, no query) passed both tests 10 of 10. A logging overlay named the waiters: settlement's
ask read on `asks`, and its `artifacts`/`issues` read on `issues`.

Round 2 (commit a7fcda36a6b4) runs the call on its own one-connection pool and polls for its pid
(`suggestions_test.go:270-333`). The probe-only mutant now fails both tests 5 of 5 with
`answered … before its query waited behind the lock`, and still passes round 1's helper 5 of 5.
The `pg_sleep` mutant fails 5 of 5. Three concurrent whole-package copies pass with both tests
passed in each copy.

Two other callers of the counting idiom have the same exposure, inferred from the code and not
reproduced: `cmd/dispatch/shutdown_test.go:101` and `api/ask_turn_test.go:386`. The report,
dispatch://INFRA-49/ask/4e275ed1-291c-42ad-9f0e-927495b45e44, was folded into dispatch://INFRA-436,
the issue that fixes flaky Dispatch tests.
