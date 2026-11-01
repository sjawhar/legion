---
title: "GitHub Git Data API: tree-swap revert is unsafe when target branch has advanced"
category: github-api
tags:
  - github-api
  - git-data-api
  - revert
  - rollback
  - safety
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "526"
symptoms:
  - "revert PR removes changes from other PRs"
  - "tree-swap clobbers subsequent commits"
  - "rollback reverts more than expected"
---

# GitHub Git Data API: tree-swap revert is unsafe when target branch has advanced

## Context

When implementing a rollback/revert command that operates via the GitHub REST API (no
local git clone), a natural approach is "tree-swap": get the merge commit's first parent,
use its tree SHA, and create a new commit on the target branch with that tree. This
effectively replaces the branch's entire file tree with the pre-merge state.

## The Problem

Tree-swap only produces a correct revert when the target branch HEAD **is** the merge
commit. If any commits have landed after the merge, the parent's tree is stale — the
revert commit will silently remove all subsequent changes, not just the target PR's.

```
main:  A ── B(merge) ── C ── D (HEAD)
                │
                └── parents[0] = A

Tree-swap creates: D' with tree(A)
Result: C and D's changes are also reverted — WRONG
```

GitHub's REST API does not expose `git revert` (which computes an inverse diff). The
Git Data API only provides low-level tree/commit/ref operations.

## Solution

**Guard against stale state.** Before performing the tree-swap, verify that the target
branch HEAD equals the merge commit SHA:

```typescript
const mainSha = (await runGhCommand([
  "api", `repos/${repo}/git/ref/heads/main`, "--jq", ".object.sha"
])).trim();

if (mainSha !== mergeCommitSha) {
  throw new CliError(
    `Main has advanced past merge commit ${mergeCommitSha.slice(0, 8)}. ` +
    `Manual revert required — the tree-swap approach would affect ` +
    `${mainSha.slice(0, 8)} and all commits between.`
  );
}
```

When the guard fails, provide manual revert instructions:
```
git fetch origin && git checkout main && git revert <merge-commit-sha>
```

## Why not compute a proper inverse diff via the API?

A true `git revert` requires three-way merge machinery (merge base + ours + theirs) that
the GitHub Git Data API doesn't expose. Options considered:

1. **GitHub Merge API** (`POST /repos/{owner}/{repo}/merges`) — creates merge commits but
   doesn't support specifying a custom merge base for inverse-diff computation.
2. **Local clone** — would work but the rollback command runs on the daemon, not in a
   worktree with the repo checked out.
3. **Guard + fail loudly** — chosen approach. Safe, simple, and the common rollback
   scenario (catching a bad merge immediately) works because main hasn't advanced yet.

## Key Takeaway

When using the GitHub Git Data API for operations that `git` handles natively (revert,
cherry-pick, rebase), always verify preconditions that the low-level API can't enforce.
Tree/commit manipulation is not the same as patch-based operations — the API gives you
a scalpel but no anesthesia.
