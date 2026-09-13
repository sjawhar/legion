---
title: "The unchanged-diff fingerprint: one jj fileset (two negations union to everything), verified by a pair of heads that must hash equal, and what a conflict-forced rebase may change without a code change"
category: legion
tags:
  - jj
  - fileset
  - rebase
  - fingerprint
  - review
  - long-lived-pr
date: 2026-09-13
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-59"
  - "sjawhar/legion#998"
symptoms:
  - "jj diff … '~.legion' '~docs/solutions' still lists .legion/ and docs/solutions/ files"
  - "two heads that differ only by .legion/ files produce different fingerprints"
  - "a conflict-forced rebase changed the fingerprint although no code line changed"
---

# The unchanged-diff fingerprint: one fileset, verified by a pair, and what a rebase may change

## Context

LEGION-59 (PR #998) added `## The unchanged-diff check` to the `legion-worker` skill: a hash of
the branch's added and removed lines against its fork point, excluding the handoff ledger and
retro's learnings, that lets each role decide whether a conflict-forced rebase is a confirmation
(fingerprint equal → tester bare gates, reviewer approves the new head by SHA, merger republishes)
or a round (fingerprint different → the full review restarts). The check itself was wrong in the
plan and in the first push, and the second of this PR's own rebases showed a way the fingerprint
changes with no code change. Both are recorded here because every worker will run this command.

## 1. Two positional filesets union; the exclusion needs one expression

The plan and round 1 wrote the exclusions as two arguments:

```sh
jj diff --from "fork_point(main@origin | <head>)" --to <head> --git --context 0 '~.legion' '~docs/solutions'
```

jj **unions** positional filesets, and `~.legion | ~docs/solutions` is every file — nothing is
excluded. Reviewer round 1 (thread 3998885581) showed two heads that differed only by
`.legion/` hashing differently (`c88658b3…` vs `18e62757…`); the "changed" verdict would have
fired on exactly the rebases the rule exists to confirm. The correct form is one expression:

```sh
jj diff --from "fork_point(main@origin | <head>)" --to <head> --git --context 0 '~(.legion | docs/solutions)' \
  | sed -e '/^@@/d' -e '/^index /d' | sha256sum
```

(`'~.legion & ~docs/solutions'` is equivalent.) The same trap applies to `--summary` and
`file list`: a single negated path as the *only* argument is fine (the merger's
`--summary '~docs/solutions'`), two side by side are not.

**Why it got through.** The plan said "every flag verified on jj 0.45.1" — and every flag *was*
accepted. Acceptance of a flag is not verification of a check. A check whose whole point is
"these two things must be equal / must differ" is verified only against a known pair:

- a **positive pair** that must hash equal (here: the code head and the same head plus a
  `.legion/*.json` — with the fix, both `f06ac0c1…`), and
- a **negative pair** that must differ (the code head and a head with one changed code line).

Run both before writing the command into a skill, a plan, or a PR body; quote the two hashes.
The tester's read-check greps could not catch this — the text was exactly what the plan asked
for — only executing the command against a pair could.

## 2. What a conflict-forced rebase may change in the fingerprint

The fingerprint is the set of added/removed lines against the *new* base, so a rebase can change
it without anyone editing a line of the branch:

- **Unchanged (LEGION-59 rebase 1, `3d599ade → cdc4e3c9`, both `fa03a9f2…`).** `main` had
  rewritten a table row *next to* one this PR edited (`skills/legion-architect/SKILL.md`,
  `pr-blocked` vs `pr-review`); the conflict was resolved by keeping both rows verbatim. Every
  added/removed line of the branch survived byte-identical; the check confirmed it.
- **Changed with no code change (rebase 2, `018525d4 → 2f9036ab`, `32f4d0b3… → d89c1239…`).**
  #1003 had inserted new sentences *inside* two paragraphs this PR rewrites (the worker skill's
  merger bullet, `merger.md`'s opening paragraph and step list). The only honest resolution keeps
  both sides — `main`'s new clause folded into this PR's rewritten paragraph — and that resolved
  paragraph is a *new* added line against the new base. Eight of ten files were identical;
  two differed by exactly the merged sentences.

The rule is the rule: a different fingerprint is a round, not a confirmation, even when the
delta is prose. What the implementer owes the other roles in that case is attribution — run the
fingerprint per file (split the `--git` output on `diff --git` and hash each file's lines) and
name the files and the sentences that differ in the rebase comment, so the tester and reviewer
can scope their round to those files instead of re-verifying everything. In LEGION-59 round 4
was already a corrective round, so nothing was lost; on a post-approval rebase this is the
difference between a confirmation and a full re-review, and the merged-prose case is common on
a fast `main` that edits the same skills.

## 3. Procedure that survived four rounds and two rebases

1. Before rebasing: `jj git fetch`, then hash the current tip with the one-fileset command and
   keep the number.
2. `jj rebase -s 'roots(main@origin..@)' -d main@origin` — the whole chain, so the tester's and
   reviewer's local handoff commits move with yours (the review App cannot push; the
   implementer's push carries them).
3. Resolve each conflicted commit at the commit that owns the file (`jj new <rev>`, edit the
   file to the resolved text, `jj squash`), first conflicted commit first; the descendants
   re-apply and often clear several commits at once. A later commit that rewrote the same
   paragraph will conflict again — resolve it the same way. Never `jj op restore` in a shared
   workspace.
4. `jj new <tip>`, fold any stray empty working-copy commit the resolution left behind, run the
   package's tests, lint, and typecheck at the tip, re-run every read-check grep.
5. Hash the new tip; post one PR comment (Legion footer) in the form
   `rebase <old> → <new>; fingerprint <before> → <after>; unchanged|changed`, with the per-file
   attribution when changed.
6. Bookmark with `-r @- --allow-backwards`, push, re-read `mergeable,mergeStateStatus` — `main`
   can move again while CI queues (rebase 2 here was reported `CONFLICTING` within a minute of
   the round-4 push).

## Related

- `conflict-only-rebases-keep-the-diff-auditable.md` — the added/removed-line identity check
  this fingerprint mechanises, and the rules for what a resolution may contain.
- `handoff-file-conflicts-during-rebases.md` — the bottom-up edit-and-squash procedure for
  `.legion/` files.
- `../github/conflicting-pr-gets-no-pull-request-ci.md` — why a `CONFLICTING` PR gets no CI
  and no wake announces it.
