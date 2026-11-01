---
title: "Non-blocking review findings are fixed in the same round, and one-concern commits are what make a conflict-forced rebase cheap"
category: legion
tags:
  - review
  - fast-follow
  - no-deferrals
  - rebase
  - jj
  - conflict-resolution
  - fingerprint
  - long-lived-pr
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-74"
  - "sjawhar/legion#1007"
  - "sjawhar/legion#991"
  - "sjawhar/legion#981"
  - "sjawhar/legion#1017"
  - "sjawhar/legion#1029"
symptoms:
  - "a review marks findings `Non-blocking (fast-follow)` and the implementer is about to answer them with a follow-up note"
  - "GitHub reports the PR CONFLICTING for the second or third time while nothing in the reviewed diff changed"
  - "a sibling PR that landed on main calls a function this PR deleted, so the merge commit fails typecheck although GitHub says MERGEABLE"
---

# Non-Blocking Findings Are Fixed in the Round; One-Concern Commits Make Conflict Rebases Cheap

PR #1007 (LEGION-74) took four review rounds and three GitHub-reported conflicts over one day
while `main` moved under the same two tmux files. Two process facts from it are worth keeping.

## "Non-blocking (fast-follow)" orders the reviewer's list; it does not defer the fix

Review round 1 marked nine minors `Non-blocking (fast-follow)` — exemption lookups through the
prototype chain, a read-then-disable race on the session table, a two-invocation session
creation, a name regex narrower than its docstring, a too-narrow credential predicate, tmux
errors interpolating stdout, a dead module, and two doc clauses. The architect overruled the
label: under the no-deferrals rule (Sami, 2026-09-11: "My rule is no deferrals") a
non-blocking finding is still fixed in the round it was raised. All nine landed in one fix
commit plus one docs commit, each answered on its own thread with the fixing SHA, and the PR
body's `Fast-follow:` line stayed `none`.

What that bought: the reviewer's round 2 re-verified nine closed threads instead of carrying
nine open ones, and two of the "minors" — the read-then-disable race and the prototype-chain
lookup — turned out to be correctness bugs in the safety mechanism itself, not polish. A
`fast-follow` tag is the reviewer's severity ordering; it is never the implementer's permission
to leave the diff as it is.

## Three conflict-forced rebases, three different collision classes, all cheap

`main` took #991, #981, #1017 and #1029 while the PR was open; each collided with the branch in a
different way, and each was a conflict-only rebase (`conflict-only-rebases-keep-the-diff-auditable.md`):

| landed on `main` | collision | resolution |
| --- | --- | --- |
| #991 (LEGION-37) | a new test fixture predating a parameter this PR added — no textual conflict, GitHub said `MERGEABLE`, the *merge commit* failed typecheck | architect-authorised rebase; one fixture line |
| #981 (LEGION-27) | the same lines of `tmux.ts`/`runtime-tmux.ts` refactored | textual; `PANE_GONE_STDERR` moved into `tmux.ts` and the branch's helper was built on it |
| #1017 (LEGION-77) | a new child (`runSecretsGet`) calling a helper this PR deleted (`stripDispatchEnv`) | semantic; the child was placed on the correct side of the process boundary instead of the helper being restored |
| #1029 (LEGION-71) | a README paragraph rewritten directly under a section this PR inserted | textual; the inserted section stays, the paragraph below it takes `main`'s text |

Each cost minutes, not a round, because of four habits:

1. **One concern per commit.** The chain was allow-list → scrub → docs → checkpoint 13 → one fix
   commit per review round, each with its own docs. A conflict therefore lands in exactly the
   commit that owns the file (`jj new <conflicted commit>`, resolve, `jj squash`), and the
   descendants re-apply untouched. A squashed "LEGION-74" mega-commit would have made every
   conflict a whole-diff conflict.
2. **The fingerprint check, before and after, in one command.** The `legion-worker` skill's
   `sha256sum` over added/removed lines against `fork_point(main@origin | <head>)`
   (`unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md`) came out identical each time
   (`2888e52c…` → `2888e52c…`); one PR comment per rebase with both SHAs and both fingerprints is
   what let the tester and reviewer treat the moved head as the verified one. A merge-commit
   typecheck failure (#991) is the case the fingerprint cannot see — GitHub's `MERGEABLE` is
   textual — so read CI on the merge commit too, not only mergeability.
3. **Re-run the gates on the rebased tree before pushing** (tsc, biome, the package's tests, the
   smoke harness): the #1017 collision was invisible to a textual merge and only tsc named it.
4. **Record every rebase in the PR body's commits sentence** — what landed, what it touched, how
   it was resolved — so the reviewer reads the drift instead of rediscovering it. Round 3's
   blocking doc finding was a `#1017` clause ("stripped environment") that sat thirteen lines
   outside every hunk; after a rebase, grep `AGENTS.md` and `docs/solutions` for wording the
   sibling PR introduced about the surface you changed.

One mechanical note: right after a push, `legion gh -- pr checks <n> --watch` can answer `no
checks reported` because the run has not registered yet; poll `legion gh -- run list --branch
<branch> --json databaseId,headSha` for the new head first, then watch.

## Related

- `conflict-only-rebases-keep-the-diff-auditable.md` — what a conflict-only rebase may and may not change.
- `unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md` — the fingerprint command and its fileset trap.
- `rebasing-a-branch-across-a-refactor-of-its-own-call-sites.md` — the #981-class collision in detail.
