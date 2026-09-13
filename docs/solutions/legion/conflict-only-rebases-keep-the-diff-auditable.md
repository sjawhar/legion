---
title: "Four rebases on a fast-moving main: the added/removed-line identity check that keeps a moved block auditable, and what a conflict-only rebase may and may not change"
category: legion
tags:
  - jj
  - rebase
  - conflict-resolution
  - diff-identity
  - long-lived-pr
  - review
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-11"
  - "sjawhar/legion#955"
  - "sjawhar/legion#962"
  - "sjawhar/legion#980"
symptoms:
  - "GitHub reports the PR CONFLICTING / DIRTY again while the previous rebase's CI is still queued"
  - "A tester's evidence was gathered at a head that no longer exists after a rebase"
  - "jj diff --from main@origin shows deletions of files the PR never touched"
---

# Conflict-Only Rebases Keep the Diff Auditable

## Context

PR #955 moved one block in `startDaemonLocked` and added one regression test. Over 21 hours
`main` took #947, #954, #962 (runtime boundary), #973, #980 (launch hold), and a dozen release
commits; GitHub flagged the PR `CONFLICTING` four times, each time suppressing `pull_request` CI
(`../github/conflicting-pr-gets-no-pull-request-ci.md`). Each rebase had to leave the reviewed
change *provably* unchanged, because the tester's scratch-daemon evidence had been gathered at
an earlier head and re-running it costs an hour.

## The identity check

The tested diff and the rebased diff are compared as *sets of added and removed lines*, not as
unified diffs — context lines legitimately change on every rebase:

```sh
jj diff --from <old base> --to <tested code head> --git \
  | grep -E '^[+-]' | grep -vE '^(\+\+\+|---) ' > tested.pm
jj diff --from main@origin --to <new code head> --git \
  | grep -E '^[+-]' | grep -vE '^(\+\+\+|---) ' > rebased.pm
diff tested.pm rebased.pm && echo IDENTICAL
```

Run it per file too, so a difference is attributed at once. Report exactly one of:

- **IDENTICAL** — every added and removed line matches; the tester's evidence stands verbatim.
- **Not identical, no executable line differs** — list every differing line and which file it is
  in, classify each as comment / doc / executable. The tester re-runs only what the executable
  differences touch (LEGION-11 rounds 3–4: one added `runtime: "tmux"` line in the test, and
  comment text; the tester re-ran the hand check anyway because the base had gained #980).
- **An executable line differs** — stop; that is a code change wearing a rebase's clothes, and
  the architect decides.

Two traps the check exposes. First, always diff against the true merge base (`heads(::branch &
::main@origin)`), never a `main@origin` that has moved past it: on the last day `main@origin`
carried an unrelated feature the branch had never seen, and a diff against it showed that
feature's files as *deletions* by this PR. Second, GitHub recomputes mergeability lazily — a
push that lands `MERGEABLE` can read `mergeable: null` for a minute and then `false` if `main`
moved again; read `mergeable,mergeStateStatus` after the recompute, and check whether `main`
moved before deciding a conflict is new.

## What a conflict-only rebase may change

`jj rebase -s 'roots(main@origin..@)' -d main@origin` moves the whole chain — plan doc, plan
handoff, fix, comment sweep, every later handoff — so the push carries other roles' unpushed
commits (the review App cannot push). Resolve inside the working copy and `jj squash --from @
--into <owning commit> <paths>` so each resolution lands in the commit that owns the file; the
descendants re-apply. The rules for what the resolution may contain:

1. **Executable lines: none.** If `main` moved code around the block, the block goes where
   `main`'s own comments say its neighbours belong (see the boot-order note: placement before
   the launch hold was forced by #980's stated intent, not chosen).
2. **Comments inside the moved block: take `main`'s wording.** `main` had edited the comments at
   the block's *old* position (`enableWorkerPromotion()` → `enableLaunches()`, launch-hold
   sentences); carry those edits into the moved copy so the block reads as if it had always
   lived at the new position.
3. **A comment-only sweep that `main` has since rewritten: drop it.** Twice, `main` rewrote the
   exact paragraphs the sweep had edited (`processes.ts`, `worker-admission.ts`) and the rewrite
   no longer stated the old order. The right resolution was `main`'s text, which took both files
   out of the diff entirely. Record that in the PR body's Files section or the body contradicts
   the diff — a reviewer will block on it.
4. **A test-harness adaptation forced by `main`'s type changes is a real change: one line,
   recorded line by line.** #962's discriminated `Locator` union made the seeded locator need
   `runtime: "tmux"`; without it `tsc` failed and `locatorHandles` returned `undefined` at
   boot. It is executable, so it went into the "not identical" report, not into a claim of
   identity.
5. **Docs `main` wrote about your own PR: update them.** #980's `AGENTS.md` bullet listed the
   pre-fix order and said in so many words that moving the reconnect "is LEGION-11's fix, which
   rebases over this restructure" — an explicit hand-off. A doc that prescribes the order the PR
   removes is a blocking finding (the launch-hold retro's pattern block was missed by the sweep
   and caught by the reviewer); grep `docs/solutions` and `AGENTS.md` for the old sequence
   after every rebase, not only `*.ts` comments.

## Waiting for CI across a moving main

Never complete a phase on a head that has not produced a green run: a `CONFLICTING` head
produces none. Poll `mergeable`/`mergeStateStatus` and the run list together; if `main` moves
while the run is queued and GitHub flips to `DIRTY`, do the next rebase in the same phase rather
than reporting completion (LEGION-11 rebases 3 and 4 were one assignment). Prefer `legion gh --
run list --branch … --json` with a grant over unauthenticated `api.github.com` polling — the
shared IP rate-limits the latter within minutes, and a `null` body looks like "no run yet".

## Related

- `../github/conflicting-pr-gets-no-pull-request-ci.md` — why a conflicting PR gets no CI and
  the `pr view --json mergeable,mergeStateStatus` check.
- `handoff-file-conflicts-during-rebases.md` — the bottom-up edit-and-squash procedure for
  `.legion/` files.
- `../daemon/api-read-by-closure-before-assignment-boot-order-audit.md` — the change these
  rebases carried, and why the block's final position was forced.
