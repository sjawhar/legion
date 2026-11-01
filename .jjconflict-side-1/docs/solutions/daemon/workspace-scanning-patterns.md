---
title: "Workspace scanning and issue ID derivation patterns"
category: daemon
tags:
  - workspace-scanning
  - filesystem
  - performance
  - issueId
  - knowledge-module
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "#240"
symptoms:
  - "CLI timeout when scanning workspaces"
  - "scanForWorkspaceDirs takes too long"
  - "all workspace issues collapse into one issue"
  - "issueId is wrong from workspace path"
---

# Workspace Scanning and Issue ID Derivation Patterns

## Problem

Legion workspaces live at `$XDG_DATA_HOME/legion/workspaces/{owner}/{number}/{workspace-name}/`. Each workspace is a full repo clone containing `node_modules/`, `.git/`, and other large directory trees. Two bugs emerged when building features that scan these workspaces:

1. **Recursive directory walks timeout** — a recursive `scanForWorkspaceDirs` hit 131,821 directories instead of the 274 actual workspaces, because it descended into `node_modules/`, `.git/`, etc. The CLI was killed after 60 seconds.

2. **Wrong issue ID derivation** — using `legionId.split("/").at(-1)` to extract the issue ID from the workspace path yields the project number (`"2"`) not the workspace name (`"sjawhar-legion-240"`). All workspace data collapsed into a single issue, making aggregation meaningless.

## Rules

### Always shallow-scan workspace directories

Workspace directories are direct children of `legionPaths.workspacesDir`. Never recurse into them — they contain full repo clones.

```typescript
// CORRECT: Shallow scan — enumerate children, check for .legion/
async function scanForWorkspaceDirs(rootDir: string): Promise<string[]> {
  const entries = await readdir(rootDir, { withFileTypes: true });
  const workspaceDirs: string[] = [];
  for (const entry of entries) {
    if (!entry.isDirectory()) continue;
    const candidate = path.join(rootDir, entry.name);
    const childEntries = await readdir(candidate, { encoding: "utf8" });
    if (childEntries.includes(".legion")) {
      workspaceDirs.push(candidate);
    }
  }
  return workspaceDirs;
}

// WRONG: Recursive walk — traverses node_modules, .git, etc.
async function walk(dir: string) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    if (entry.name === ".legion") { found.add(dir); continue; }
    await walk(path.join(dir, entry.name)); // 131K dirs instead of 274
  }
}
```

### Always use `path.basename(workspaceDir)` for issue ID

The workspace directory name IS the issue ID in Legion. Never derive it from `legionId`.

```typescript
// CORRECT
const issueId = path.basename(workspaceDir);
// → "sjawhar-legion-240"

// WRONG
const issueId = legionId.split("/").at(-1);
// → "2" (project number, not issue ID)
```

### Unit tests won't catch these bugs

Both bugs passed all 34 unit tests because test fixtures use clean temp directories without `node_modules/` or `.git/`. **Any feature that scans workspace directories needs at least one integration test with realistic directory structure** — or a behavioral test that runs against the actual workspace root.

## Why Plan Concerns Matter

The architect phase flagged: *"Recursive .legion workspace scanning may be overkill — shallow scan of direct children is simpler and sufficient."* The plan phase repeated this concern. The implementer ignored both. Treat plan concerns as hard constraints — they exist because an earlier phase identified a real risk.
