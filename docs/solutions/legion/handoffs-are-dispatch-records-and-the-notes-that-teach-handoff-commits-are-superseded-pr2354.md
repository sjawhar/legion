---
title: "Handoffs are Dispatch records, and the notes that teach handoff commits are superseded: when a change removes a mechanism, supersede every solutions note whose rule depends on it"
category: legion
tags:
  - handoff
  - dispatch
  - skip-checks
  - docs-solutions
  - supersedes
date: 2026-10-28
status: active
module: skills/legion-worker
supersedes:
  - docs/solutions/legion/a-handoff-only-push-runs-no-check-so-check-private-names-over-a-handoff-fires-only-at-the-next-code-push-LEGION-698.md
  - docs/solutions/legion/a-round-that-changes-no-code-writes-its-handoff-again-so-its-completion-keys-to-a-fresh-commit-and-strips-the-four-keys-the-cli-stamps-LEGION-629.md
  - docs/solutions/legion/a-trailered-head-is-missing-the-checks-a-push-starts-not-every-check-run-INFRA-149.md
  - docs/solutions/legion/an-implement-handoff-push-runs-ci-in-full-and-cancels-the-code-heads-runs-cite-the-tips-runs-and-give-the-lane-the-code-heads-image-LEGION-631.md
  - docs/solutions/legion/lint-the-handoffs-you-inherit-before-your-first-code-push-a-redaction-commit-answers-another-roles-private-name-LEGION-791.md
  - docs/solutions/legion/handoff-file-conflicts-during-rebases.md
  - docs/solutions/legion/handoff-complete-checks-the-handoff-commits-app-and-nothing-checks-a-code-commits-INFRA-92.md
  - docs/solutions/legion/a-solutions-note-cannot-cite-a-legion-handoff-because-retro-removes-it-before-the-merge-INFRA-69.md
  - docs/solutions/skill-patterns/workflow-ordering-and-stale-workspace-data.md
  - docs/solutions/legion/tree-recovers-from-lost-wakes-by-reading-state-and-handoffs.md
---

# Handoffs are Dispatch records, and the notes that teach handoff commits are superseded

The superseded notes each give steps for a committed `.legion/<issue>/<phase>.json`, a
handoff-only push or its `skip-checks: true` trailer. None of those exists after sjawhar/legion#2354.

## The rule

1. A phase's handoff is a record on its Dispatch issue. Write the body to a file outside the
   workspace, record it with `dispatch handoff-write --issue <KEY> --phase <word> --body-file <file>`,
   then call `handoff_complete`. Read a predecessor's with
   `dispatch handoff-read --issue <KEY> --phase <word> --version <n>`, the version your task's
   `Handoff:` line names. No phase commits or pushes a handoff, and no push carries
   `skip-checks: true`.
2. A note under `docs/solutions/` that tells you to commit, lint, push, trailer or rebase around a
   handoff file is history. Where it disagrees with rule 1, rule 1 wins.
3. When your change removes a mechanism, grep `docs/solutions/` for the mechanism's vocabulary in
   retro. Supersede, in a new note, every note whose title or whole rule is the mechanism, and
   mark it `status: superseded`. Correct in place a note whose rule stands but whose step or
   present-tense sentence teaches the mechanism: that clause in the past tense, beside what holds
   now. Leave a note alone when it mentions the mechanism only as history. A census line that calls
   such notes "historical, kept" is not enough, because a worker follows a note's steps as written.

## Notes corrected in place, whose rule stands

These notes still hold. Each handoff clause in them is corrected in place: the removed mechanism
in the past tense, beside one sentence that says what holds now:
- `docs/solutions/daemon/codebase-index-file-placement.md`, the table row that put a worker's
  handoff under `.legion/`;
- `docs/solutions/daemon/targeted-delta-filtering-pattern.md`, its `.legion/` rebase-conflict
  recipe;
- `docs/solutions/daemon/webhook-rule-across-listener-and-daemon.md`, the classifier that owned
  the path rule and the push that took a counted attempt back;
- `docs/solutions/daemon/workspace-scanning-patterns.md`, the marker its workspace scan reads;
- `docs/solutions/github/conflicting-pr-gets-no-pull-request-ci.md`, the shared workspace's
  handoff commits and the empty `@` a handoff split left;
- `docs/solutions/github/zero-check-runs-on-a-pushed-head-is-a-conflicting-head-not-a-trailer-pr2504.md`,
  its handoff-push clauses, corrected in place;
- `docs/solutions/legion/a-branch-whose-every-round-pushes-never-settles-a-required-check-pr2512.md`,
  the `.legion/`-only handoff commit that followed the code;
- `docs/solutions/legion/a-commit-made-outside-your-assignment-….md`, the App check
  `handoff_complete` makes;
- `docs/solutions/legion/a-conflict-that-arrives-while-another-role-holds-the-phase-….md`, the
  step that recreated `.legion/<KEY>/`;
- `docs/solutions/legion/a-decision-taken-between-rounds-….md`, the committed handoff a
  recreated pane reads;
- `docs/solutions/legion/a-decision-that-binds-a-later-phase-….md`, the plan handoff as
  `plan.json`, and the phase commits a branch carried;
- `docs/solutions/legion/a-description-edit-clears-pr-title-but-not-lint-….md`, its sentence
  about a handoff-only push;
