---
title: "Linear MCP label resolution with cache refresh on miss"
category: integration-issues
tags:
  - linear
  - mcp
  - labels
  - caching
  - name-resolution
  - error-handling
  - graphql
module: streamlinear
symptoms:
  - labels parameter silently ignored in update/create actions
  - '"No updates provided" error when labels were the only update'
  - label names not resolved to IDs for Linear API
  - stale cache missing recently created labels
date: 2026-02-15
status: active
---

# Linear MCP Label Resolution with Cache Refresh on Miss

## Problem

The Linear MCP accepted a `labels` parameter in `update` and `create` actions but never processed it. This caused silent failures:

- **Silent parameter ignoring**: `labels=["worker-active"]` was accepted but never applied
- **False "no updates" error**: When labels were the only update, returned "No updates provided"
- **No label resolution**: Linear API requires label IDs, but MCP accepted names without resolving them
- **Stale cache issues**: Recently created labels wouldn't be found in cached label list

## Solution

Implement label name-to-ID resolution following the existing codebase pattern used for teams (`getTeams()`) and states (`resolveState()`):

1. **Cached label fetcher** (`getWorkspaceLabels()`) - fetches all workspace labels once
2. **Name-to-ID resolver** (`resolveLabels()`) - maps label names to IDs with cache refresh on miss
3. **Integration into mutations** - add `labelIds` to GraphQL mutation input
4. **Actionable error messages** - return specific missing labels and available options

## Implementation

### 1. Cached Label Fetcher

Follows the same pattern as `getTeams()`:

```typescript
let cachedLabels: Array<{ id: string; name: string }> | null = null;

export async function getWorkspaceLabels(): Promise<Array<{ id: string; name: string }>> {
  if (cachedLabels) return cachedLabels;
  const data = (await graphql(`
    query { issueLabels(first: 250) { nodes { id name } } }
  `)) as { issueLabels: { nodes: Array<{ id: string; name: string }> } };
  cachedLabels = data.issueLabels.nodes;
  return cachedLabels;
}
```

**Key details:**
- Module-level cache variable initialized to `null`
- Returns cached value if available (avoids redundant API calls)
- Fetches first 250 labels (Linear workspace limit is typically much lower)
- Stores minimal data: only `id` and `name` fields needed for resolution

### 2. Label Name-to-ID Resolver with Cache Refresh

Two-pass resolution with cache refresh on miss:

```typescript
export async function resolveLabels(
  names: string[]
): Promise<{ ids: string[] } | { missing: string[] }> {
  if (names.length === 0) return { ids: [] };

  const resolve = (allLabels: Array<{ id: string; name: string }>) => {
    const ids: string[] = [];
    const missing: string[] = [];
    for (const name of names) {
      const match = allLabels.find((l) => l.name.toLowerCase() === name.toLowerCase());
      if (match) ids.push(match.id);
      else missing.push(name);
    }
    return missing.length > 0 ? { missing } : { ids };
  };

  const first = resolve(await getWorkspaceLabels());
  if ("ids" in first) return first;

  cachedLabels = null;
  const second = resolve(await getWorkspaceLabels());
  return second;
}
```

**Key details:**
- **Empty array handling**: `labels=[]` returns `{ ids: [] }` to clear all labels
- **Case-insensitive matching**: `"Bug"` matches `"bug"` or `"BUG"`
- **Discriminated union return**: `{ ids: string[] }` on success, `{ missing: string[] }` on failure
- **Two-pass resolution**: First attempt uses cache, second attempt refreshes cache
- **Cache invalidation**: Sets `cachedLabels = null` before second fetch
- **Extracted resolution logic**: `resolve()` function used for both passes (DRY principle)

### 3. Integration into Update Action

Add label resolution before mutation:

```typescript
export async function handleUpdate(
  id: string,
  updates: { state?: string; priority?: number; assignee?: string | null; labels?: string[] }
): Promise<string> {
  // ... existing code for state, priority, assignee ...

  if (updates.labels !== undefined) {
    const result = await resolveLabels(updates.labels);
    if ("missing" in result) {
      const allLabels = await getWorkspaceLabels();
      return `Labels not found: ${result.missing.join(", ")}. Available: ${allLabels.map((l) => l.name).join(", ")}`;
    }
    input.labelIds = result.ids;
  }

  if (Object.keys(input).length === 0) {
    return "No updates provided";
  }

  const updateResult = (await graphql(
    `
    mutation($id: String!, $input: IssueUpdateInput!) {
      issueUpdate(id: $id, input: $input) {
        success
        issue { identifier title state { name } priority assignee { name } labels { nodes { name } } }
      }
    }
  `,
    { id: issueData.issue.id, input }
  )) as { issueUpdate: { issue: Record<string, unknown> } };

  // ... format response with label changes ...
  if (updates.labels !== undefined) {
    const labelNames = ((issue.labels as { nodes: Array<{ name: string }> })?.nodes || [])
      .map((l) => l.name)
      .join(", ");
    changes.push(`labels → ${labelNames || "(none)"}`);
  }
}
```

**Key details:**
- **Check for `undefined`**: `updates.labels !== undefined` allows `labels=[]` to clear labels
- **Early return on error**: Returns actionable error message before mutation
- **Add to mutation input**: `input.labelIds = result.ids` (Linear API field name)
- **Fetch labels in response**: Add `labels { nodes { name } }` to mutation response
- **Include in change summary**: Show updated labels in success message

### 4. Integration into Create Action

Same pattern as update:

