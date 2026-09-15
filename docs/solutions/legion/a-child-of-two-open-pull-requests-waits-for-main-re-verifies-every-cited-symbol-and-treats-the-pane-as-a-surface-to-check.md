---
title: "A child of two open pull requests waits for main, re-verifies every cited symbol there, and treats its own pane as a surface to check"
category: legion
tags:
  - stacked-branches
  - plan-citations
  - rebase
  - daemon-api-contract
  - commit-identity
  - ci-flake
  - process
date: 2026-09-15
status: active
module: legion workflow (planner, implementer, tester, reviewer)
related_issues:
  - "LEGION-81"
  - "sjawhar/legion#1108"
  - "LEGION-24"
  - "LEGION-25"
  - "LEGION-164"
  - "LEGION-170"
  - "LEGION-174"
  - "LEGION-175"
---

# A child of two open pull requests waits for main, re-verifies every cited symbol there, and treats its own pane as a surface to check

LEGION-81 was planned against two open pull requests (#1031 the Kubernetes runtime, #1044 the
in-cluster daemon) plus a third branch (LEGION-80) — three heads that were not `main` — while an
operator finished those pull requests by hand. Five process facts from the eight passes it took
are worth a paragraph each.

## When an operator is finishing the base by hand, the child waits for main

The first spec said "branch from `legion/LEGION-24`, open against it, retarget when it merges".
The operator was taking #1031 and #1044 through test, review, and fixes personally; a Legion push
or pull request on those branches would have landed on top of that work. The rule that replaced
it: the child's planner reads the base branches read-only (`legion gh -- pr diff`, a fetch read
in the workspace, never a commit on them), cites symbols *at the head it read*, and names that
head; the implementer starts only when both bases show `MERGED`, rebases the plan commit onto
`main`, and opens against `main` from the first push. The cost is idle time. The cost of the
alternative is an operator's in-flight branch receiving a Legion commit.

## A plan cited at three heads is a list of things to re-verify, not a map

The plan named `runProbePod` (at #1044's head 17d27613); on `main` it had been split into
`createProbePod`/`awaitProbeVerdict`/`judgeProbeLog`. It said `SESSION_STORAGE_VARIABLE` was
exported from `boot-probes.ts`; on `main` it was a bare `const`. Every line number was from a
head that no longer existed. A plan that spans branches under active review should carry a
`reverifyOnMain` list — this one did — and the implementer treats symbol names and shapes as
authoritative and everything else as a hint. Grep every cited symbol on `main` before writing a
test against it; when the plan's import plan turns out to be a module cycle, prove it with a
two-file repro (one minute) rather than reasoning about the import graph.

## A daemon API contract bump strands live panes (LEGION-164)

An architect pane keeps the plugin it booted with for its whole life. This tree's root architect
pane had loaded the contract-3 plugin at 05:43Z on 2026-09-14; when `main` moved the daemon API
contract from 3 to 4 (LEGION-102, the plugin-minted `requestId` on `spawn_worker`) and the
matching plugin release was installed, the daemon required the new shape while the pane still
spoke the old one: at 00:02Z on 2026-09-15 every `spawn_worker` it issued was refused
`400 requestId: Invalid input: expected string, received undefined` and its tree stalled, with
nothing on the pane saying why, until the daemon owner retired the pane at 00:10Z and the daemon
resumed the session under the installed plugin. Two consequences for a worker: a branch that
*does* bump the contract owns the operator's install-then-restart step in its README, because
the panes already running will not pick it up; and "contract stays N" in a pull-request body is
a claim about `main` at the last rebase, re-read every time — this branch bumped nothing, and
the number it did not bump moved from 4 to 5 under it (LEGION-16), which is how the body's
"contract stays 4" went stale by review time.

## The pane is a surface: check the commit identity before the first split (LEGION-170)

The daemon re-authors the workspace's working copy for the assigned role at each assignment
delivery (`adoptWorkingCopy`). It did not for this worker's first assignment — `@`'s author was
still the planner's review App — and for a stretch the pane's bash tool was unreachable while
the daemon was down, so tests and splits ran through the eval kernel, whose environment lacks
`JJ_USER`/`JJ_EMAIL`; jj then fell back to a user-scope config naming the implementer under a
*different* numeric email. Check `jj log -r @ -T 'author.email()'` before the first split; if it
is not the pane's leased identity, `jj metaedit --update-author -r @` from the pane shell (the
adoption step itself), never `jj config set`. And run credentialed commands from the pane's bash,
not a subprocess: the grant file is the pane's, minted per bash call, and a kernel-side `legion
gh` sees the previous call's grant or none.

## A CI flake is filed, not silently re-run (LEGION-174)

The `test` job failed once at a corrective head on `real-deployment-instructions-e2e.test.ts`
(`tmux window ownership marker failed: server exited unexpectedly` in `spawnController`, 160 ms
in) — a real-tmux LEGION-16 test none of the branch's commits touched. The right sequence: read
the job log (`legion gh -- run view <run> --job <job> --log-failed`; the `/actions/jobs/<id>/logs`
API answer carries escape sequences the shim refuses), reproduce locally under CI's exact flags
(`LEGION_E2E=1 LEGION_TMUX_LIVE=1 bun test` in `packages/daemon` — 3/3 on the file, 1654/1654 on
the suite), confirm `main`'s same job was green, re-run only the failed job (`legion gh -- run
rerun <run> --failed`, no commit — a push onto a red verdict would count as a fix attempt), record
the flake and the evidence in the pull request's CI line, and **file it** (LEGION-174) so the
pattern is visible the second time. A rerun with no record is how a flake becomes folklore.

## Non-blocking review items land in the round, not in a follow-up

The reviewer named six no-behaviour nits (a comment that overstated a cycle argument, a doc
phrase one word too wide, two duplicate tests, a tuple beside a union, a regex retyped in its own
message) and offered them as one fast-follow. They landed as one separate commit in the same
corrective push — the round already had a blocking fix to test and a rebase to do, and each nit
was a sentence or a deletion. What did *not* land is recorded as declined with the reason (the
role-neutral rewording of a string `main` already answers). The items that could not land in
the round (LEGION-175: the reviewer's six round-2 no-behaviour nits) are filed and the issue is
named on the `Fast-follow:` line instead of the items being listed there.
