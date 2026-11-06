---
title: "Resolving main's new members inside a block your branch gated, and naming what actually moved the fingerprint"
category: legion
tags:
  - forward-merge
  - conflict-resolution
  - react
  - fingerprint
date: 2026-11-01
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2549"
---

# Resolving main's new members inside a block your branch gated, and naming what actually moved the fingerprint

A branch that moves a call inside a new condition meets main's additions to that same call as a
textual conflict. "Keep both sides" is the start of the resolution, not the whole of it.

- **Decide where main's new member belongs by the gate's contract, not by the diff.** The gate
  exists because something is a page-wide or surface-wide singleton; read what the ungated surface
  does before you put main's member inside it. A member that belongs outside the gate, left inside
  it, silently stops running on the surface the gate excludes.
- **Keep each teardown in the same gate as the call it undoes.** A registration inside the
  condition with its unregister outside it leaks on every mount of the gated surface.
- **Write the union in the effect's dependency array.** That is the one hunk where a take-ours or
  take-theirs resolution produces a stale closure that compiles and passes: your gate variable and
  main's new values must both be there. The check that catches it is the lint's exhaustive-
  dependency rule, not the type checker - do not report `tsc` as the control for a missing
  dependency, because it has nothing to say about one.
- **Name the files that moved the fingerprint by computing them, never by copying the conflict
  list.** The fingerprint hashes the pull request's own added and removed lines with `--context 0`
  and no `@@` or `index` lines, so a file can conflict, be resolved, and leave the hash untouched
  when main's edits merely surround your lines. Diff the two fingerprinted diffs against each
  other and report what that shows.

## Evidence

In sjawhar/legion#2549 main added `remarkMark` and `whenSent` to the margin's `registerDocument`
call and `pendingWaiter`/`whenSent` to that effect's dependency list, inside the block this branch
had gated on `page` (`packages/dispatch/web/src/features/doc/ProofDocument.tsx`). Both of main's
members belong inside the gate, because the margin's open document is a page-level singleton the
reading column must not claim, and the gated unregister sits in the same condition. The dependency
list is the union, `page, pendingWaiter, queryClient, whenSent`.

Two files took the conflict (`ProofDocument.tsx` and `packages/proof-editor/src/lib.ts`) and the
implementer reported both as the files that moved the fingerprint `c7cd1a30…` → `4e4c9820…`. The
reviewer, computing it rather than inferring it, found only `ProofDocument.tsx` among the branch's
own changed lines: `lib.ts`'s resolution kept each side's statements whole, so the branch's own
lines there are what they were. Three roles passed the round with two different file lists on the
record.