- `docs/solutions/legion/a-forward-merge-after-retro-reinstalls-….md`, its retro removal step;
- `docs/solutions/legion/a-mass-re-layout-of-a-file-many-pull-requests-edit-is-a-script-and-each-merge-of-main-reruns-it-pr2422.md`,
  where a regeneration script lives once retro deletes nothing;
- `docs/solutions/legion/a-proof-pastes-every-commit-sha-….md`, the handoff push as the
  checkpoint that resolves each SHA;
- `docs/solutions/legion/a-proofs-round-key-follows-the-engines-rule-….md`, the harness helpers
  that keyed a handoff fact by round;
- `docs/solutions/legion/a-record-is-true-when-written-….md`, evidence linked into
  `.legion/<issue>/` and retro's commit scope;
- `docs/solutions/legion/a-retired-reviewer-cannot-see-a-workspace-only-commit-push-the-corrective-head-before-accepted.md`,
  the round's handoff file, the deletion head it approved, and the `handoff_write` refusals;
- `docs/solutions/legion/architect-acts-on-the-phase-complete-wake-not-the-artifact-appearing.md`,
  the planner's handoff push in the 409 sequence;
- `docs/solutions/legion/conflict-only-rebases-keep-the-diff-auditable.md`, the empty `@` a
  handoff split left and its cross-reference;
- `docs/solutions/legion/eleven-passes-on-one-pull-request-a-shared-working-copy-three-heads-and-three-credential-shapes.md`,
  the merger's diff of retro's removal;
- `docs/solutions/legion/fast-follow-pr-mechanics-and-queue-registration.md`, the handoff file on
  the follow-up branch and the head-moved follow-up line;
- `docs/solutions/legion/fixing-a-defect-that-is-live-in-your-own-pane.md`, the round order that
  ended at the `.legion/` deletion;
- `docs/solutions/legion/gh-run-list-commit-takes-the-full-sha-….md`, the handoff-only head and
  the daemon's verdict carry;
- `docs/solutions/legion/mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md`, the
  handoff of the rebased push;
- `docs/solutions/legion/never-jj-split-a-merge-commit-write-the-next-file-into-a-jj-new-child-pr2529.md`,
  the handoff as the file a merge meets;
- `docs/solutions/legion/rebasing-a-branch-across-a-refactor-of-its-own-call-sites.md`, the
  handoff commits above the chain;
- `docs/solutions/legion/retro-does-not-freeze-a-regeneration-script-a-refused-ready-can-need-one-more-anchor-pr2422.md`,
  the script's return in the round's handoff push;
- `docs/solutions/legion/review-thread-replies-fan-out-pr-review-wakes.md`, the clean round's
  `commented` verdict;
- `docs/solutions/legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md`, the
  renumber step that named handoff files;
- `docs/solutions/legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md`,
  the plan handoff the base selection moved and the end-game deletion push;
- `docs/solutions/legion/stacked-base-and-review-folded-into-rebase.md`, the duplicate's handoff
  commits and the second handoff a 409 must not produce;
- `docs/solutions/legion/template-names-a-ci-check-the-repository-never-produces.md`, the
  predecessor read from `.legion/` and the cleanup push;
- `docs/solutions/legion/text-only-skill-pr-mechanics.md`, the handoff schema and the approval of
  the deletion head;
- `docs/solutions/legion/the-merge-queues-adversarial-deep-review-can-fail-a-ready-head-the-corrective-round-after-approval.md`,
  the post-approval shape of the branch;
- `docs/solutions/legion/the-private-names-check-reads-each-events-frozen-body-….md`, its handoff
  half, and its title;
- `docs/solutions/legion/the-record-is-not-the-reader-….md`, the grep over the handoff files;
- `docs/solutions/legion/the-trailer-on-a-pushs-tip-suppresses-the-runs-of-every-code-commit-beneath-it-pr2542.md`,
  the inherited handoff trailer, the rewrite it guarded and the handoff-only cause of no runs;
- `docs/solutions/legion/when-runners-queue-longer-than-a-review-round-make-one-code-push-per-round-pr2422.md`,
  the handoff pushes that followed the round's one code push;
- `docs/solutions/testing/a-docker-free-rig-renders-the-real-authorization-….md`, where the rig
  lives;
- `docs/solutions/testing/fresh-state-dir-boot-beside-a-busy-rig.md`, the `.legion/`-only pushes
  in its CI-filter incident;
- `docs/solutions/testing/proving-a-refusals-absence-….md`, the deletion of the control files
  before the handoff commit.

## Evidence

- sjawhar/legion#2354:
  - `packages/pi-shared/src/handoff-record.ts` reports the record;
  - `packages/daemon/internal/api/handoff.go` `handoffRecord` reads it back and holds it to the
    claim's session;
  - `packages/daemon/internal/classify/push.go` is deleted;
  - `skills/legion-worker/references/skip-checks-trailer.md` is deleted.
- That pull request's contract change census marked the `docs/solutions` hits "historical
  learning, kept". At retro, a grep of `docs/solutions` for
  `skip-checks: true|handoff-only push|READY_HEAD_CARRIES_HANDOFFS` still matched 23 notes, 18 of
  them active with no superseding note.
