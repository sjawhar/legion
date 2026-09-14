---
title: "A sibling PR rewrites the function you are editing: spell both post-merge shapes in the plan, select the base at task 0, and expect the conflict-forced rebase to change the fingerprint"
category: legion
tags:
  - jj
  - rebase
  - conflict-resolution
  - planning
  - fingerprint
  - shared-workspace
  - end-game
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-79"
  - "sjawhar/legion#1037"
  - "LEGION-57"
  - "sjawhar/legion#1024"
symptoms:
  - "Two open issues edit the same two reducer functions; whichever lands second will conflict"
  - "GitHub flips the PR to CONFLICTING between the reviewer's approval and the .legion/ deletion push"
  - "The unchanged-diff fingerprint differs after a conflict-forced rebase even though the resolution was mechanical"
  - "jj shows a bullet main edited as plain context on your side of an AGENTS.md conflict"
---

# A sibling PR rewrites the function you are editing

## Context

LEGION-79 (`dequeue`: a waiting issue that leaves the line leaves the admission queue) and
LEGION-57 (#1024: a released child stays in its parent's tree) both rewrote
`reduceIssueUpdated` and `reduceIssueClosed` in `packages/daemon/src/daemon/reducers.ts`. A
textual conflict was certain whichever landed first. #1024 was `OPEN` when LEGION-79's
implementer started and merged to `main` (`1c749943`, 15:52Z) between the reviewer's round-1
approval and the end-game `.legion/` deletion push. The rebase took one command and one test
file; this document records why it was that cheap and what the fingerprint rule means when the
resolution has to change executable lines.

## 1. The plan carries both target shapes before the conflict exists

The planner wrote task 0 — *base selection, no code* — and two versions of the reducer step:

- **Task 0.** `jj git fetch`; `legion gh -- pr view 1024 --json state,mergedAt,mergeCommit`. If
  `MERGED`, move the plan handoff onto `main` **before writing any code** and implement against
  the post-merge shape; if `OPEN`, implement on the current base and rebase only when GitHub
  reports `CONFLICTING`. Choosing a base before a pull request exists is not the rebase Sami's
  rule forbids ("Please don't do unnecessary rebases (i.e. unless there are merge conflicts)",
  2026-09-11); the architect agreed and the spec's *New since we talked* records it.
- **Task 1, step 8a (`shapeOnCurrentMain`)** and **step 8c (`shapeAfterLegion57`)**: the exact
  code for each base. 8c names LEGION-57's `todo` line (`return admitOnTodo(state, node)` —
  keep it), its `own`/`result` body in `reduceIssueClosed`, and the single expression to change:

  ```ts
  const result: Effect[] =
    own?.status === "active"
      ? [{ kind: "linger", tree: issue.key }]
      : dequeueIfWaiting(state, issue.key, issue.status);
  ```

  plus the named check: `bun test packages/daemon/src/daemon/__tests__/reducers.test.ts`.

When the conflict arrived, resolution was reading `main`'s copy of the function
(`jj file show -r main@origin packages/daemon/src/daemon/reducers.ts`), confirming it matched
the shape 8c described, and typing 8c's expression into it. No design happened at rebase time,
and the reviewer's round 2 verified the resolution against the plan rather than re-deriving it.

**Rule.** When `knives`/`legion gh -- pr list` shows another open PR touching the function your
plan edits, the planner spells out the resolved shape for *both* landing orders as plan steps,
with the exact expression that differs and the one test file that proves it. The concern goes
in the plan's `concerns` and the spec's *New since we talked*, agreed with the architect, so
the reviewer can hold the resolution to a written target.

## 2. The rebase itself

```sh
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch
jj -R "$LEGION_WORKSPACE" rebase -s 'roots(main@origin..@)' -d main@origin   # the whole chain
```

