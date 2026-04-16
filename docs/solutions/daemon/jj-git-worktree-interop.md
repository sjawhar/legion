---
title: "jj/git Worktree Interop: Idempotent Workspace Creation"
category: daemon
tags:
  - jj-workspaces
  - git-worktree
  - workspace-management
  - idempotent-operations
  - retry-pattern
  - dependency-injection
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "580"
symptoms:
  - "jj workspace add fails with 'already registered as a git worktree'"
  - "workspace directory was deleted but git worktree registration persists"
  - "daemon restart leaves stale worktree registrations"
  - "worker pruning removes directory but not git worktree state"
---

# jj/git Worktree Interop: Idempotent Workspace Creation

## Problem

jj uses git internally for storage (at `.jj/repo/store/git`), but doesn't always clean up
git's worktree registrations when workspace directories are deleted externally. This happens
when:

- The daemon prunes a dead worker (removes directory, but git worktree registration persists)
- The daemon restarts (directories may be cleaned up without proper `jj workspace forget`)
- Manual workspace cleanup (rm -rf without jj/git cleanup)

When a new worker is dispatched for the same issue, `jj workspace add` fails because git
still thinks the worktree path is registered.

## Solution: Prune-Before-Add with Force-Clear Retry

### Strategy

1. **Preventive**: Run `git worktree prune` before every `jj workspace add` to clean up
   registrations for directories that no longer exist
2. **Reactive**: If `jj workspace add` still fails with "already registered", force-clear
   the stale registration and retry
3. **Non-fatal**: All cleanup operations are wrapped in try/catch — failures don't block
   the main workspace creation path

### The Force-Clear Sequence

When `git worktree prune` isn't sufficient (the directory exists but is stale), use this
three-step sequence to clear the registration:

```typescript
const gitDir = `${clonePath}/.jj/repo/store/git`;

// Step 1: Force-add a git worktree at the same path (overwrites stale registration)
await runGit([`--git-dir=${gitDir}`, "worktree", "add", "-f", workspacePath, "HEAD"]);

// Step 2: Force-remove the worktree we just created
await runGit([`--git-dir=${gitDir}`, "worktree", "remove", "--force", workspacePath]);

// Step 3: Prune to clean up any remaining state
await runGit([`--git-dir=${gitDir}`, "worktree", "prune"]);
```

This works because `git worktree add -f` overwrites the stale registration, and the
subsequent remove + prune cleans it up completely, leaving jj free to create its own
workspace.

### Key Insight: jj's Internal Git Store

When you need to run git commands against a jj repository, target the internal git store:

```
${clonePath}/.jj/repo/store/git
```

Use `--git-dir=` to point git at this location. This is the git repository that jj manages
internally — worktree registrations, refs, and objects all live here.

## Testing Pattern: Optional Dependency Injection

The `RepoManagerDeps` interface uses optional deps with defaults for testability:

```typescript
interface RepoManagerDeps {
  runJj: (args: string[]) => Promise<JjResult>;   // required
  runGit?: (args: string[]) => Promise<JjResult>;  // optional, has default
  // ...
}

// In implementation:
const runGit = deps.runGit ?? defaultDeps.runGit;
if (runGit) {
  // Use it — guard handles the case where neither dep nor default is available
}
```

This pattern allows:
- Tests to inject mock `runGit` to verify git commands without running real git
- Production code to use the default `Bun.spawn(["git", ...])` implementation
- Graceful degradation if `runGit` is not provided (prune/retry is skipped)

## Known Limitations

1. **Error message brittleness**: The retry path triggers on `stderr.includes("already registered")`.
   If jj changes this error message, the retry won't fire (but the error will still be thrown).

2. **Race conditions**: If multiple workers create workspaces simultaneously, the force-clear
   sequence could interfere. In practice, workspace creation is per-issue so this is unlikely.

3. **Non-atomic force-clear**: If `git worktree add -f` succeeds but `remove --force` fails,
   the forced worktree remains and the jj retry will fail. The error is still surfaced to the
   caller.
