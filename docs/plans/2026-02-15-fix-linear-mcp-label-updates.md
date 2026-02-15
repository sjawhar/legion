# Fix Linear MCP Label Updates Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix the streamlinear MCP so that `linear_linear(action="update", labels=[...])` and `linear_linear(action="create", labels=[...])` actually update/set labels instead of silently failing with "No updates provided".

**Architecture:** The bug is in `handleUpdate()` and `handleCreate()` in streamlinear's `linear-core.ts` — both accept a `labels` parameter but never process it. The fix adds a `resolveLabels()` function (following the existing `resolveState()` pattern) that queries Linear's GraphQL API for workspace labels, resolves names to IDs, and feeds `labelIds` into the mutation input.

**Tech Stack:** TypeScript, Linear GraphQL API, esbuild (build), MCP SDK

**Repository:** `/home/sami/.dotfiles/vendor/streamlinear` (local clone, jj-colocated)

**Source file:** `mcp/src/linear-core.ts` (all changes in this one file)

---

## Context

### Root Cause

In `handleUpdate()` (line 268), the function signature accepts `labels?: string[]` but the function body never processes `updates.labels`. After processing `state`, `priority`, and `assignee`, if `labels` was the only field provided, the `input` object remains empty and the function returns `"No updates provided"`.

Same bug in `handleCreate()` (line 381) — `options.labels` is accepted but never added to the mutation input.

### Existing Pattern to Follow

`resolveState()` (line 119):
1. Uses cached data from `getTeams()`
2. Matches by name (exact first, then partial, then aliases)
3. Returns the Linear UUID

**Note:** For labels, we use **exact match only** (case-insensitive). Partial matching is dangerous for automation — "worker-done" could accidentally match "worker-done-review". The `resolveState()` pattern is followed for caching, not for fuzzy matching.

### Key Constraint

Linear's `IssueUpdateInput.labelIds` requires **label UUIDs**, not names. We must resolve label names to IDs before sending the mutation.

---

### Task 1: Add label caching and resolution functions — Independent

**Files:**
- Modify: `mcp/src/linear-core.ts` — insert after `getTeams()` (line 116), before `resolveState()` (line 118)

**Step 1: Add cached label fetcher and resolver**

Insert after `getTeams()`:

```typescript
// Cache for workspace labels
let cachedLabels: Array<{ id: string; name: string }> | null = null;

export async function getWorkspaceLabels(): Promise<Array<{ id: string; name: string }>> {
  if (cachedLabels) return cachedLabels;

  const data = await graphql(`
    query {
      issueLabels(first: 250) {
        nodes { id name }
      }
    }
  `) as { issueLabels: { nodes: Array<{ id: string; name: string }> } };

  cachedLabels = data.issueLabels.nodes;
  return cachedLabels;
}

// Resolve label names to IDs (exact match, case-insensitive).
// Returns null if any label not found.
export async function resolveLabels(names: string[]): Promise<string[] | null> {
  if (names.length === 0) return [];

  const allLabels = await getWorkspaceLabels();
  const ids: string[] = [];

  for (const name of names) {
    const lower = name.toLowerCase();
    const match = allLabels.find((l) => l.name.toLowerCase() === lower);

    if (match) {
      ids.push(match.id);
    } else {
      return null;
    }
  }

  return ids;
}
```

**Step 2: Verify no syntax errors**

Run: `npx tsc --noEmit` (from `mcp/` dir)

Expected: No errors

**Step 3: Commit**

```bash
jj describe -m "feat: add resolveLabels() for label name-to-ID resolution"
jj new
```

---

### Task 2: Fix `handleUpdate()` to process labels — Depends on: Task 1

**Files:**
- Modify: `mcp/src/linear-core.ts` — `handleUpdate()` function (lines 268-353)

Three insertion points in this function:

**Step 1: Add label processing block**

Insert **after** the assignee handling (line 329, closing `}` of `if (updates.assignee !== undefined)`) and **before** the empty check (line 331, `if (Object.keys(input).length === 0)`):

```typescript
  if (updates.labels !== undefined) {
    if (updates.labels.length === 0) {
      // Empty array = clear all labels
      input.labelIds = [];
    } else {
      const labelIds = await resolveLabels(updates.labels);
      if (labelIds === null) {
        const allLabels = await getWorkspaceLabels();
        const validLabels = allLabels.map((l) => l.name).join(", ");
        return `Some labels not found. Available labels: ${validLabels}`;
      }
      input.labelIds = labelIds;
    }
  }
```

**Step 2: Update mutation response to include labels**

Replace the mutation query (lines 335-342) — add `labels { nodes { name } }` to the issue fields:

```typescript
  const updateResult = await graphql(`
    mutation($id: String!, $input: IssueUpdateInput!) {
      issueUpdate(id: $id, input: $input) {
        success
        issue { identifier title state { name } priority assignee { name } labels { nodes { name } } }
      }
    }
  `, { id: issueData.issue.id, input }) as { issueUpdate: { issue: Record<string, unknown> } };
```

**Step 3: Add labels to changes tracking**

After the assignee change tracking (around line 350), add:

```typescript
  if (updates.labels !== undefined) {
    const labelNames = ((issue.labels as { nodes: Array<{ name: string }> })?.nodes || [])
      .map((l) => l.name).join(", ");
    changes.push(`labels → ${labelNames || "(none)"}`);
  }
```

**Step 4: Verify no syntax errors**

