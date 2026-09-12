---
title: "A PR whose merge is CONFLICTING gets no pull_request CI at all: check mergeStateStatus before waiting on checks"
category: github
tags:
  - github-actions
  - pull_request
  - mergeable
  - mergeStateStatus
  - rebase
  - jj
  - long-lived-pr
  - ci-watcher
date: 2026-09-12
status: active
module: .github/workflows, packages/daemon
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
---

# A CONFLICTING PR Gets No `pull_request` CI at All

## Symptom

PR #962 had been open for a day across several sibling merges to `main`. A push at 18:17Z ran
both workflows green. Two later pushes (18:30Z and 18:40Z) produced nothing: `gh run list
--branch legion/LEGION-21` listed no run for either sha, the commit's check-suites API showed
only the `claude` and `legion-reviewer` app suites (both `queued`) and no `github-actions` suite,
and a watcher polling for the runs sat for twenty-five minutes reporting nothing. Nothing on the
PR page said why.

## Mechanism

Between the green push and the next one, `main` had advanced eight commits — among them
LEGION-23 (#966), which moved code this branch also edited. GitHub recomputed the PR as
`mergeable: CONFLICTING` / `mergeStateStatus: DIRTY`. A `pull_request` event on a conflicting PR
has no merge ref to build, so GitHub does not queue `pull_request`-triggered workflows for it.
Both of this repository's workflows trigger on `pull_request`, so the PR simply stopped being
tested, with no failure to see.

Diagnosis is one call, and it is the call to make before waiting on checks:

```sh
legion gh -- pr view <n> --repo <owner>/<repo> --json mergeable,mergeStateStatus
# {"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}    -> rebase now; CI will not run
# {"mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED"}    -> fine; BLOCKED = review required
```

After the rebase and push, the `github-actions` check suites appeared within about forty
seconds and both workflows completed green.

## Rules

1. **After every push to a PR that has been open across a sibling merge, read
   `mergeable`/`mergeStateStatus` first.** `CONFLICTING` means rebase, not "CI is slow".
2. **A CI watcher must assert that a `github-actions` check suite exists for the sha within a
   couple of minutes** (`gh api repos/<o>/<r>/commits/<sha>/check-suites`) and alert when it does
   not; waiting for a run id that will never be created is an unbounded wait.
3. **Poll with a token.** Unauthenticated polling of `api.github.com/repos/…/actions/runs` from a
   busy shared IP hits `403` almost immediately; use `legion gh -- run list --branch <b> --json
   databaseId,name,status,conclusion,headSha` with a grant, or the daemon's `ci-green` notice.
4. **`main` moves while you resolve.** Fetch again before pushing the rebased branch; on this PR
   a release commit landed during conflict resolution and a second, conflict-free rebase was
   needed before the push.

## Rebasing a Legion branch that carries other roles' commits

The shared issue workspace holds the reviewer's and tester's handoff commits, some of them not
yet on origin (the review App cannot push; its commits ride on the implementer's next push).
Rebase the whole chain, never only your own commits:

```sh
jj rebase -s 'roots(main@origin..@)' -d main@origin
```

This moves every commit above `main` — yours, the tester's, the reviewer's local one — so the
push that follows carries them in order. Resolve conflicts bottom-up with edit-and-squash
(`jj new <first conflicted>`, fix, `jj squash`), as
`docs/solutions/legion/handoff-file-conflicts-during-rebases.md` describes; jj re-applies the
resolution to every descendant, so a conflict at the cutover commit is resolved once. Leave the
stray empty working-copy commits `jj new` creates with `jj abandon` before `jj edit`ing the tip,
and re-set the bookmark to the tip before pushing.

## Related

- `docs/solutions/github/pull-request-trigger-paths-follow-the-pr-head.md`: a different way a PR
  check fails to appear (a path-filtered workflow triggered on `push` instead of
  `pull_request`) on a *mergeable* PR.
- `docs/solutions/legion/handoff-file-conflicts-during-rebases.md`: the `.legion/` conflict
  resolution procedure the rebase above relies on.
