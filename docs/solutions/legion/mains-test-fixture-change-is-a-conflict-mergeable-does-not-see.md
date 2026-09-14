---
title: "main changed the test fixture your new tests call: a conflict MERGEABLE does not see, and why the rebase costs no CI run when the merge ref does not compile"
category: legion
tags:
  - rebase
  - merge-ref
  - MERGEABLE
  - typecheck
  - test-fixture
  - fingerprint
  - jj-rebase
  - ci-queue
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/__tests__/processes.test.ts
problem_type: process
severity: medium
related_issues:
  - "LEGION-93"
  - "sjawhar/legion#1082"
  - "LEGION-15"
  - "sjawhar/legion#1027"
  - "LEGION-42"
applies_when:
  - A pull request's CI goes red at a head whose own code was green in an earlier run, and GitHub still reports MERGEABLE
  - main merged a change to a shared test helper or fixture (a signature, a removed helper) between your branch's green run and its next push
  - You are deciding whether Sami's no-rebase rule applies
---

# `main` changed the test fixture your new tests call

## What happened

LEGION-93's round-1 run was green at `8101429b` (2026-09-14 ~06:40Z). Thirty minutes later `main`
merged #1027 (LEGION-15, `bcdeff9f`): every wait in `processes.test.ts` became an awaited event,
`expireTurnStartWait(clock)` became `expireTurnStartWait(sleeps, clock)`, and `flushEventLoopUntil`
was removed. The branch's two new tests and their fixture — written against the old harness, in a
file #1027 rewrote around them — called the old signature at four sites. No hunk collided: GitHub
said `MERGEABLE`, `mergeStateStatus: BLOCKED` (branch protection, not a conflict). The cleanup
push's CI (run 34819529304) ran the **merge ref** and failed `typecheck` with four `TS2554: Expected
2 arguments, but got 1` and `test` with `TypeError: sleeps is not a function`. No daemon source was
implicated; the reviewer's REQUEST_CHANGES named the cause.

## How it differs from the case already documented

`conflict-only-rebases-keep-the-diff-auditable.md` § "MERGEABLE is not compatible" records the
mirror image (LEGION-42): the *branch* tightened a contract and `main`'s *new* tests failed on the
merge ref — its first diagnostic is "the failing test names are not in your tree". Here the
direction is reversed: `main` changed the **harness** and the branch's **own** tests fail, so the
failing names *are* in your tree, but no commit on the old base can fix them — the branch does not
have `sleeps` to call. Tell the two apart by the same three checks with the first inverted:

1. The failing lines are your own new call sites, and the job is `typecheck` (or a `TypeError` on
   a helper), not an assertion — a signature the branch has never seen.
2. `main` moved since the last green merge commit:
   `jj log -r 'fork_point(main@origin | @-)..main@origin'` names the harness commit.
3. Re-running the job would fail again; the merge commit is deterministic. `run rerun --failed` is
   for a flake.

## The decision: the rebase is necessary and costs nothing the rule protects

Sami's rule (2026-09-11) exists because the CI queue is slow: an unnecessary rebase spends a run.
When the merge ref does not compile, three facts make the rebase the cheaper path, and the
architect recorded the exception on those grounds:

- A test-fix commit is needed regardless, and it needs a CI run regardless; a rebase adds none.
- The fix cannot be verified on the old base: `bunx tsc` and `bun test` there run against the
  harness the branch has, not the one `main` has. Only the merged tree can prove the adaptation.
- The alternative — fixing blind and pushing to let the merge ref judge — is the extra CI run.

Report a `CONFLICTING` PR without rebasing, as the skill says; report a `MERGEABLE` PR whose merge
ref does not compile to the architect as this case, and rebase on its decision.

## The procedure, with the fingerprints that prove each step changed only what it should

```sh
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch
# 1. before: the tip you are about to move
jj -R "$LEGION_WORKSPACE" diff --from "fork_point(main@origin | <tip>)" --to <tip> \
  --git --context 0 '~(.legion | docs/solutions)' | sed -e '/^@@/d' -e '/^index /d' | sha256sum
# 2. the whole chain, change ids kept (never `jj duplicate`, never an operation-log rollback)
jj -R "$LEGION_WORKSPACE" rebase -b legion/<KEY> -d main@origin
# 3. rebased-only: must EQUAL step 1 -- the text merge itself changed nothing in the product
# 4. adapt, gates, one commit, then the fingerprint again: differs by that commit alone
```

LEGION-93: `ba2adbdb` `adddb96b…` → rebased-only `25af70d5` `adddb96b…` (equal; 12 commits, no
conflict markers) → `e7e0b7ed` `ba383a28…` after `test(daemon): follow LEGION-15's event-awaiting
fixture in the LEGION-93 tests` (+13/−8, one file). The reviewer verified the same pair and, with
`processes.test.ts` excluded too, an unchanged `02cf14a3…` from the reviewed head — no daemon
source, docs, or e2e file moved — and approved without a new tester round. The middle fingerprint
is what earns that: it separates "the rebase" from "the fix" for the reviewer.

Adapt **in `main`'s own style**, not yours: read how #1027 converted the sibling tests in the same
block before touching your own. Here that meant `sleeps` destructured from the fixture,
`expireTurnStartWait(sleeps, clock)` (which does `await sleeps(TURN_START_BOUND_MS).next()` before
firing the clock), and the one remaining tick budget — a 50× `onceEventLoop()` guard against a
relaunch that must *not* happen — becoming `await flushEventLoop(50)` under `main`'s
`// Negative wait:` marker with the decline chain named, since #1027's `flushEventLoop` doc reserves
the fixed drain for exactly a decline that does no I/O. The positive event (`worker-died`) was
already awaited: it is published inside the drain the test `await`s.

Push is a sideways bookmark move (`[move sideways from … to …]`) — the expected force-push of a
rebased branch. `.legion/` stays deleted on the new head; no handoff is written for this push.

## Related

- `conflict-only-rebases-keep-the-diff-auditable.md` — the mirror case (branch tightened a
  contract, `main`'s new tests fail) and the added/removed-line identity check.
- `unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md` — the hashed fingerprint and
  its fileset trap.
- `completed-phase-touches-nothing-and-conflicting-is-reported-not-rebased.md` — the default:
  `CONFLICTING` is reported, not rebased.
- `../testing/await-the-event-not-a-tick-budget.md` — the harness change (#1027) this branch
  met, and the negative-wait marker rule.
