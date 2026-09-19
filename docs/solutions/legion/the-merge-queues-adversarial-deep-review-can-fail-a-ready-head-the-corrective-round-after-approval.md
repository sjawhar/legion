---
title: "The merge queue's adversarial deep review can fail a READY head: what the corrective round after approval looks like, and why its CI runs on a main that moved"
category: legion
tags:
  - merge-queue
  - deep-review
  - READY
  - corrective-round
  - review-round
  - merge-commit
  - logical-conflict
  - implementer
  - merger
date: 2026-09-18
status: active
module: skills/legion-worker (READY and post-approval mechanics)
related_issues:
  - "LEGION-105"
  - "sjawhar/legion#1187"
  - "LEGION-201"
  - "sjawhar/legion#1208"
symptoms:
  - "A pull request the reviewer approved by SHA and the merger posted READY for receives a `VERDICT: fail` review from sjawhar-agent[bot] naming findings the reviewer's two lenses did not"
  - "The implementer is revived for a corrective round on a branch whose `.legion/` is already deleted and whose retro docs are already committed"
  - "CI at the corrective head fails tests that pass locally under the exact CI recipe, twice, with the same timings"
  - "A test file appears in the CI job's ##[group] list that does not exist on the branch"
---

# The merge queue's adversarial deep review can fail a READY head

## What happened on #1187

The lifecycle ran to its documented end: tester green, reviewer approved the `.legion/` deletion
head `7db2b5db` by SHA, retro committed `docs/solutions/` at `ba7a1dc8`, the merger posted READY.
Then the merge queue ran its own review — **`sjawhar-agent[bot]`, model `openai/gpt-5.5`, a
thermonuclear deep review it runs on every pull request that changes daemon behaviour, whoever
authored or reviewed it** — and answered `VERDICT: fail for ba7a1dc8…` with one blocker and one
should-fix that the reviewer's two lenses (run twice, at two heads), the tester's independent
scratch daemon, and 31 unit titles had all passed over. Both findings were real
([`../daemon/a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md`](../daemon/a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md),
"Round 2").

Two facts about this gate that no skill stated before:

1. **It is not the reviewer's Thermo line.** The repository's "pair" gate is the review App's two
   lenses recorded in one review (`shared-main-repo-hazards-for-concurrent-issue-workspaces.md`).
   The queue's deep review is a third, independent read on a different model, and it happens
   *after* READY. A daemon-behaviour PR is not through the gate when the reviewer approves; it is
   through when the queue's verdict at the READY head is a pass.
2. **A posted READY names a head.** A failing verdict supersedes it, and every commit the
   corrective round adds above that head does too. The merger re-posts READY at the new tip, with
   a fresh `jj diff --from <approved sha> --to <tip> --summary`, and the queue re-verdicts there.
   Nothing about the earlier READY carries over.

## What the corrective round looks like above an approved, retro'd head

The branch is in the post-approval shape — `.legion/` gone, `docs/solutions/` committed — and the
round has to respect it:

