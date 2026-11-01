---
title: "Killable work lands atomically and fails per item: clone into a temporary sibling then rename, and isolate each record in a boot-time reconcile"
category: daemon
tags:
  - workspace-provisioning
  - jj-clone
  - atomic-rename
  - reconnectWorkers
  - failure-isolation
  - boot
date: 2026-09-13
status: active
module: packages/workspace/src/workspace.ts, packages/daemon/src/daemon/processes.ts
related_issues:
  - "LEGION-28"
  - "sjawhar/legion#980"
  - "LEGION-11"
symptoms:
  - "Incomplete Jujutsu clone at <dir>: missing <dir>/.jj on every launch after one killed clone"
  - "a half-written .jj accepted as a finished clone"
  - "TypeError: undefined is not an object (evaluating 'api.revokeSessionCapability') aborts reconnectWorkers; every later dead worker stays counted as running"
---

# Killable Work Lands Atomically and Fails Per Item

Two unrelated-looking bugs in LEGION-28 share one shape: a multi-step operation that can be
interrupted partway (by a kill, a crash, or a throw) left state that a later reader mistook for
a completed step. The fixes are the two standard answers, recorded here with the specifics that
made them non-obvious in this codebase.

## 1. A clone that can be killed is written next to its destination, then renamed into place

`ensureRepoClone` (`packages/workspace/src/workspace.ts`) used to `jj git clone` straight into
the final directory and then check for `.jj`. A clone killed at its budget (or by a daemon crash)
left a directory with a partial `.jj`; the check passed; every workspace add against it failed
forever, and a *missing* `.jj` threw "Incomplete Jujutsu clone" on every launch with no way out.

Now:

1. `mkdtemp(`${repoCloneDir}.clone-`)` — a sibling in the **same parent**, so the final step is a
   same-filesystem directory move. (`jj git clone` accepts an existing empty destination;
   verified at plan time, jj 0.45.1.)
2. `jj git clone <remote> <tempDir>` through the budgeted, checked runner.
3. Completeness is filesystem truth, not exit-code truth: exit 0 **and** `<tempDir>/.jj` exists.
   A killed clone can exit non-zero with a partial `.jj`; a crash can leave any state; both
   checks are independent.
4. `rename(tempDir, repoCloneDir)`. On `ENOTEMPTY`/`EEXIST` (two issues provisioning the same
   repository for the first time concurrently) the other clone won if `<repoCloneDir>/.jj` now
   exists — remove our temp dir and return; otherwise rethrow.
5. `finally`: remove the temp dir, **best-effort**. A cleanup failure is logged (`failed to
   remove temporary clone at …`) and never replaces the clone's own error — the reviewer caught a
   version where an `EACCES` from `rm` masked the `Command timed out` the operator needed.
6. An existing final directory **without** `.jj` (left by an older daemon) is removed and
   re-cloned with a log line instead of throwing forever — the retroactive recovery for anything
   the old code left behind.

Leftover `<repo>.clone-*` siblings from a crash are inert (nothing can mistake one for a clone)
and are deliberately not swept: a sweep would race a concurrent in-flight clone of the same
repository.

The killed command itself is now reported as `Command timed out after <limit> s (ran <wall> s):
<cmd>` (stderr appended when present) — see `boot-probe-kill-is-transient-not-a-verdict.md` for
why the runner carries that data; the root's launch failure is still counted as before.

Regression pins: `workspace.test.ts` "reports a clone killed at its budget as a timeout, leaves
nothing at the final path, and clones fresh on the next attempt" (a fake runner writes a partial
`.jj` into the temp target and returns `timedOut`), "removes an incomplete clone that has no .jj
and clones again, logging it", and the cleanup-failure test (an `0o000` subdirectory makes `rm`
fail — skipped as root, where mode bits do not bind).

## 2. A boot-time reconcile over independent records isolates each one

`reconnectWorkers` probed every located worker claim inside a `claims.map(async …)` under
`Promise.all`. One claim's retirement threw (`revokeSessionCapability` reached `api` before it
was assigned — the ordering LEGION-11 fixes); the whole `Promise.all` rejected; `index.ts`'s
outer catch logged `worker reconnection failed`; every later dead worker stayed counted as
running and held a running-worker slot until some unrelated event probed it.

The fix is the per-item `try/catch` **inside** the mapped function, not `Promise.allSettled` at
the call site: every claim is still awaited before boot continues (the count `runningWorkerCount`
relies on must be settled), a throwing claim is logged with its token (`[legion] failed to
reconcile worker <token>:`) and left exactly where the throw found it — its locator still
recorded, so it still counts as running and a later probe can retry — and there is no array of
outcomes nobody inspects. `reconnectRoots` gets the same shape per tree (`failed to reconcile
root <tree>:`). The outer `worker reconnection failed` catch stays as a last resort.

Test shape worth copying (`processes.test.ts`): three confirmed claims with dead sockets,
`revokeSessionCapability` throwing for the second only; assert claims 1 and 3 were retired
(`kill-pane` ran, locator cleared, `resumeSessionFile` set), claim 2 kept its locator and saw no
`kill-pane`, and exactly one isolation log names claim 2's token with the thrown error as its
second argument.

This landed **without** LEGION-11's reordering (see the process note in
`../legion/stacked-base-and-review-folded-into-rebase.md`): on `main`'s ordering the pre-API
`TypeError` still happens for a dead confirmed worker at boot, but it is now a contained
per-claim failure, and LEGION-11 (#955) removes the cause separately.

## What to reuse

- Any artifact produced by a killable subprocess in more than one step: write beside the
  destination on the same filesystem, verify completeness from the filesystem, rename last,
  clean up best-effort, and give the *old* partial state a recovery path.
- Any boot loop over independent durable records: `try/catch` per item inside the map, log with
  the record's identity, leave the failing record untouched, still await all.
