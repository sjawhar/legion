---
title: "Graduated fetch urgency — match blocking behavior to cost of staleness"
category: daemon
tags:
  - repo-management
  - dispatch
  - blocking-vs-nonblocking
  - dependency-injection
  - error-handling
  - testing
  - cleanup
date: 2026-04-14
status: active
module: daemon
related_issues:
  - "523"
symptoms:
  - "worker starts with stale code after dispatch"
  - "implement worker runs jj git fetch that takes too long"
  - "daemon startup slow due to repo fetching"
  - "duplicate fetches after multi-worker cleanup"
---

# Graduated Fetch Urgency — Match Blocking Behavior to Cost of Staleness

## Context

The daemon maintains a default clone per repo via `ensureRepoClone`. Workers are
created from this clone, so a stale clone means workers start with outdated code
and waste time fetching during startup. Issue #523 added `jj git fetch` at three
points in the lifecycle, each with a different urgency.

## Pattern: Graduated Blocking by Consequence

Not every fetch has the same urgency. Match the async strategy to the cost of delay:

| Context | Strategy | Rationale |
|---------|----------|-----------|
| Implement dispatch | **Blocking** (`await`) | Implementer needs latest code; stale clone causes real rework |
| Other mode dispatch | **Non-blocking** (fire-and-forget) | Nice-to-have freshness, shouldn't delay dispatch response |
| Issue close (Done) | **Non-blocking** | Merge just landed — opportunistic warmth for next dispatch |
| Daemon startup | **Non-blocking** | Warm cache, don't delay startup |

### Implementation detail: one function, two calling conventions

`startBackgroundFetch` is called with `await` when blocking is needed and with
`.catch()` when fire-and-forget is needed. The name is slightly misleading for the
blocking path, but the alternative (two functions or a boolean parameter) adds code
for no behavioral change. Pragmatic choice.

```typescript
// Blocking (implement mode) — in server.ts dispatch handler
if (mode === WorkerMode.IMPLEMENT) {
  await startBackgroundFetch(opts.paths, repoRef, opts.repoManagerDeps);
}

// Non-blocking (other modes) — same function, different call site
startBackgroundFetch(opts.paths, repoRef, opts.repoManagerDeps).catch((err) => {
  console.error(`Background fetch failed: ${err.message}`);
});
```

## Pattern: Structured Error Collection for Batch Operations

`fetchAllTrackedRepos` walks `reposDir/{host}/{owner}/{repo}` and fetches every
clone. Rather than throwing on the first failure, it collects errors and continues:

```typescript
return { fetched: string[], errors: Array<{ repo: string; error: string }> };
```

This lets callers log partial failures without aborting. Essential for daemon startup
where one failing repo shouldn't prevent warming others.

## Pattern: Dedup via Map Before Batch Operation

When Done issues are cleaned up, multiple workers may reference the same repo. The
cleanup collects unique `RepoRef` values in a `Map<string, RepoRef>` keyed by
`{host}/{owner}/{repo}` before firing post-close fetches:

```typescript
const cleanedRepoRefs = new Map<string, RepoRef>();
// During first pass (per worker):
cleanedRepoRefs.set(repoKey, repoRef);
// After cleanup: iterate unique repos only
for (const [repoKey, repoRef] of cleanedRepoRefs) { ... }
```

Without this, N workers for the same repo would trigger N redundant fetches.

## Gotcha: Fresh Clone Edge Case

On first-ever dispatch for a repo, the blocking pre-dispatch fetch runs before
the clone exists. It fails gracefully and the clone proceeds normally via
`ensureRepoClone`. This is why **all fetch operations must be non-fatal** — it's
not just about network failures; the clone might not exist yet.

## Testing Patterns

### Virtual filesystem via record

For testing directory-walking code like `fetchAllTrackedRepos`, mock the filesystem
as a `Record<path, entries>` rather than touching real FS:

```typescript
const dirTree: Record<string, string[]> = {
  "/repos": ["github.com"],
  "/repos/github.com": ["acme"],
  "/repos/github.com/acme": ["widgets"],
};
const deps = { listDir: async (p) => dirTree[p] ?? [] };
```

### Command recording with defensive spread

When recording `jj` commands for test assertions, always spread/clone the args
array to prevent mutation leakage from later operations:

```typescript
const commands: string[][] = [];
runJj: async (args) => {
  commands.push([...args]);  // Spread prevents mutation
  return { exitCode: 0, stdout: "", stderr: "" };
},
```

### Background operation timing in tests

For non-blocking (fire-and-forget) operations, tests use `setTimeout` to let
background work complete before asserting:

```typescript
await new Promise((resolve) => setTimeout(resolve, 50));
const fetchCmd = jjCommands.find((c) => c[0] === "git" && c[1] === "fetch");
expect(fetchCmd).toBeDefined();
```

This is pragmatic — the alternative (exposing a flush mechanism) would leak test
concerns into production code.
