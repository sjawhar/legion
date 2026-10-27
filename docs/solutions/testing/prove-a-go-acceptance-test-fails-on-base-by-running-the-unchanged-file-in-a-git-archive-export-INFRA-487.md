---
title: "Prove a Go acceptance test fails on base by running the unchanged file in a git archive export"
category: testing
tags:
  - go-test
  - negative-control
  - git-archive
  - worker-pod
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-487"
  - "sjawhar/legion#2269"
---

# Prove a Go acceptance test fails on base by running the unchanged file in a git archive export

- Write the acceptance test against symbols the base commit already has (its existing helpers, SQL
  on tables that exist there), so the same file compiles on base.
- Export base read-only, drop the unchanged file in, and run it there:
  `git --git-dir="$(jj -R "$LEGION_WORKSPACE" git root)" archive <base> packages/<module> | tar -x -C "$TMPDIR/<dir>"`,
  copy the test file in, and run `GOWORK=off go test -count=1 -v -run <Test> ./<pkg>` from the
  export's module. `GOWORK=off` only works for a module with no intra-repository dependency
  (`packages/envoy` qualifies; `packages/daemon` does not, see
  `docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md`).
  It is an export, not a second workspace; delete it after.
- Count zero skips on both runs (`grep -c -- '--- SKIP'`), and accept only a failure on the
  behaviour, never a compile error, a skip or a harness failure.
- Assert the new log line or other new artefact last, so base fails on the refusal the user met
  rather than on a missing line, and quote that base failure in the PR.

## Evidence

`packages/envoy/cmd/broker/outage_test.go` (`TestABrokerOutageLongerThanALease`) uses only
`buildBroker`, `startBroker`, `podBroker` and SQL on `enrollments`, all present on `eee16056`. In
an export of that commit it failed at its first post-restart step with
`agent-secrets: proof invalid: not live (PROOF_INVALID)`, zero skips; at the head it passed. The
tester re-ran the same export independently and saw the same failure.
