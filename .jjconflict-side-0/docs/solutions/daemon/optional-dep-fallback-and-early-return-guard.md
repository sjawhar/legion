---
title: "Optional dependency fallback pattern and early-return guard pitfall"
category: daemon
tags:
  - dependency-injection
  - testing
  - early-return
  - fallback
  - dead-code
  - cleanup
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "sjawhar-legion-472"
symptoms:
  - "directory scan never runs in production"
  - "new code path unreachable due to early return"
  - "if (x) guard always true after ?? fallback"
  - "test passes but production silently skips feature"
---

# Optional Dependency Fallback Pattern and Early-Return Guard Pitfall

## Problem

Two related bugs emerged when adding a directory scan fallback to `cleanupDoneIssueWorkers` in `server.ts`:

1. **Early return blocked the scan**: The function had `if (doneIssueIds.length === 0) { return; }` before the directory scan. The scan is independent of Done issues — it removes workspaces for issues that fell off the board entirely. When no issues were Done, the function returned before reaching the scan, silently skipping it on every state collect where no issues transitioned to Done.

2. **Dead `if (listDir)` guard**: After writing `const listDir = opts.repoManagerDeps?.listDir ?? defaultRepoManagerDeps.listDir`, the code had `if (listDir) { ... }`. Since `defaultRepoManagerDeps.listDir` is always defined, the `??` fallback always resolves to a function — the `if` check was always true and could never gate anything.

## Rules

### Check whether early returns block new independent code paths

When adding a new code path inside a function that has early returns, explicitly verify that every early return above the new code is appropriate for the new path too.

```typescript
// BEFORE — early return was correct for Done-issue cleanup loops,
// but silently blocked the directory scan on every non-Done collect
const cleanupDoneIssueWorkers = async (state: CollectedState) => {
  const doneIssueIds = ...;
  if (doneIssueIds.length === 0) {
    return; // ← blocks everything below, including the scan
  }
  // ... Done-issue cleanup loops ...

  // NEW: directory scan — independent of Done issues, should always run
  if (opts.paths) { ... }
};

// AFTER — remove the early return; loops naturally no-op when set is empty
const cleanupDoneIssueWorkers = async (state: CollectedState) => {
  const doneIssueIds = ...;
  const doneIssueIdSet = new Set(doneIssueIds);
  // loops check doneIssueIdSet.has(issueId) — skip all when empty, no early return needed

  // directory scan always runs
  if (opts.paths) { ... }
};
```

### After `?? always-defined-value`, don't add a truthiness guard

When the right-hand side of `??` is always defined (e.g., a concrete object property, not an optional), the result is always truthy. A subsequent `if (result)` is dead code and misleads readers into thinking the value could be falsy.

```typescript
// WRONG — if (listDir) is dead code; defaultRepoManagerDeps.listDir is always defined
const listDir = opts.repoManagerDeps?.listDir ?? defaultRepoManagerDeps.listDir;
if (listDir) {
  // ...
}

// CORRECT — use the value directly
const listDir = opts.repoManagerDeps?.listDir ?? defaultRepoManagerDeps.listDir;
const entries = await listDir(workspacesDir);
```

Only add `if (x)` after `??` when the fallback itself could be undefined (e.g., `?? someOptionalField`).

### Optional dependency fallback pattern

When a dependency needs to be mockable in tests but have a real default in production:

1. Add it as optional to the interface: `listDir?: (path: string) => Promise<string[]>`
2. Implement it in `defaultDeps` (the concrete default object)
3. Export `defaultDeps` so consuming modules can reference it
4. Use `??` fallback in consuming code: `opts.repoManagerDeps?.listDir ?? defaultRepoManagerDeps.listDir`
5. **Test both paths**: inject the dep explicitly in one test, omit it (forcing the fallback) in another

```typescript
// repo-manager.ts
export interface RepoManagerDeps {
  rmDir: (path: string) => Promise<void>;
  listDir?: (path: string) => Promise<string[]>; // optional — has default
}

export const defaultDeps: RepoManagerDeps = {
  rmDir: async (p) => { /* real fs */ },
  listDir: async (p) => {
    try { return await readdir(p, { encoding: "utf8" }); }
    catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw err;
    }
  },
};

// server.ts — fallback to real fs when not injected
const listDir = opts.repoManagerDeps?.listDir ?? defaultRepoManagerDeps.listDir;
```

### Test the fallback path explicitly

Tests that inject all dependencies never exercise the fallback. Add a test that omits the optional dep, uses a real temp directory, and verifies the fallback fires:

```typescript
it("directory scan uses real fs listDir when repoManagerDeps.listDir is not injected", async () => {
  const rmDirCalls: string[] = [];
  const scanDir = await mkdtemp(...);
  await mkdir(path.join(scanDir, "off-board-issue"));

  // No listDir in repoManagerDeps — fallback to defaultDeps.listDir must fire
  await startTestServer({
    paths: pathsPointingTo(scanDir),
    repoManagerDeps: makeRepoManagerDeps({ rmDir: async (p) => rmDirCalls.push(p) }),
    // listDir intentionally omitted
  });

  await triggerStateCollect({ someIssue: "Done" }); // must have ≥1 Done to pass early returns
  await new Promise(r => setTimeout(r, 50));

  expect(rmDirCalls).toHaveLength(1); // real readdir found the off-board dir
});
```

**Note**: If the function has early returns before the new code path, the test data must satisfy all guards leading to that path (e.g., include at least one Done issue if the function returns early when `doneIssueIds.length === 0`).
