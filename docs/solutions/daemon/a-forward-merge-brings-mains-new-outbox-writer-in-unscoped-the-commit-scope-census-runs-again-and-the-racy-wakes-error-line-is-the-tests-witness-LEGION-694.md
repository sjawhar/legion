---
title: "A forward merge brings main's new outbox writer in unscoped: the commit-scope census runs again over main's delta, and the racy wake's ERROR line is the test's witness"
category: daemon
tags:
  - outbox
  - forward-merge
  - commit-hooks
  - intake.WithCommitHooks
  - wakeOnCommit
  - census
  - race-witness
  - review-round
date: 2026-10-17
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "LEGION-694"
  - "sjawhar/legion#1999"
---

# A forward merge brings main's new outbox writer in unscoped: the commit-scope census runs again over main's delta, and the racy wake's ERROR line is the test's witness

Extends docs/solutions/testing/the-comparability-grep-runs-again-after-every-forward-merge-of-main-and-a-port-to-deepequal-keeps-slices-equals-nil-rule-LEGION-578.md.

- A branch that puts a new invariant on an existing call — here, every outbox write runs inside
  `intake.WithCommitHooks` so `projectRecords.wakeOnCommit` can wake the row's lane at the commit
  (`internal/daemon/daemon.go`), and every field the runner reads on many goroutines sits under a
  mutex — has a census of that call's sites. The census is a step of every forward merge, run over
  `main`'s delta, not a list the plan settled once: code `main` gained after the fork point was
  written against the call as it was, and arrives either with no conflict marker or inside a
  conflict you resolve by keeping both sides, which makes `main`'s side look reviewed when it is
  new. For the outbox the census is
  `grep -n 'records\.Enqueue(\|records\.ExpediteOutbox(' internal/daemon/*.go internal/api/*.go | grep -v _test`:
  every hit's context must be the `scoped` a `WithCommitHooks` returned (a write inside an
  `intake.ApplyFact` handler owns its scope by construction). For a runner the branch made
  concurrent, every field `main` added to the runner struct is shared state until shown otherwise:
  grep `r.<field>` and guard it, and move a check `main` made before its serial loop's `execute`
  into the per-row path.
- When the broken path's symptom is a wake that races the commit, the eventual effect still
  usually happens — the commit wins the race more often than not — so a test that waits for the
  effect (the notice published, the row gone) passes red and green alike. The deterministic
  witness is the fallback's own log line (`wakeOnCommit`'s ERROR
  `outbox row written outside a commit scope; its lane is woken at once`), captured by the
  package's `syncBuffer` and asserted absent, beside the effect and the untouched sweep timer,
  never instead of them. Prove the test red by replacing the scope with
  `scoped, committed := ctx, func() {}` at the site (a `go test -overlay` file does it without
  touching the tree), then restore it.
- `go test -race` in CI catches none of this: the race is between a commit and a wake, not two
  memory accesses, and an unguarded map `main` added is only raced once a test drives two rows of
  different issues at once, which no test did. The census is the check.

## Evidence

- LEGION-694's plan (D2) censused six outbox writers at `9fef603b` and the branch scoped each
  (`notice_exceptions.go:170-190`, `:340-351`; `api/architect.go:362-399`; the engine's,
  admission's and linger's writes inside `ApplyFact`). The fourth forward merge of `main`
  (`0b7360de`) brought sjawhar/legion#1987's `ready_merge` kind, whose refusal path
  (`finishRefused`, `outbox.go`) enqueues the architect's `merge-refused` notice through
  `r.records` — in production the waking `projectRecords` — in a plain `pgx.BeginFunc(ctx, …)`.
  The merge tests could not see it: `mergeRunner` passes a bare `record.NewStore()` with no wake.
  Review round 2 found the ERROR line on a normal path and the notice waiting for the 30 s sweep
  where `main`'s poll took it within 100 ms.
- The same merge's `limits` map (#1987's per-process GitHub rate-limit deferral) was written for
  the serial loop and read unguarded; folded in as `noteLimit` under `limitsMu` in `settle` and
  `deferral` before a claimed row runs in `runRow` (`outbox_lanes.go`).
- `TestAREADYMergeRefusalsNoticeRunsOnTheBoardLaneAsTheRefusalCommits` (`outbox_lanes_test.go`)
  drives a `ready_merge` refusal through the real lanes over Postgres with a fake GitHub
  (`refusedAnswer`) and a waking `projectRecords`, and holds the publish to the architect topic,
  both rows finished, the one sweep timer Run armed never fired, and the ERROR line absent. Against
  the unscoped write it fails on that line (`outbox_lanes_test.go:545`), 3/3 under the tester's
  overlay; the publish and the row counts passed either way.
- A sibling instance of the same rule, for a branch that deletes a mechanism:
  docs/solutions/legion/a-deletion-branchs-forward-merge-runs-the-deletion-census-over-mains-new-files-and-looks-for-the-working-copys-orphan-LEGION-631.md.
