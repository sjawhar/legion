---
title: "A many-package Go lane reads the package-level FAIL lines, uncapped: the pod's environmental test failures flood a head window and hide the branch's one red package"
category: testing
tags:
  - go-test
  - worker-pod
  - lane-output
  - environmental-failures
  - truncation
date: 2026-10-09
status: active
module: packages/daemon
applies_when:
  - Running go test over many packages of packages/daemon in a worker pod and reading the result from a filtered or capped shell pipeline
  - Packages the pod cannot run (Postgres, Docker, cgo) are in the set
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# A many-package Go lane reads the package-level FAIL lines, uncapped: the pod's environmental test failures flood a head window and hide the branch's one red package

Extends docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md.

- Read a multi-package run at the package level first: `go test … 2>&1 | grep -E '^(ok|FAIL|---
  FAIL)'` is the wrong first filter, since one package the pod cannot run (`internal/admit` without
  `LEGION_TEST_PG_DSN`) prints forty `--- FAIL:` lines, and a `| head -40` after it shows those
  forty and nothing else. Filter to `^(ok|FAIL)\s` — the per-package verdict lines, one per package
  — and never cap the result; read the `--- FAIL` lines afterwards, per package you did not expect
  red.
- Take the environmental packages out of the set by name before the run, so every `FAIL` left is
  the branch's: `go test $(go list ./... | grep -v -E
  'internal/(admit|daemon|intake|natsauth|testnats|config|supervise|launcher)$')`, the list the
  extended note gives. A lane whose expected failures are mixed with its unexpected ones is read by
  eye, and the eye stops at the first expected one.
- A package `go test` reports red for the environment is still a package to run alone before
  calling it environmental: under the parallel load of a whole-module run in a gVisor pod,
  `cmd/legion`'s `TestWorkspaceInitSerializesTwoProcessesOnOneVolume` exceeds its 30 s flock bound
  and passes alone in 21 s. The verdict line names the package; the reason is in the `--- FAIL`
  lines of a run of that package by itself.

## Evidence

sjawhar/legion#1848 round 5 (the forward merge of main's #1846, merge commit 24f2532c). The
implementer's lane split the module in two: the touched packages by name (green), then every other
package through `go test … | grep -E '^(ok|FAIL|---|panic)' | head -40`. The second half's window
held exactly forty `--- FAIL: TestApplyFact…` and `TestReconcile…` lines from `internal/admit`
(`LEGION_TEST_PG_DSN is required for real Postgres admission tests`), an environmental failure the
extended note lists, and the package verdict `FAIL github.com/sjawhar/legion/daemon/internal/api`
— `TestStateGolden`, the contracts golden stale after the merge — was past the cap. The merge was
pushed; CI's `Tests` run 37864230699 at 24f2532c failed on that one test, reproduced in the pod with
`go test -run 'TestStateGolden$' ./internal/api/` in 0.008 s, and fixed in 7d98d70e (`Tests`
37865275948 green). In the same lane `cmd/legion` reported `FAIL` on
`TestWorkspaceInitSerializesTwoProcessesOnOneVolume` (30.46 s, its flock bound) under the parallel
run and passed alone in 21.16 s; `internal/launcher`'s
`TestAGenerationsCredentialsLiveExactlyAsLongAsItsChild` failed on `main`'s own tree in the same pod
(`the launcher left descendant 5338 running`), the gVisor timing failure the extended note names.
