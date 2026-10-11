---
title: "The comparability grep runs again after every forward merge of main, and a port to reflect.DeepEqual keeps slices.Equal's nil rule"
category: testing
tags:
  - go
  - comparable
  - slices.Equal
  - reflect.DeepEqual
  - forward-merge
  - record.Notice
  - postgres-backed-tests
  - worker-pod
date: 2026-10-08
status: active
module: packages/daemon
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
---

# The comparability grep runs again after every forward merge of main, and a port to reflect.DeepEqual keeps slices.Equal's nil rule

Extends docs/solutions/testing/a-struct-that-gains-a-slice-field-stops-being-comparable-grep-every-equality-over-it-the-suites-that-panic-are-the-ones-a-pod-skips-LEGION-578.md.

- The grep that note prescribes is not once-before-the-first-push: it is a step of every forward
  merge of `main` while the branch lives. Code `main` gained after the fork point was written against
  the type as it was, comparable, and a textual merge carries it in untouched. Run the same grep over
  the merged tree with `main`'s `*_test.go` in range before pushing the merge commit.
- Add a third shape to the grep. Beside `==` on the struct (a compile error) and `slices.Equal` over
  an interface slice (a run-time panic), an operand converted to the interface — `x != record.OutboxPayload(y)`
  — compiles, passes `go vet`, and panics only when it runs. Grep `!= <Interface>(` and `== <Interface>(`
  for every interface the struct satisfies.
- Push the merge commit alone and read its CI `Tests` run before stacking anything on it. The suites
  that compare notices need Postgres, which a worker pod does not have, so that run is their first
  execution; a pod-side `go test` that passes proves nothing about them.
- A port of `slices.Equal` to `reflect.DeepEqual` changes one answer: `slices.Equal` calls an empty
  slice and a nil slice equal, `reflect.DeepEqual` does not. Before porting a site whose `want` can be
  nil (a table case expecting no rows), make the producer start nil (`var rows []T`, appending into
  it) so an empty result is nil too, or keep the equivalence in one named helper beside the site.
  A mechanical substitution turns that case red with `rows = [], want []`.

## Evidence

sjawhar/legion#1846 forward-merged `main@origin` at 38e27ad3 after LEGION-462's #1752 landed. The
merge compiled and vetted clean but for `undefined: slices` in `hold_test.go` (the branch had removed
the import; `main` had added a `slices.Equal` over notice rows), the one symptom jj's textual merge
showed. CI's `Tests` run 37849592966 at the merge then failed in two suites `main` had added since the
fork: `TestAFinishedWorkerThatDiesIsRelaunchedAndItsFailureHoldsNothing` panicked
`comparing uncomparable type record.Notice` at `internal/daemon/outbox_lifecycle_test.go:628`
(`notices[0] != record.OutboxPayload(died)`, the interface-converted operand), and the three table
cases of `TestAFinishedRolesFailedClaimIsNoticedAndHoldsNothing` in `internal/workflow/hold_test.go`
failed with `notice rows = [], want []` once the branch's `reflect.DeepEqual` replaced `main`'s
`slices.Equal` — `noticeRows` returns a non-nil empty slice, the table's `want` is nil. Neither suite
runs in a worker pod (no `LEGION_TEST_PG_DSN`). 7721ffa0 fixed both: `reflect.DeepEqual` at the
first, a `sameNotices` helper (`len(got) == 0 && len(want) == 0 || reflect.DeepEqual(got, want)`) at
the second; `Tests` run 37850383816 at 7721ffa0 passed, `internal/daemon`, `internal/workflow` and
`internal/admit` included. The reviewer's round-4 fast-follow names the simpler form the rule above
prefers: `var payloads []record.OutboxPayload` in `noticeRows`, plain `reflect.DeepEqual` at the
call, the helper deleted (thread 4224909990; the fast-follow comment on the pull request,
issuecomment-6070622880). The tester ran the two fixed tests on a real Postgres in its pod and read
the two CI runs; the first run's failure is the proof that the pod-side checks could not have seen it.
