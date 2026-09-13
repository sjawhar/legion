---
title: "A stopgap that lands on main mid-plan is superseded, not merged: tell the architect before pushing, name it in the code and PR body; plus the 409 fallback and three pane mechanics"
category: legion
tags:
  - rebase
  - conflicting
  - supersession
  - fingerprint
  - handoff-complete-409
  - gh-pr-create
  - jj-bookmark
  - implementer
date: 2026-09-14
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-107"
  - "sjawhar/legion#1085"
  - "LEGION-103"
  - "LEGION-37"
symptoms:
  - "the first push of a planned change is CONFLICTING because a hotfix for the same symptom landed in the same function while the plan was written"
  - "`legion gh -- pr create` from $LEGION_WORKSPACE: `failed to run git: fatal: not a git repository: …/.git/worktrees/<issue>`"
  - "`jj bookmark set legion/<KEY> -r @-` refuses: `Refusing to move bookmark backwards or sideways`"
  - "the planner's `legion handoff complete` answers 409 `no longer owned by this worker`"
applies_when:
  - GitHub reports the PR CONFLICTING and the conflict is semantic (main fixed the same symptom another way), not textual
  - Opening a PR or moving the issue bookmark from a phase-worker pane
---

# A stopgap that lands on main mid-plan is superseded, not merged

## What happened on LEGION-107

The plan (07766f2a) was written against `main` at d35e8b59. By the time the implementation was
pushed, `main` had moved 45 commits, three of them into the files under change: LEGION-103
(#1053, #1057) put a stopgap for the *same incident* into `handleException` — ignore every
`delivery_failed` on an alive holder, cap `no_holder` republishes per token per 30 minutes — and
LEGION-15 (#1027) and #1050 rewrote the test fixture and the worker queue order. GitHub reported
#1085 `CONFLICTING` on its first push. The conflict-forced rebase is exactly what the skill
permits; the point of this note is what a *semantic* conflict adds to the procedure.

## The rule

1. **Read the conflict as a design question before a text one.** `jj file show -r main@origin
   <file>` for each conflicted file, then ask whether main's change and yours implement the same
   contract. LEGION-103's stopgap contradicted the LEGION-101 Design the plan was built on (once the
   listener says `receipt_timeout` for a late receipt, `delivery_failed` means never-forwarded and
   deserves the bounded re-send; a blanket ignore would drop the LEGION-29 lost-claim case the spec's
   Rejected list protects). Keeping both would have been wrong, not merely redundant.
2. **Supersede, and remove the whole stopgap** — constants, counters, helpers, and exactly its
   tests (`MAX_ROLE_REDELIVERIES`, `ROLE_REDELIVERY_PERIOD_MS`, `roleRedeliveries`,
   `noteLateReceipt`, `countRoleRedelivery`, `countRedelivery`, three tests). Then grep the
   repository for its names so no `AGENTS.md` or `docs/solutions/` text describes dead behaviour.
3. **Tell the architect before the push, with the reasoning and the deploy-window cost**, by
   `envoy_publish` to its role topic — this is a cross-issue decision even when the contract makes it
   clear. LEGION-107's architect answered within minutes (do not keep the guard on either side; two
   carries: no doc describes the old counters, and the PR body states the supersession).
4. **Name the supersession where a reader will look**: the replacing function's doc comment
   ("supersedes LEGION-103's daemon stopgap, which …, because …"), the PR body's "What changed", and
   the rebase comment. A reviewer seeing 150 deleted lines from another issue otherwise reads a lost
   merge. The reviewer's round-1 body confirmed each item by name.
5. **Expect the fingerprint to change and say so.** The unchanged-diff fingerprint
   (`conflict-only-rebases-keep-the-diff-auditable.md`) went `a4075899… → e9683443…`; the rebase
   comment carries both SHAs, both fingerprints, and "changed, on purpose" with the reason. A
   changed fingerprint before review is a fact to record, not a fault; after approval it would be a
   new round.
