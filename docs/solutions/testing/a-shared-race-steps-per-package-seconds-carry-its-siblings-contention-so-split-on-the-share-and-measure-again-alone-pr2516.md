---
title: "A shared race step's per-package seconds carry its siblings' contention, so split on the share and measure again alone"
category: testing
tags:
  - go-test
  - race-detector
  - timeout
  - github-actions
  - contention
  - measurement
date: 2026-10-29
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2516"
---

# A shared race step's per-package seconds carry its siblings' contention, so split on the share and measure again alone

Extends docs/solutions/testing/a-new-tests-race-cost-is-charged-to-a-package-already-near-its-lanes-limit-so-size-the-fixture-by-the-cases-pr2497.md.

One `go test` line's `-timeout` is one budget for every package on it, and `go test` runs those
package binaries concurrently. So the `ok <package> <N>s` line of a multi-package step is an
upper bound that carries the other packages' contention, not the package's own cost.

- **Decide the split on the share, not the seconds.** A package at 98% of its step's `-timeout`
  needs a lane of its own whatever the absolute number is; the failure is not that the slow
  package goes red, it is that the step dies and every other package on it is never judged.
- **Never size a new lane's `-timeout` from a shared step's number.** It is high by whatever the
  neighbours cost. Give the new job a deadline the shared figure fits inside, run it once, then
  restate the budget against the isolated figure.
- **Every quotation of a package time says which it is.** Write "`<N>`s while it shared
  `<job>`'s `<M>`-minute step" or "`<N>`s alone in `<job>`", because the two differ by tens of
  percent and a later reader cannot tell them apart.
- **Read which package died from the panic's goroutine 1 stack, not from the step's package
  lines.** A multi-package step's panic prints `running tests:` for whatever was running at that
  moment, and its package lines in finish order; the binary that hit the deadline is the one
  whose `TestMain` is in goroutine 1.

## Evidence

In sjawhar/legion `envoy-go-race` ran six package groups on one `go test -race -timeout 30m`
line. On job 123461966609 `internal/dispatch/docs` reported `1772.969s` of that 1800 s budget,
and the six packages' times summed to 5125 s inside 1842 s of step wall — they ran concurrently,
so none of those six numbers was a package's own cost. The step later died on its deadline
(job 123503246603).

sjawhar/legion#2516 moved that one package to `envoy-go-race-docs` with `-timeout 60m`. On its
own job (run 40921821162, job 123625657214) the same package reported
`ok  github.com/sjawhar/envoy/internal/dispatch/docs  1015.560s` — **43% below the shared
figure, from nothing but the removal of its neighbours**. The two packages left behind show the
same gap from the other side: in that run the surviving step reported `api 871.539s` and
`pmdoc 1210.064s`, while sjawhar/legion#2517 measured the same two alone at 746.7 s and
1023.5 s.

The stack rule comes from the same lane. Job 123643922299 printed
`panic: test timed out after 30m0s` under four subtests of
`TestSetAskBlockTextRefusalOfEveryBrokenBodyHoldingADecisionLeadsToAValidDecision`, and its
package lines named `api`, `pmdoc`, `delivery`, `store` and `bus` as `ok`. Goroutine 1 held
`github.com/sjawhar/envoy/internal/dispatch/store/storetest.Main` →
`github.com/sjawhar/envoy/internal/dispatch/docs.TestMain`
(`packages/envoy/internal/dispatch/docs/persistence_test.go:19`), so the binary that died was
the document service's. A report of that run that read the directory out of the test's name
instead proposed a race job for `packages/envoy/internal/dispatch`, which holds no Go file at
all (`go list ./internal/dispatch` answers `no Go files in …`).
