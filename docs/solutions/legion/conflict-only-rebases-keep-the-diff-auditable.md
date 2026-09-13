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
  - "LEGION-42"
  - "sjawhar/legion#1021"
symptoms:
  - "GitHub reports the PR CONFLICTING / DIRTY again while the previous rebase's CI is still queued"
  - "A tester's evidence was gathered at a head that no longer exists after a rebase"
  - "jj diff --from main@origin shows deletions of files the PR never touched"
  - "GitHub reports MERGEABLE but the merge-commit CI is red on tests the branch does not contain"
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

## MERGEABLE is not "compatible": a red merge commit whose failing lines live only on main is a conflict in effect

Sami's rule (2026-09-11) is "no unnecessary rebases (i.e. unless there are merge conflicts)".
LEGION-42 (#1021) met a case GitHub's mergeability check cannot see. The branch **tightened a
contract**: `loadGitHubApps` began refusing a `legion.yaml` without both GitHub Apps. Between
the reviewer's read and the corrective push, `main` took #1016, which added `config.test.ts` and
`cli/__tests__` cases that build a config with only the implement App — legal on `main`, refused
by the branch. No line overlapped, so GitHub said `MERGEABLE`; but the merge commit CI runs —
`main` + branch — failed its `test` job on those four cases with `github_apps is required` /
`github_apps.review is required`.

How to tell it apart from a CI refresh or a flake, in order:

1. **The failing test names are not in your tree.** `grep` the branch for the test titles CI
   printed; `jj file show -r main@origin <file> | grep` finds them. Failing lines that exist only
   on `main` cannot be fixed by any commit on the branch alone.
2. **`main` moved since the last green merge commit.** `jj log -r 'fork_point(main@origin | @-)..main@origin'`
   names the commit that introduced them.
3. **Re-running the job would fail again** — the merge commit is deterministic. `run rerun
   --failed` is for a flake, not for this.

That is a conflict in effect: the merge result is broken even though no hunk collided. The
remedy is the skill's conflict-forced rebase — record the fingerprint at the current tip, rebase
the whole chain, adapt `main`'s fixtures to the tightened contract (here: point the four new cases
at the branch's own shared `BOTH_APPS`/`resolveWithApps`/`bothAppsYaml` fixtures, one commit,
`test(daemon): main's LEGION-46 config fixtures carry both Apps`), run the merged tree's gates,
push, fingerprint again, and post the rebase comment with both SHAs and both hashes. Tell the
architect before rebasing and say why the MERGEABLE read does not apply.

The fingerprint **changes** in this case — the adaptation adds executable test lines — and that
is the honest result: it means a full tester round on the new head, not the bare-gate re-check an
unchanged hash earns. Contrast the same PR's earlier textual rebase (`4e67eb28 → a49929e1`,
two adjacent-insertion conflicts, both sides kept), whose fingerprint was byte-identical and
earned bare gates only. Report which of the two you did; the tester's next step depends on it.

Two habits that make this cheap. When a contract tightens, consolidate the fixtures that
construct the old shape into one constant or helper *in the same PR*, so `main`'s new cases are a
one-line follow-through each. And after any push on a tightened-contract branch, read the
**merge-commit** CI, not just `mergeable`: a `MERGEABLE` PR with a red `test` job at the head is
this pattern until proven a flake.

## Related

- `one-role-keyed-table-decides-which-github-app-acts.md` — the tightened contract (#1021) that
  produced the MERGEABLE-but-red case above.
- `../github/conflicting-pr-gets-no-pull-request-ci.md` — why a conflicting PR gets no CI and
  the `pr view --json mergeable,mergeStateStatus` check.
- `unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md` — the `legion-worker` skill's
  hashed form of this identity check (LEGION-59), the jj fileset-union trap in it, and the case
  where a merged-prose resolution changes the hash with no code change.
- `handoff-file-conflicts-during-rebases.md` — the bottom-up edit-and-squash procedure for
  `.legion/` files.
- `../daemon/api-read-by-closure-before-assignment-boot-order-audit.md` — the change these
  rebases carried, and why the block's final position was forced.