The chain included the tester's and reviewer's local handoff commits (the review App cannot
push, so they ride on the implementer's next push) and an undescribed working-copy commit. In
this historical rebase, before LEGION-58, that commit held `.omp/config.yml`. Two files
conflicted; resolve each in the working copy, then squash into the commit that owns the file so
the descendants re-apply:

```sh
jj -R "$LEGION_WORKSPACE" squash --into <task-1 change id> packages/daemon/src/daemon/reducers.ts
jj -R "$LEGION_WORKSPACE" squash --into <docs change id>   packages/daemon/src/daemon/AGENTS.md
```

Run only the plan's named test file (`reducers.test.ts`: 103 pass), set the bookmark on `@-`
with `--allow-backwards` (it sits on `@` after every split), push, and read
`legion gh -- pr view <n> --json mergeable,mergeStateStatus` again — GitHub recomputes lazily,
so give it a few seconds.

## 3. Reading jj's conflict display when one side is a diff

jj renders a two-sided conflict as *one side's text* plus a `%%%%%%% diff from … to …` hunk for
the other side. Two traps from `AGENTS.md`:

- **Both sides edited the same single-line table row** (`processes.ts` in the Files table:
  LEGION-57 added `boot child-tree adoption (…)`, LEGION-79 added `the boot sweep of stale
  admission-queue entries (…)`). Neither side "wins"; the resolution is the union, with the
  phrases in the order the code runs. Assert the count of the substring you expect before
  writing (`s.count(old) == 1`) so you do not silently drop a phrase.
- **A bullet `main` rewrote appears as unchanged context (` ` prefix) on *your* side of the
  hunk.** The boot-ordering bullet gained `adoptOwnerlessChildTrees()` in #1024; jj showed
  LEGION-79's copy of that bullet as context below my two added bullets, so a resolution that
  "kept my side" would have reverted `main`'s edit. Diff the two versions of any context line
  that is long enough to hide an edit (`difflib`, or `jj file show -r main@origin` and compare)
  and take `main`'s.

## 4. The fingerprint changes, and that is correct

The `legion-worker` skill's unchanged-diff fingerprint is the set of added and removed lines
outside `.legion/` and `docs/solutions/`. A resolution that changes an executable expression —
here `dequeueIfWaiting` moving from two early returns into one initialiser, and the doc comment
above it — *must* change that set:

```
ceb8021ed29bd0c1ac1b409a692c46ec7b39563fc395ad422002d914c90ac6ab   (b87ab6f9, pre-rebase)
1f1af80c44aebc94e20dca1c8e2884b776b6261f9fc5317dbab17716fbf3042f   (30857329, post-rebase)
```

Do not contort the resolution to keep the hash equal. Instead prove the change is *only* the
predicted sites with an interdiff of the two fingerprint inputs:

```sh
jj diff --from "fork_point(main@origin | <old>)" --to <old> --git --context 0 '~(.legion | docs/solutions)' \
  | sed -e '/^@@/d' -e '/^index /d' > /tmp/fp-before.diff
jj diff --from "fork_point(main@origin | <new>)" --to <new> --git --context 0 '~(.legion | docs/solutions)' \
  | sed -e '/^@@/d' -e '/^index /d' > /tmp/fp-after.diff
diff /tmp/fp-before.diff /tmp/fp-after.diff | grep -E '^[<>] (diff --git|[-+])'
```

On LEGION-79 that printed exactly the `reduceIssueClosed` hunk and the one `AGENTS.md` row.
Quote it in the skill's `rebase <old> → <new>; fingerprint <a> → <b>; changed` PR comment and
in the completion summary. `changed` means a full test round — the architect routes tester
then reviewer again (LEGION-79 round 2: tester at `f328eb57`, reviewer `COMMENT` 5191364309,
then the deletion push and `APPROVE` 5191380595 at `a6b38398`). A "changed" fingerprint with an
interdiff confined to the plan's named sites is the expected outcome, not a defect.

## 5. The end-game is a two-branch step

The architect's end-game assignment reads mergeability *first*:

1. `legion gh -- pr view <n> --json mergeable,mergeStateStatus`.
2. `CONFLICTING` → do **not** push the `.legion/` deletion. Rebase as above, post the
   fingerprint comment, push, `legion handoff complete` with both fingerprints. The architect
   routes the next round.
3. `MERGEABLE` → `legion threads resolve --pr <n> --repo <owner>/<repo>` (expect `No unresolved
   threads` when the review was clean), `rm -r .legion`, `jj split -m "chore: remove .legion/
   handoffs after clean review (<KEY>)" .legion`, bookmark on `@-`, push, and confirm
   `jj diff --from <reviewed head> --to <new head> --summary` prints only `D .legion/…` lines.

LEGION-79 took branch 2 on its first end-game (the sibling had merged in the 40 minutes since
approval) and branch 3 on the second. Reading `mergeable` before acting is what kept the
approved head clean; a deletion pushed onto a conflicting head would have produced no CI run
and a rebase on top of it.

## 6. `jj split` keeps the change id on the *selected* half

After your push, `@` is the undescribed working-copy commit where the next role's jj commands
write. `jj split -m … <path>` gives the **selected** paths the original change id and mints a new
one for the remainder — so the tester's `test: record handoff (round 2)` commit carried what had
been *my* `@` change id (`oqosmssm`). Nothing is wrong; track the chain by commit id and
`jj diff -r <commit> --summary`, never by "my change id". Before every push, confirm `@` is clean
and `@-` contains exactly the paths the commit describes.

## Related

- [conflict-only-rebases-keep-the-diff-auditable](conflict-only-rebases-keep-the-diff-auditable.md)
  — what a conflict-only resolution may and may not change; the case where the fingerprint
  *should* stay equal.
- [unchanged-diff-fingerprint-one-fileset-verified-by-a-pair](unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md)
  — the fingerprint command itself.
- [completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased](completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased.md)
  — only the *active* implementer rebases; a finished phase reports.
- `docs/solutions/daemon/two-documented-models-one-predicate-and-what-a-removed-tree-leaves-behind.md`
  — LEGION-57's side of the same two functions.
