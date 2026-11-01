---
title: git stash Interferes with jj Working Copy in Mixed-VCS Sessions
date: 2026-04-12
status: active
tags:
  - jj
  - git
  - working-copy
  - footgun
  - version-control
---

# git stash Interferes with jj Working Copy in Mixed-VCS Sessions

## Problem

In a jj-managed repository, running `git stash` reverts the working tree without jj's
knowledge. jj auto-snapshots the working copy on each command, but if `git stash` runs
before jj has snapshotted your edits, those edits are silently lost from jj's perspective.

**Symptom:** After `git stash`, `jj status` shows no changes even though you had edits.
The edits are in the git stash but jj doesn't know about them.

## Root Cause

jj and git share the same working tree. `git stash` directly manipulates the working tree
and git index, bypassing jj's snapshot mechanism. jj only learns about working copy changes
when you run a jj command — if git stash runs first, jj snapshots the post-stash (clean)
state.

## Fix

Run `git stash pop` to restore the working tree, then verify with `jj status`:

```bash
git stash pop
jj status  # Should now show your edits
```

## Prevention

**Never use `git stash` in a jj-managed repo.** Use jj equivalents instead:

| Intent | Instead of | Use |
|--------|-----------|-----|
| Save work temporarily | `git stash` | `jj new` (start a new change; old work stays in parent) |
| Check diff | `git diff` | `jj diff` or `jj diff --git` |
| Check status | `git status` | `jj status` |
| View history | `git log` | `jj log` |

If you need to inspect the git working tree state for debugging, use `git diff` (read-only)
rather than any git command that modifies the working tree.

## Related

- jj auto-snapshots on every jj command, not on git commands
- Mixing git and jj working-copy commands in the same session is unsafe
