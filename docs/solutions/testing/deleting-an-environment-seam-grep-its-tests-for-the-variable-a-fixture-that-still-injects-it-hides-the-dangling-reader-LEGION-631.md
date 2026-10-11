---
title: "Deleting an environment seam: grep its tests for the variable, since a fixture that still injects it hides the dangling reader"
category: testing
tags:
  - environment-variable
  - fixture
  - deletion
  - shim
  - PATH
  - live-proof
date: 2026-10-10
status: active
module: packages/daemon/internal/shim
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# Deleting an environment seam: grep its tests for the variable, since a fixture that still injects it hides the dangling reader

- A variable the runtime stops setting is deleted at three sites, not one: the setter, every
  reader, and every test fixture that injects it. A reader left behind keeps passing its unit
  tests as long as the fixture still supplies the value, and fails for the first time in the live
  proof, where nothing does.
- Grep the variable's name over `*_test.go` and `*.test.ts` with the same census as the production
  code, and rewire each fixture to the seam that replaces it (the real `PATH` for a binary, a mounted
  file for a credential). A fixture that asserted the old seam's exclusivity — a decoy binary first on
  `PATH` that must never run — is the loudest sign the reader is still there: it was written to prove
  the opposite of the new rule.
- The plan's risk list is a grep list: a risk it names ("dropping `LEGION_JJ_PATH` puts the jj on an
  unvalidated `PATH`") is a variable whose readers and fixtures are enumerated before the first push,
  not a sentence left for the tester.

## Evidence

sjawhar/legion#1843 deleted `LEGION_GH_PATH`, `LEGION_GIT_PATH` and `LEGION_JJ_PATH` from every pane
and pod (`internal/api/version.go`, the LEGION-631 entry). The shim's adoption still read
`LEGION_JJ_PATH` and refused when it was empty, and its tests still passed the variable in
`cfg.Env` with a decoy `jj` first on `PATH` asserting a `PATH` lookup never happened — so
`go test ./internal/shim/` was green while the operator lane's stage 4a at 18dc1df2 died at
`adopt-working-copy` with `worker-shim: LEGION_JJ_PATH is not set` on every role of every pod
(`.legion/LEGION-631/test.json`, round 4, failure 1). The tester's red test
`TestAdoptWorkingCopyRunsTheJJOnPATHWithNoLEGIONJJPATH` (`internal/shim/shim_test.go:1013`) pinned the
new seam; the fix is `exec.LookPath("jj")` at `internal/shim/shim.go:597`, with the refusal naming the
`PATH` searched and never the variable. The plan had named the risk (`plan.json`, risks: "Dropping
LEGION_JJ_PATH puts legion push/handoff complete on an unvalidated PATH jj").
