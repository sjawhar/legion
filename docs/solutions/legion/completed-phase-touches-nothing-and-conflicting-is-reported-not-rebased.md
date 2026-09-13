---
title: "A completed phase touches nothing in the shared workspace; a CONFLICTING PR is reported, not rebased, and its signature is a missing Tests run"
category: legion
tags:
  - jj
  - shared-workspace
  - rebase
  - conflicting
  - github-actions
  - phase-worker
  - incident
date: 2026-09-13
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#1010"
---

# A completed phase touches nothing in the shared workspace; a CONFLICTING PR is reported, not rebased, and its signature is a missing Tests run

## What happened

LEGION-41's implementer pushed its round-5 fix at 11:33Z and reported completion at 11:42Z.
No `Tests` run appeared for either round-5 head. The implementer investigated, found GitHub
reporting the PR `CONFLICTING` (LEGION-20, #975, had landed on `main` touching the same
`up.sh` lines), and — because the skill allows a rebase for exactly that condition — ran
`jj rebase -s 'roots(main@origin..@)' -d main@origin` in `$LEGION_WORKSPACE` at 11:44–11:45Z.

The tester's phase had been active since 11:42Z. A Legion issue has **one** jj workspace shared
by every role; a rebase rewrites every commit in the chain, and jj moves the working copy with
it. While the rebase was in flight the tester's checkout sat on the rewritten *round-1* commit,
so the `down.sh` the tester executed at 11:44:50Z was the pre-port-gate version. Its acceptance
probe removed LEGION-44's live `legion-smoke-nats` container. (PR #1010 body, "Incident,
round 5"; architect's message to the implementer, 11:52Z.)

The rebase itself was correct — fingerprint unchanged, head green, `MERGEABLE` restored. The
*timing* caused the incident.

## The rules

1. **After `legion handoff complete`, the workspace is read-only for you.** No `jj new`, no
   edits, no rebase, no push, until the architect's next `spawn_worker` names you. The
   `legion-worker` skill says this ("treat `$LEGION_WORKSPACE` as read-only … a code change
   belongs to whichever phase is active now"); the point of this document is *why*: one role's
   jj operation moves every role's working copy, and the other role is mid-command.
2. **A `CONFLICTING` PR is reported to the architect**, who sends the implementer back with a
   fresh assignment. The skill's "rebase only on a real conflict" permission is a permission
   for the *active* implementer, not for a worker whose phase has ended.
3. **Read mergeability on every end-game wake** instead of waiting for a run:

   ```sh
   legion gh -- pr view <n> --repo <owner>/<repo> --json mergeable,mergeStateStatus,headRefOid
   ```

   "No `Tests` run for a pushed head" on a `pull_request`-triggered workflow is the
   `CONFLICTING` signature — GitHub Actions creates no `pull_request` run for a conflicting PR.
   On LEGION-41 the implementer spent ten minutes checking Actions status pages and re-listing
   runs before reading `mergeable`. Other branches pushed in the same window got runs; this
   one did not. That asymmetry *is* the diagnosis.
4. **The tester runs from a pristine export, not the shared checkout.** From round 5 onward the
   LEGION-41 tester ran every command from a `git archive <head>` export whose `scripts/smoke/*`
   hashes were checked against `jj file show -r <head>`. A concurrent jj operation cannot move an
   export. This is the tester-side half of rule 1.

## Related

- `docs/solutions/legion/shared-main-repo-hazards-for-concurrent-issue-workspaces.md` — the
  repository-level version of the same hazard (one operation log for every workspace).
- `docs/solutions/legion/conflict-only-rebases-keep-the-diff-auditable.md` — the fingerprint
  procedure the rebase itself followed correctly.