6. **Re-run the full package suite after the rebase**, not only the plan's named files: main's
   fixture refactor changed a wrapper that silently dropped a new argument
   (`../testing/model-the-receivers-dedupe-in-the-test-when-a-rig-is-forbidden.md`).
7. **Read `mergeable` again at retro time, before committing the docs.** LEGION-107 went
   `CONFLICTING` a second time between the reviewer's approval of `9bf634be` and the retro
   commit: LEGION-93 (#1082) landed in the same four daemon files, LEGION-84 (#1080) in two of
   them. Retro's brief said "report, do not rebase", so the retro worker stopped with the three
   documents written but uncommitted and asked. The architect's ordering: commit the docs above
   the approved head first (they ride the rebase — the skill's "a conflict-forced rebase after
   retro moves these documents with the branch"), then rebase the whole chain, resolve
   semantically, re-run the gates, fingerprint at the old approved head and at the rebased
   deletion commit (the new code head, *below* the docs), push at the docs commit, and say in one
   PR comment that the fingerprint changed and the tester and reviewer will re-run and re-approve
   the new head by SHA — so nobody reads the old approval as covering it. A rebase that resolves
   real conflicts after approval is a new review round, whatever it does to the diff.

## The 409 fallback, as it happened this time

The planner's `legion handoff complete` answered 409 `no longer owned by this worker` (the
LEGION-37 class); the branch carries two `plan: record handoff` commits (07766f2a, then 21a9003e
re-recording) and the planner reported completion by `envoy_publish` to the architect and stopped.
The implementer's and the review-round completions did not 409. Nothing new here beyond the
instance: the fallback is one message, then stop — never a retry loop, never a second handoff
file. Mechanics and history: `worker-pane-shell-gotchas.md` §11,
`tree-recovers-from-lost-wakes-by-reading-state-and-handoffs.md`,
`long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md` §3.

## Three pane mechanics

- **`legion gh -- pr create` fails from inside `$LEGION_WORKSPACE`** with `fatal: not a git
  repository: …/repos/github.com/sjawhar/legion/.git/worktrees/legion-107` — the workspace's
  `.git` file points at a worktree path that does not exist on this box, and `gh` consults it before
  using `--repo`. Run the command from a non-repository directory (`cd /tmp`) with `--repo
  <owner>/<repo> --head legion/<KEY> --base main`; the grant is the pane's, not the cwd's, so
  `legion gh` still authenticates. `pr view`, `pr edit`, `pr comment`, and `api` need the same.
- **Set the bookmark on `@-`, and expect `--allow-backwards` the first time.** A fresh issue
  workspace's bookmark sits on the working-copy commit `@`, which carries the stray untracked
  `.omp/config.yml`; after `jj split -m … <paths>` the implementation is `@-` and `bookmark set
  legion/<KEY> -r @-` refuses (`Refusing to move bookmark backwards or sideways`) until
  `--allow-backwards`. Every later `set -r @-` is a forward move and needs no flag. `jj git push
  --bookmark` refuses a bookmark on a commit with no description, which is the loud version of the
  same mistake. Always `jj split` with explicit paths so `.omp/config.yml` enters no commit (see
  `jj-bookmark-facts-verified-on-0-44-0-and-0-45-1.md` for why the bookmark starts on `@`).
- **`bun install` first.** A fresh issue workspace has no `node_modules`; the first `bun test`
  fails with `Cannot find module '@legion/contracts'` (also noted in
  `../daemon/omp-pin-bump-behavioral-proof.md`).

## Related

- `completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased.md` — the rebase
  permission is for the *active* implementer only; LEGION-107's rebase happened inside the
  implementer's own phase, before any other role was spawned.
- `conflict-only-rebases-keep-the-diff-auditable.md` — the fingerprint procedure.
- `sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md`
  — the same "main rewrote your function" hazard where both shapes were kept; this note is the
  case where one shape must win.
- `../daemon/re-send-chains-are-keyed-by-the-message-and-a-late-receipt-is-receipt-timeout-not-delivery-failed.md`
  — the technical reasoning for why LEGION-103's stopgap could not stay.
