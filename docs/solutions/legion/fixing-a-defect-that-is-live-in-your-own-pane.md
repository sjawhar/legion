---
title: "Fixing a defect that is live in your own pane: pin the identity per command, rewrite born-wrong commits before push, and take the docs fast-follow before the .legion deletion"
category: legion
tags:
  - process
  - jj
  - commit-identity
  - metaedit
  - review-rounds
  - fast-follow
  - conflict-rebase
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-44"
  - "sjawhar/legion#1025"
---

# Fixing a defect that is live in your own pane: pin the identity per command, rewrite born-wrong commits before push, and take the docs fast-follow before the `.legion` deletion

## Context

LEGION-44 fixed the shared-repo-config identity leak while running on a daemon that still had it.
The implementer's pane predated the fix and carried none of the six identity variables; every
other tree on the box was still writing its App into the one repo config. The defect bit the
branch fixing it twice, and the issue then went through one corrective round, one conflict-forced
rebase, one docs-only fast-follow, and the `.legion/` deletion. What each cost, and what to do
the next time a fix runs on infrastructure that has the bug.

## The defect bit the fix twice

Between the Task 1 and Task 2 commits a reviewer on another tree wrote `legion-reviewer[bot]`
into the shared repo config; three of this branch's commits were born committer
`legion-reviewer[bot]`. Later, after the reviewer committed its round-1 handoff in the shared
workspace, `@` carried committer `legion-reviewer[bot]` again for the corrective commits.

What worked, in order:

1. **Check before push, every time**, with the description in the template so you can tell your
   commits from other roles':
   `jj log -r 'main@origin..@' -T 'author.name() ++ " / " ++ committer.name() ++ " [" ++ description.first_line() ++ "]\n"'`.
2. **Rewrite the born-wrong commits before they leave the box**, under the right identity:
   `JJ_USER=… JJ_EMAIL=… jj metaedit --force-rewrite -r '<first>..@'` rewrites the committer of
   every commit in the range (author untouched, change ids kept, `Omp-Session` trailers kept —
   verify with `description.contains("Omp-Session: <id>")`). `--update-author` is the author-side
   twin. Neither exists on `jj describe` in the pinned builds (`--reset-author` is not a flag).
3. **Export the identity for every later jj command in the pane** (`export JJ_USER=… JJ_EMAIL=…`
   in each bash call, since the persistent shell can die mid-session): `JJ_USER`/`JJ_EMAIL` outrank
   every config scope, so the shared file can flip under you without effect. This is the
   per-command form of the fix itself, and it is exactly what a pane opened after the fix gets
   for free.

Do not pin with `--config user.name=…`: it also outranks the environment, and once the fix is
deployed it would put the wrong App back on a pane that has the right one.

## A rebased commit's committer is the rebaser's

`jj rebase` rewrites every commit it moves under the rebaser's identity: after the implementer's
conflict-forced rebase the reviewer's `review: record handoff` read author `legion-reviewer[bot]`
/ committer `legion-implementer[bot]`. That is jj, not a defect. Say so in the handoff and in the
rebase comment so the tester and reviewer do not report it, and check identity only on the
commits you made (the skill's pre-push check says exactly this since this issue).

## Conflict resolution from a scratch child moves `@`

`jj new <conflicted>` → edit → `jj squash --into <conflicted>` is the clean way to resolve a
conflict inside a rebased chain without `jj edit`. It leaves `@` on the (now empty) scratch
child of that commit, not on your working-copy commit at the tip; `legion handoff write` then
writes into the wrong place. Return with `jj edit <your own working-copy change id>` — the
undescribed working-copy commit, never a described commit or another role's — and the empty
scratch commit is abandoned automatically. Record the detour in the handoff.

## Take the docs fast-follow before the `.legion/` deletion

Round 2's review was clean pending the `.legion/` deletion and listed a docs-only fast-follow
(two items that would mislead readers: a pre-push identity check that did not say "on your own
commits", and a runbook calling a load-bearing scrub "belt and braces"). Landing it before the
deletion cost one tester bare-gate pass and one short reviewer re-check — the same price round
1's fast-follow paid — and left the PR with no follow-up. The alternative, a follow-up LEGION
issue for wording, is a deferral in disguise; Sami's rule is no deferrals. The order that works:
fast-follow commit → `legion threads resolve` (every accepted thread resolves in that push) →
push → tester bare gates → reviewer re-check → `.legion/` deletion → approval of that head → retro.

## Two fingerprints, one fileset — and `docs/solutions/` is shared surface

After the first conflict-forced rebase the skill's unchanged-diff fingerprint
(`'~(.legion | docs/solutions)'`, hunk headers and `index` lines stripped) was identical before and
after (`c7c1a48d…5899`), which is what let the tester run bare gates only and the reviewer
continue the round instead of restarting it. The one conflict was in `docs/solutions/`, outside
the fileset, so it could not have moved the hash — but say where the conflict was and what the
resolution kept from each side anyway; the reviewer verified it independently.

Two pull requests with disjoint code can still conflict through `docs/solutions/`: every retro
edits the same few hazard and runbook files, so a retro commit is shared surface even when the
fixes are not. This PR's retro touched the shared-repo hazards doc that LEGION-45 had just
extended on `main`, and the merge queue refused the `READY` as `CONFLICTING`; #1041 and #1042 hit
the same thing within ten minutes the same evening. Expect a rebase after retro when another tree
retro'd on the same doc since your fork point, and fetch before publishing `READY`.

The second rebase then conflicted where nobody predicted — not the docs file (jj merged it) but
three product files `main` had rewritten meanwhile: `packages/workspace/src/workspace.test.ts`
(LEGION-70's new exact-command-list tests each needed `...identityProbeCommands(repoCloneDir)`
appended, or the suite fails, because provisioning here runs two probes on every launch);
`processes.ts` (LEGION-60 rewrote `promptExistingWorker` around a turn-start receipt, and the
adoption call had to be re-placed before the new `client.prompt()`); and `environment.ts`
(LEGION-74 replaced the strip list this branch had extended with an allow-list that already
excludes the six variables, so that hunk had nothing left to apply to and the lock moved into
the allow-list test). `main` also advanced by three commits between the fetch and the push, so
the rebase ran twice. The fingerprint changed (`66631d9c…7abe` → a new value) with the product
logic untouched. A fingerprint is a promise about the branch's added and removed lines, not about
its behaviour: a test on `main` that enumerates exactly what your code does will absorb your
additions on rebase, and a refactor of the function you hooked moves your hook — both are a
changed diff for the reviewer, however mechanical. Say precisely which lines and why, fetch
immediately before the rebase, and check `main@origin` again before the push.

## What the smoke rig proved that unit tests could not

The pane environment is asserted by tests that see only the `-e` pairs the daemon adds. A
variable inherited from the daemon's own environment — the six identity keys, when the daemon is
started from a worker pane — is invisible to them and was caught by the reviewer, not the suite.
The tester's real-pane evidence (`/proc/<pane_pid>/environ` on worker, root-architect, and
controller panes of a rig daemon deliberately started with the six variables leaked into its
environment) is the proof for that class; the corresponding unit test (`environment.test.ts`)
leaks them into the daemon env and asserts `paneEnv` and the `tmux new-session` command carry
none.
