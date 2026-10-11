---
title: "Arm a GitHub merge only after your own gate accepts: gh pr merge --auto is a merge, not a flag, and --disable-auto dequeues nothing"
category: github
tags:
  - gh
  - auto-merge
  - merge-queue
  - match-head-commit
  - READY
  - merger
date: 2026-10-10
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# Arm a GitHub merge only after your own gate accepts: gh pr merge --auto is a merge, not a flag, and --disable-auto dequeues nothing

- `gh pr merge <n> --auto --squash --match-head-commit <head>` merges at once when the repository's
  rules are already satisfied (cli/cli#13880; the CLI only passes the request to GitHub). Treat it as
  the merge itself. Run it only after every gate of your own has accepted — in Legion, the moment the
  daemon accepts the merger's `handoff_complete` with `ready: true`, in the same turn, never before
  and never from a later role — because each refusal that gate can still give (the design gate, a
  head carrying `.legion/<issue>/`, a conflict, a check not green) names a condition an armed request
  would merge through. A child issue's merger cannot even read its root's design gate (`designGate`
  is on the root's record alone), so "check the gate yourself, then arm" is not available to it.
- `--match-head-commit` is checked when the request is made, not when the merge lands: an armed
  request survives a later push. A withdrawn READY therefore needs an explicit disarm,
  `gh pr merge <n> -R <owner>/<repo> --disable-auto`, by whoever pushes next.
- `--disable-auto` calls `disablePullRequestAutoMerge` and nothing else (gh v2.98.0,
  `pkg/cmd/pr/merge/merge.go:206-211`, `http.go:106`): it dequeues nothing. A merge queue drops a
  pull request whose required check failed by itself; one still queued is removed with the
  `dequeuePullRequest(input:{id:<pull request node id>})` mutation.
- When nothing is armed — the submission was refused (a repository with `allow_auto_merge=false`),
  or the queue already dropped it — `--disable-auto` exits 1 with
  `Can't disable auto-merge for this pull request.`. A prompt that orders the disarm says what that
  answer means, or the next agent treats a routine exit 1 as a failure.
- A sentence about what a third-party command does is read at the pinned version's source before it
  goes into a prompt: the first wording here said `--disable-auto` "dequeues it", which the source
  contradicts; the tester's live run on real GitHub, not the fake, found the exit-1 case.

## Evidence

sjawhar/legion#1843, spec v8 (Sami: "you should be submitting to the merge queue. That's why it's
there!"). The spec paragraph read submit-then-post; the implementer ordered it acceptance-first in
`packages/daemon/internal/prompts/roles/merger.md` step 4 and `go/merger.md` (a refused READY submits
nothing; a design-gate refusal is submitted later on the architect's word), and the architect kept
the ordering (envoy 7e046fdf). The implementer's disarm rule is `roles/implementer.md:12` and
`skills/legion-worker/references/merge-gate.md`'s "A withdrawn READY is disarmed by the implementer";
pinned by `prompts_test.go`'s merger and implementer rows. The tester's round 7 drove the command as
the review App in `sjawhar/legion-smoke` (pull requests #538–#541): `--auto` refused on a repository
without auto-merge leaves the pull request OPEN with `autoMergeRequest` null, and `--disable-auto`
on it exits 1 with the message above — the gap the reviewer's round 3 closed (eabefdc2).
