---
title: "A struct that gains a slice field stops being comparable: grep every equality over it and over the interfaces that hold it before the push, since the suites that panic are the ones a pod skips"
category: testing
tags:
  - go
  - comparable
  - slices.Equal
  - reflect.DeepEqual
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

# A struct that gains a slice field stops being comparable

- Adding a slice or map field to a Go struct makes it non-comparable. A direct `==` on the struct
  fails to compile and is found at once; `slices.Equal` over a slice of an **interface** the struct
  implements still compiles — the interface is comparable — and panics (`comparing uncomparable
  type`) only when the comparison runs.
- Before the push, grep the type's name and every interface it satisfies for `==`, `!=` and
  `slices.Equal`, in tests included (`grep -rn 'slices.Equal\|== record\.\|!= record\.' --include=*_test.go`
  for a `record` type), and move each to `reflect.DeepEqual`. Do not wait for the tests.
- A worker pod runs no Docker and, unless the tester starts one, no Postgres, so the packages whose
  suites need them (`internal/admit`, `internal/workflow`, the Postgres-backed tests of
  `internal/daemon` and `internal/record`) skip or `Fatal` there. Those are exactly the suites that
  build notices and compare them. Name the skipped packages in the handoff, and treat CI's `Tests`
  run as the first execution of those comparisons.

## Evidence

sjawhar/legion#1846 gave `record.Notice` an `OpenCapabilities []string` field (the controller tick's
payload). `go vet ./...` found the two direct `!=` comparisons (`internal/daemon/outbox_test.go`,
`internal/workflow/engine_test.go`), which moved to `reflect.DeepEqual` before the first push. Three
sites in `internal/workflow/hold_test.go` compared `[]record.OutboxPayload` — an interface slice —
with `slices.Equal`; they compiled, were skipped in the pod (`LEGION_TEST_PG_DSN` unset), and
panicked in CI's `Tests` run at ff33c8c9 (`panic: runtime error: comparing uncomparable type
record.Notice`, `hold_test.go:43`). The reviewer's census row later noted the same: the signature
changes were compile-time, the comparability change was not.
