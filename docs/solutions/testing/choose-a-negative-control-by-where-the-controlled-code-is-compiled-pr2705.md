---
title: "Choose a negative control by where the controlled code is compiled"
category: testing
tags:
  - negative-control
  - go
  - overlay
  - subprocess
date: 2026-11-04
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2705"
---

# Choose a negative control by where the controlled code is compiled

Before you plan a one-edit revert, ask how the code you want to break reaches the process that
runs the assertion. The answer picks the control, and getting it wrong costs a run that is green
for the wrong reason.

- **Linked into the test binary** — `go test -overlay <file>.json` with the one edited file. It
  reaches everything the test binary compiles, and nothing else.
- **A subprocess the test builds itself** — the overlay reaches it only when the builder passes
  `-overlay` to `go build`. Check the builder. When it does not, export the branch
  (`git archive <rev> <path> | tar -x -C <dir>`), apply the one edit in the export, build the
  binary there, and point the test at it.
- **A surface an operator meets** — the previous head's binary, built the same way from a
  `git archive` of that head, run beside this head's. That control also answers "was this
  actually broken before", which an overlay cannot.
- Say in the evidence which kind you used and why, because the reader cannot tell from the
  result, and a control that could not reach the code reports green.
- A control measured at the head where that code last changed stays valid at a later head that
  did not touch it; say which head, rather than re-running it blind or dropping it.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454) used all three in one pull request.

Overlay, for the model linked into the test binary: one edit snapping each refused renewal's
failure to the nearer end of its client timeout turns six pinned rows of
`TestSurvivesWalksEachRenewersOwnRetrySchedule` red and moves every floor.

Export, for the fleet: `RereadOverallFor`'s `n := max(replicas, MinReplicas)` pinned to
`n := MinReplicas`, built from an edited `git archive` export, because
`cmd/broker/main_test.go`'s `buildBroker` is a plain `exec.Command("go", "build", "-o", binPath, ".")`
with no `-overlay` hook — `go test -overlay` cannot reach that binary at all. All three rows of
`TestABrokerFleetDerivesItsRereadRate` then go red, the pods logging `one every 1s` and refusing
with `Retry-After: 1`. The contrast is in the same module: `internal/cmdtest`'s builder does pass
`ENVOY_TEST_GO_OVERLAY` to `go build -overlay`, so a probe built through it takes an overlay, and
a reader who assumes every builder has that hook plans the wrong control.

Across heads, for the operator's own surface: the lease refusal is a boot-time refusal, so the
tester built the previous head's broker binary (`git archive 7d025090 packages/envoy`) and showed
it boots at the two settings the review traced as lapsing (100 s at a 10 s sweep, 355 s at 60 s)
while this head's binary refuses both, naming 105 s and 360 s.

The fleet control was measured at the head where that code last changed and recorded with that
head's SHA, since neither the deriver nor its rig moved in the rounds after it.