```typescript
export async function handleCreate(
  title: string,
  team: string,
  options: { body?: string; priority?: number; labels?: string[] }
): Promise<string> {
  // ... existing code ...

  if (options.labels && options.labels.length > 0) {
    const result = await resolveLabels(options.labels);
    if ("missing" in result) {
      const allLabels = await getWorkspaceLabels();
      return `Labels not found: ${result.missing.join(", ")}. Available: ${allLabels.map((l) => l.name).join(", ")}`;
    }
    input.labelIds = result.ids;
  }

  // ... mutation ...
}
```

**Key details:**
- **Only resolve if provided**: `options.labels && options.labels.length > 0` (optional parameter)
- **Same error handling**: Consistent error message format across actions

## Key Implementation Details

| Aspect | Implementation |
|--------|----------------|
| **Pattern Consistency** | Follows existing `getTeams()`/`resolveState()` pattern for familiarity |
| **Cache Strategy** | Module-level variable, refresh on miss (handles recently created labels) |
| **Case Sensitivity** | Case-insensitive matching (`toLowerCase()`) for user convenience |
| **Empty Array Semantics** | `labels=[]` clears all labels (explicit empty array vs undefined) |
| **Error Messages** | Actionable: lists missing labels AND available options |
| **Return Type** | Discriminated union: `{ ids: string[] } \| { missing: string[] }` |
| **GraphQL Field** | Linear API uses `labelIds` (not `labels`) for mutation input |
| **Response Inclusion** | Fetch `labels { nodes { name } }` in mutation response to show changes |
| **Two-Pass Resolution** | First attempt uses cache, second refreshes (handles stale cache) |

## Pattern: Refresh-on-Miss Caching

This pattern is useful when:
- Data changes infrequently but can change (labels created/deleted)
- Fetching is expensive (API call)
- Stale cache is acceptable for most cases but must handle misses

**Implementation:**
1. Try resolution with cached data
2. If resolution fails (missing items), invalidate cache
3. Retry resolution with fresh data
4. Return error if still missing (truly doesn't exist)

**Comparison to other patterns:**

| Pattern | When to Use |
|---------|-------------|
| **Refresh-on-miss** | Data changes occasionally, stale cache acceptable, need to handle new items |
| **TTL expiration** | Data changes predictably, can tolerate bounded staleness |
| **No caching** | Data changes frequently, freshness critical |
| **Cache-aside** | Read-heavy workload, data rarely changes |

## Pattern: Discriminated Union Returns

Using TypeScript discriminated unions for success/failure:

```typescript
type Result = { ids: string[] } | { missing: string[] };
```

**Benefits:**
- Type-safe error handling: `if ("missing" in result)` narrows type
- No exceptions for expected failures (missing labels is expected)
- Caller must handle both cases explicitly
- Clear semantics: success has `ids`, failure has `missing`

**When to use:**
- Expected failures that aren't exceptional (user input errors)
- Multiple failure modes need different handling
- Want to avoid try/catch for control flow

## Pattern: Actionable Error Messages

Error messages include:
1. **What failed**: "Labels not found"
2. **Specific failures**: Which labels were missing
3. **Available options**: What labels exist

```typescript
return `Labels not found: ${result.missing.join(", ")}. Available: ${allLabels.map((l) => l.name).join(", ")}`;
```

**Example output:**
```
Labels not found: workeractive, urgnt. Available: worker-active, worker-done, bug, feature, urgent
```

User can immediately see:
- Typo: "workeractive" should be "worker-active"
- Typo: "urgnt" should be "urgent"

## Prevention Strategies

### Follow Existing Codebase Patterns

Before implementing new functionality:
1. **Search for similar patterns**: Found `getTeams()` and `resolveState()` doing similar resolution
2. **Match the structure**: Used same module-level cache, same function naming convention
3. **Reuse error handling**: Followed same "not found" error message format
4. **Maintain consistency**: Makes codebase easier to understand and maintain

### Handle Empty Arrays vs Undefined

Distinguish between:
- `undefined`: Parameter not provided (don't update)
- `[]`: Empty array provided (clear all values)

```typescript
if (updates.labels !== undefined) {  // Not: if (updates.labels)
  // labels=[] will enter this block and clear labels
}
```

### Cache Invalidation for Stale Data

When caching data that can change:
1. **Detect staleness**: Resolution failure indicates possible stale cache
2. **Invalidate and retry**: Set cache to `null` and refetch
3. **Limit retries**: Only retry once (avoid infinite loops)
4. **Return error if still missing**: After refresh, missing items truly don't exist

### Test with Edge Cases

Verification covered:
- `labels=["worker-active"]` → sets label (main case)
- `labels=[]` → clears all labels (empty array)
- `labels=["nonexistent"]` → error with available options (validation)
- Combined updates → multiple fields update together (integration)
- Build passes → no TypeScript errors (type safety)

## Related Patterns

- **GitHub GraphQL PR Draft Status** (this directory) - Similar batched GraphQL fetching pattern
- **State Resolution** (`resolveState()` in same file) - Fuzzy matching with aliases
- **Team Resolution** (`resolveTeam()` in same file) - Exact matching by key or name

## Verification

From PR description:

```bash
# Test cases verified:
✅ labels=["worker-active"] → sets label (instead of "No updates provided")
✅ labels=[] → clears all labels
✅ labels=["nonexistent"] → error with missing name and available labels
✅ Combined state="Done" + labels=[...] → both update
✅ Build passes: npm run build
```

## When to Use This Pattern

**Use refresh-on-miss caching when:**
- Data is relatively stable but can change (labels, teams, users)
- Fetching is expensive (API call, database query)
- Most requests will hit cache (common labels used repeatedly)
- Occasional cache miss is acceptable (new labels are rare)
- Need to handle recently created items without TTL complexity

**Don't use when:**
- Data changes frequently (use no cache or short TTL)
- Freshness is critical (use no cache or validation)
- Fetching is cheap (just fetch every time)
- Data is immutable (use permanent cache)