Run: `npx tsc --noEmit` (from `mcp/` dir)

Expected: No errors

**Step 5: Commit**

```bash
jj describe -m "fix: process labels parameter in handleUpdate()"
jj new
```

---

### Task 3: Fix `handleCreate()` to process labels — Depends on: Task 1

**Files:**
- Modify: `mcp/src/linear-core.ts` — `handleCreate()` function (lines 381-412)

**Step 1: Add label processing**

After priority check (line 399: `if (options.priority !== undefined) input.priority = options.priority;`), before the mutation:

```typescript
  if (options.labels && options.labels.length > 0) {
    const labelIds = await resolveLabels(options.labels);
    if (labelIds === null) {
      const allLabels = await getWorkspaceLabels();
      const validLabels = allLabels.map((l) => l.name).join(", ");
      return `Some labels not found. Available labels: ${validLabels}`;
    }
    input.labelIds = labelIds;
  }
```

**Step 2: Verify no syntax errors**

Run: `npx tsc --noEmit` (from `mcp/` dir)

Expected: No errors

**Step 3: Commit**

```bash
jj describe -m "fix: process labels parameter in handleCreate()"
jj new
```

---

### Task 4: Update help text — Independent

**Files:**
- Modify: `mcp/src/linear-core.ts` — `handleHelp()` (line 419) and `buildToolDescription()` (line 480)

**Step 1: Update handleHelp() update section**

Replace lines 432-436:

```
**update** - Change state, priority, assignee
  {"action": "update", "id": "ABC-123", "state": "Done"}
  {"action": "update", "id": "ABC-123", "priority": 1}
  {"action": "update", "id": "ABC-123", "assignee": "me"}
  {"action": "update", "id": "ABC-123", "assignee": null}  → unassign
```

With:

```
**update** - Change state, priority, assignee, labels
  {"action": "update", "id": "ABC-123", "state": "Done"}
  {"action": "update", "id": "ABC-123", "priority": 1}
  {"action": "update", "id": "ABC-123", "assignee": "me"}
  {"action": "update", "id": "ABC-123", "assignee": null}  → unassign
  {"action": "update", "id": "ABC-123", "labels": ["bug", "urgent"]}
  {"action": "update", "id": "ABC-123", "labels": []}  → clear labels
```

**Step 2: Update handleHelp() create section**

After line 443, add a labels example:

```
  {"action": "create", "title": "Bug", "team": "ENG", "labels": ["bug"]}
```

**Step 3: Add labels note to Reference section**

After the state matching line (line 456), add:

```
Label matching is exact (case-insensitive): "bug" → "Bug", "Worker-Active" → "worker-active"

Labels array replaces all labels. Fetch current first, then append.
```

**Step 4: Update buildToolDescription()**

In `buildToolDescription()` (line 500), add a labels example line after the update example:

```
{"action": "update", "id": "ABC-123", "labels": ["bug"]}
```

**Step 5: Commit**

```bash
jj describe -m "docs: document labels parameter in help text"
jj new
```

---

### Task 5: Build, verify, and test — Depends on: Task 1, Task 2, Task 3, Task 4

**Step 1: Install dependencies and build**

Run (from `mcp/` dir):
```bash
npm install && npm run build
```

Expected: Build succeeds, `dist/index.js` and `dist/cli.js` updated

**Step 2: Verify build contains changes**

Run (from repo root): `grep -c "resolveLabels\|labelIds\|getWorkspaceLabels" mcp/dist/index.js`

Expected: 6+ matches

**Step 3: Commit build artifacts**

```bash
jj describe -m "build: rebuild dist with label support"
jj new
```

---

### Task 6: Squash and push — Depends on: Task 5

**Step 1: Review changes**

```bash
jj log --limit 10
```

**Step 2: Squash into single commit**

```bash
jj squash --from <first-change> --into <last-change>
jj describe -m "feat: add label support to update and create actions

Labels parameter was accepted in the schema but never processed.
Added workspace label caching, name-to-ID resolution, and wired
labels into handleUpdate() and handleCreate() mutations.

Fixes: labels=[] clears labels, labels=['name'] resolves names
to IDs via exact case-insensitive matching."
```

**Step 3: Push**

```bash
jj git push
```

---

## Verification Checklist

After implementation, verify these from the issue's acceptance criteria:

1. `linear_linear(action="update", id="LEG-126", labels=["worker-active"])` → returns success with `labels → worker-active` (NOT "No updates provided")
2. `linear_linear(action="update", id="LEG-126", labels=[])` → returns success with `labels → (none)`
3. `linear_linear(action="update", id="LEG-126", labels=["nonexistent"])` → returns error listing available labels
4. Combined: `state="In Progress"` + `labels=["worker-active"]` → both update
5. `linear_linear(action="get", id="LEG-126")` → confirms labels actually changed in Linear

**Note:** Testing requires the MCP server to be restarted to pick up the new build. The implementer should restart their OpenCode session or the MCP process after building.

## Notes

- Repository: `/home/sami/.dotfiles/vendor/streamlinear` — jj-colocated, use `jj` commands not `git`
- All changes are in ONE file: `mcp/src/linear-core.ts`
- The MCP binary is `mcp/dist/index.js` — rebuilt via `npm run build` in `mcp/`
- `issueLabels(first: 250)` may not cover all labels in very large workspaces — acceptable for now
- Consider submitting a PR to `github:obra/streamlinear` after the fix is verified
