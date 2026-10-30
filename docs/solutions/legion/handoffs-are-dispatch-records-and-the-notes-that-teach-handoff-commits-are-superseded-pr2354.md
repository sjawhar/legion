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
  - docs/solutions/legion/a-branch-whose-every-round-pushes-never-settles-a-required-check-pr2512.md
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
   retro. Supersede, in a new note, every note whose rule depends on the mechanism. Leave a note
   alone when its rule stands and the mechanism is only mentioned in it. A census line that calls
   such notes "historical, kept" is not enough, because a worker follows a note's steps as written.

## Notes that mention the mechanism and keep their rule

These still hold. Skip only the sentence about handoff pushes:
- `docs/solutions/legion/the-private-names-check-reads-each-events-frozen-body-….md`,
  its handoff half;
- `docs/solutions/legion/a-forward-merge-after-retro-reinstalls-….md`, its retro
  removal step;
- `docs/solutions/legion/a-description-edit-clears-pr-title-but-not-lint-….md`;
- `docs/solutions/github/zero-check-runs-on-a-pushed-head-is-a-conflicting-head-not-a-trailer-pr2504.md`,
  its handoff-push clauses, corrected in place.

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