- **Fixes go on top of the retro head.** Never recreate `.legion/`; write no handoff file; the TDD
  red runs, the re-run proof, and the mechanism chosen go into the PR body's `E2E (implementer)`
  paragraph (fetched live and patched by section — the tester's line is in it now). `legion
  handoff complete` alone reports the round, naming the new head.
- **Reply on the deep review as a PR comment**, one paragraph per finding naming the fixing
  commit, the red-then-green test titles, and — when the review offered alternatives ("fence the
  launch, or derive the resume from the retained locator") — which one you chose and why. The
  re-approving reviewer reads that reply against the delta and says so.
- **The reviewer re-approves by SHA** ("Approved: `83709677…` — supersedes my approval at
  `7db2b5db`"), the tester runs another round at that head, retro extends its documents with one
  more `docs/solutions/`-only commit, and only then does the merger re-post READY. The
  docs-only-above-approval rule still holds for that final commit.
- **The round's own proof has to reach the failure branch the review read.** Both prior proofs
  drove the success path; the finding lived on the `StopFailed` branch. The tester's
  `LEGION_TMUX_PATH` wrapper (fail `kill-pane` with a non-proving stderr while a flag file exists)
  is the reusable recipe:
  [`../testing/an-isolated-scratch-daemon-from-inside-a-legion-pane-what-it-proves-and-the-two-things-it-cannot-reach.md`](../testing/an-isolated-scratch-daemon-from-inside-a-legion-pane-what-it-proves-and-the-two-things-it-cannot-reach.md),
  "The third thing".

The gpt-5.5 review's method is worth copying before READY rather than waiting for it: it read
each fallible `await` inside the new retire, asked what state the *throw* leaves behind, and then
walked every downstream launch decision that reads that state (`admit`, `spawnRoot`,
`spawnTree`), with line numbers. An implementer can do that walk for their own primitive in ten
minutes; two review lenses and a tester did not, because all three were reading what the change
*does*, not what it leaves when its stop fails.

## The corrective push lands on a `main` that moved

A round after approval starts hours after the last green CI. `main` has moved, and the PR's CI
runs on GitHub's merge of the head into *current* `main` — so a corrective push can turn CI red
for a reason that is on neither the branch nor `main` alone.

On #1187 the corrective head `f95c3991` failed two real-tmux E2E tests, deterministically, twice
(`run rerun --failed` reproduced it exactly), while every local run passed: the branch's own
suite, the exact CI recipe (`LEGION_E2E=1 LEGION_TMUX_LIVE=1 bun test` in `packages/daemon`),
under CI's bun version, under a two-CPU affinity mask. The tell was in the job log, not the
failing test: the `##[group]<file>:` list of the red run contained
`src/daemon/__tests__/tmux-e2e-isolation.test.ts`, which did not exist on the branch. That file
came from #1208, which had landed on `main` that day and had rewritten the same E2E file the
branch had added its two tests to — minting a per-run tmux server name (`PROJECT =
realshutdown<uuid>`) and converting every *existing* test's seed from the literal `"realshutdown"`
to `PROJECT`. The branch's two new tests still seeded the literal. On the branch the two strings
were equal; on the merge they were not, and the fixture builds the manager's `TmuxRuntime` on
`legion-${state.project}` while the pane helpers open panes on `legion-${PROJECT}`: the daemon
probed a tmux server that did not exist, `no server running` is legitimately "the pane is gone"
(`PANE_GONE_STDERR`), the stop was silently skipped, the retire reported `root stopped`, and the
real pane survived the five-second poll. The daemon's contract was right; the test's seed was the
lie. LEGION-201 records the root cause in full (filed as a flake first — see below).

The general rules for a red merge commit are already written —
[`conflict-only-rebases-keep-the-diff-auditable.md`](./conflict-only-rebases-keep-the-diff-auditable.md)
("MERGEABLE is not compatible"),
[`mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md`](./mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md),
[`non-blocking-findings-are-fixed-in-the-round-and-small-commits-make-conflict-rebases-cheap.md`](./non-blocking-findings-are-fixed-in-the-round-and-small-commits-make-conflict-rebases-cheap.md)
— and they already say the two things this round paid to relearn: a deterministic merge-commit
failure is not a flake, and `run rerun --failed` is for a flake. What this round adds:

1. **Diff the two runs' file lists first.** `gh run view <red> --log | grep '##\[group\]src/'`
   against the last green run: a file present only in the red run is a `main` commit the branch
   has never seen, and it names the PR. Faster than reading the failing test, and it works when
   the failing test is one you wrote and cannot see anything wrong with.
2. **Reproduce on a scratch merge, never a rebase.** `jj new <head> main@origin` gives a working
   copy that is exactly what CI ran, with the branch and its bookmark untouched (Sami's rule:
   rebase only when GitHub says CONFLICTING, and it said MERGEABLE). Run the failing file there;
   apply the candidate fix there; watch it go green. Then `jj new <head>` and make the real commit
   on the branch. The scratch merge stays behind as an empty, described change — `jj abandon` is
   off limits in a shared workspace — so describe it as scratch when you create it.
3. **Fix the branch's copy the way `main` fixed its own.** The one-line repair was the same
   conversion #1208 applied to the file's other tests (`newLegionState(PROJECT, 1)`), so the
   merge result is what `main`'s author intended everywhere. The branch's remaining pre-#1208
   literals were left alone: `main`'s hunks convert those lines, and editing them on the branch
   would have manufactured the textual conflict GitHub had correctly said did not exist.
4. **A green local run is not proof against the merge when the file you touched was touched on
   `main` since your fork point.** `jj log -r 'fork_point(main@origin | @-)..main@origin' -T
   'description.first_line()'` before every corrective push; if it names a commit touching your
   test files, run those files on the scratch merge before pushing, not after CI.

The mistakes, so nobody repeats them: the implementer reran the failed job (a wasted CI cycle,
predicted by the docs above) and filed LEGION-201 as a flake with a "suggested diagnostics" plan
before doing step 1 — the root cause was found within minutes once the file lists were compared.
Read `docs/solutions/legion/*merge*` before touching `run rerun`.

## Related

- [`../daemon/a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md`](../daemon/a-kept-field-changes-every-gate-keyed-on-it-test-the-reopen-a-linger-change-exists-for.md)
  — the two findings the queue's review made and how they were fixed.
- [`shared-main-repo-hazards-for-concurrent-issue-workspaces.md`](./shared-main-repo-hazards-for-concurrent-issue-workspaces.md)
  — the reviewer's two-lens "pair" record, which this gate is *in addition to*.
- [`fast-follow-pr-mechanics-and-queue-registration.md`](./fast-follow-pr-mechanics-and-queue-registration.md)
  — what READY carries and how the queue learns a PR's scope.
- [`../github/release-bump-push-resolves-one-conflict-shape-and-routes-every-failure-through-one-sink.md`](../github/release-bump-push-resolves-one-conflict-shape-and-routes-every-failure-through-one-sink.md)
  §6 — a `pull_request` run tests the cached potential merge commit, and only a head push
  refreshes it.
