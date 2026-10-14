---
title: "Red-on-main for a Go daemon test is a git archive of main from the jj git store plus the branch's test files, run against the pod's embedded Postgres"
category: testing
tags:
  - red-on-main
  - git-archive
  - jj-git-root
  - go-test
  - worker-pod
  - LEGION_TEST_PG_DSN
date: 2026-10-14
status: active
module: packages/daemon
related_issues:
  - "LEGION-698"
  - "sjawhar/legion#1950"
---

# Red-on-main for a Go daemon test is a git archive of main from the jj git store plus the branch's test files, run against the pod's embedded Postgres

Extends docs/solutions/testing/an-embedded-postgres-from-the-zonky-jar-runs-the-pods-postgres-lanes-and-the-nats-only-tests-are-skipped-by-name-LEGION-668.md.

- Prove a new Go test red on `main` without a second jj workspace and without touching the
  working copy: export `main`'s tree read-only from the workspace's git store and copy in only the
  branch's test files, then run the selection there against the same embedded Postgres the branch
  run uses.
  `GITDIR=$(jj -R "$LEGION_WORKSPACE" git root)` names the store (a pod workspace is a jj workspace
  of a shared clone, so `.jj/repo` is a pointer, not the store); then
  `git --git-dir="$GITDIR" archive <main sha> go.work go.work.sum packages/daemon packages/envoy | tar -x -C /tmp/main-scratch`
  (`go.work` binds both modules, so both are exported), then
  `jj -R "$LEGION_WORKSPACE" file show -r @- packages/daemon/internal/<pkg>/<file>_test.go > /tmp/main-scratch/packages/daemon/internal/<pkg>/<file>_test.go`
  for each test file, `go -C /tmp/main-scratch/packages/daemon vet ./internal/<pkg>` to see they
  compile against main's other test files, and the `go test -run` selection with
  `LEGION_TEST_PG_DSN` set. Go needs no install step, unlike the TS rigs. Quote the `--- FAIL`
  lines and the assertion messages in the proof; the tester re-runs the same command.
- Write the new tests so they compile on `main`: only helpers that exist on `main`, and any new
  helper defined in one of the copied files under a name no other test file in the package uses.
  A helper changed in a file you do not copy breaks the compile there.
- Remove `/tmp/main-scratch` afterwards; `git archive` is a read of the store, so nothing in the
  workspace changes, and `jj status` stays clean.

## Evidence

LEGION-698's implementer proved `TestAnApprovalEndsTheRoundWhateverCISays`,
`TestARedVerdictSendsTheTreeBackToImplementing`'s retro and merging rows and
`TestEveryPhaseAndTriggerHasARowOrNamedIgnore` red on `main` e4ec5a20 this way (the first run of
`git archive` against `.jj/repo/store/git` failed with "not a git repository": the workspace's
`.jj/repo` points at the shared clone's store, which `jj git root` names), and the tester re-ran
the same recipe in a fresh archive on a Postgres of its own and got the same FAIL/PASS split
(`.legion/LEGION-698/implement.json` proof[0], `test.json` round 1, on the branch of
sjawhar/legion#1950).
