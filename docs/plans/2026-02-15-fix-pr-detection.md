# Fix PR Detection (LEG-124) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix the state machine's PR detection so workers always create the artifact the state machine already reads — a Linear attachment containing the PR URL.

**Architecture:** The state machine already searches Linear attachments for PR URLs. The bug is that the implement worker never creates an attachment after opening a PR. Primary fix: worker creates a Linear attachment via GraphQL after `gh pr create`. Defensive fallback: state machine also searches comments for PR URLs (handles edge cases where attachment creation fails, or data arrives in comments from other sources).

**Tech Stack:** TypeScript/Bun (state machine), Markdown skill (implement workflow), Linear MCP (GraphQL)

---

### Task 1: Update implement workflow to create Linear attachment after PR creation — Independent

This is the primary fix. The state machine already reads attachments — make the worker produce one.

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/implement.md:121-139`

**Step 1: Modify the Ship section to add attachment creation**

In `.opencode/skills/legion-worker/workflows/implement.md`, find the Ship section (### 6. Ship). Replace everything from `### 6. Ship` through `Linear auto-associates the PR via the issue ID in the branch/title.` with:

````markdown
### 6. Ship

```bash
jj describe -m "$LINEAR_ISSUE_ID: [description]"
jj git push --named "$LINEAR_ISSUE_ID"=@

gh pr create --draft \
  --title "$LINEAR_ISSUE_ID: [title]" \
  --body "Implements $LINEAR_ISSUE_ID

[summary]" \
  --head "$LINEAR_ISSUE_ID"
```

After creating the PR, link it to the Linear issue so the state machine can detect it:

```bash
PR_URL=$(gh pr view --json url -q '.url')
```

Create a Linear attachment (the state machine reads attachments to detect PRs). Use the issue UUID from the `linear_linear(action="get", ...)` call in step 1:

```
linear_linear(action="graphql", graphql="mutation { attachmentLinkURL(url: \"$PR_URL\", issueId: \"$ISSUE_UUID\") { success } }")
```

If attachment creation fails (permissions, API error), fall back to posting a comment:

```
linear_linear(action="comment", id="$LINEAR_ISSUE_ID", body="PR: $PR_URL")
```
````

**Step 2: Commit**

```bash
jj describe -m "LEG-124: implement worker creates Linear attachment after PR creation"
```

---

### Task 2: Add comment-based PR URL fallback to state machine — Independent

Defensive fallback. If attachment creation fails, or if PRs are linked via comments from other sources (human-posted, older workers), the state machine should still find them. This code is safe even if comments aren't in the data pipeline — it gracefully returns null (same as current behavior).

**Files:**
- Modify: `packages/daemon/src/state/types.ts:128-144`
- Modify: `packages/daemon/src/state/fetch.ts:286-353`
- Test: `packages/daemon/src/state/__tests__/fetch.test.ts`

**Step 1: Add comment types to LinearIssueRaw**

In `packages/daemon/src/state/types.ts`, add after the `LinearAttachment` interface (after line 131):

```typescript
export interface LinearComment {
  body?: string;
}

export interface LinearCommentsContainer {
  nodes: LinearComment[];
}
```

Then add `comments` field to `LinearIssueRaw` (after `attachments` on line 143):

```typescript
  comments?: LinearComment[] | LinearCommentsContainer; // MCP: array, GraphQL: {nodes: [...]}
```

**Step 2: Write failing tests**

In `packages/daemon/src/state/__tests__/fetch.test.ts`, update the import on line 11 to include `extractPrRefFromText`:

```typescript
import {
  type CommandRunner,
  extractPrRefFromText,
  fetchAllIssueData,
  GitHubAPIError,
  getLiveWorkers,
  getPrDraftStatusBatch,
  parseLinearIssues,
} from "../fetch";
```

Add a new describe block after the `parseLinearIssues edge cases` block (after line 141):

```typescript
describe("extractPrRefFromText", () => {
  it("extracts PR URL from text", () => {
    const ref = extractPrRefFromText("PR: https://github.com/owner/repo/pull/42");
    expect(ref).toEqual({ owner: "owner", repo: "repo", number: 42 });
  });

  it("returns null for text without PR URL", () => {
    expect(extractPrRefFromText("No PR here")).toBeNull();
    expect(extractPrRefFromText("https://github.com/owner/repo/issues/1")).toBeNull();
    expect(extractPrRefFromText("")).toBeNull();
  });

  it("extracts first PR URL when multiple present", () => {
    const ref = extractPrRefFromText(
      "See https://github.com/a/b/pull/1 and https://github.com/c/d/pull/2"
    );
    expect(ref).toEqual({ owner: "a", repo: "b", number: 1 });
  });
});
```

Add to the existing `parseLinearIssues edge cases` describe block (before its closing `});`):

```typescript
  it("extracts PR from comments when no attachment PR found", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Needs Review" },
        labels: { nodes: [{ name: "worker-done" }] },
        comments: [{ body: "PR: https://github.com/owner/repo/pull/42" }],
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result[0].hasPr).toBe(true);
    expect(result[0].prRef).toEqual({ owner: "owner", repo: "repo", number: 42 });
  });

  it("prefers attachment PR over comment PR", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Needs Review" },
        labels: { nodes: [] },
        attachments: [{ url: "https://github.com/owner/repo/pull/1" }],
        comments: [{ body: "PR: https://github.com/owner/repo/pull/2" }],
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result[0].prRef).toEqual({ owner: "owner", repo: "repo", number: 1 });
  });

  it("handles missing and null comments gracefully", () => {
    const issues: LinearIssueRaw[] = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: { nodes: [] },
      },
      {
        identifier: "ENG-22",
        state: { name: "Todo" },
        labels: { nodes: [] },
        comments: null as unknown as undefined,
      },
    ];
    const result = parseLinearIssues(issues);
    expect(result[0].prRef).toBeNull();
    expect(result[1].prRef).toBeNull();
  });
```

**Step 3: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: FAIL — `extractPrRefFromText` not exported, comment parsing not implemented

**Step 4: Implement extractPrRefFromText and comment fallback**

In `packages/daemon/src/state/fetch.ts`, add before `parseLinearIssues` (before the `// Issue Parsing` section comment around line 274):

```typescript
/**
 * Extract a GitHub PR reference from free text (e.g., a comment body).
 *
 * Searches for the first GitHub PR URL pattern in the text and parses it.
 *
 * @param text - Free text that may contain a GitHub PR URL
 * @returns GitHubPRRef or null if no PR URL found
 */
export function extractPrRefFromText(text: string): GitHubPRRefType | null {
  const match = text.match(/https:\/\/github\.com\/[\w.-]+\/[\w.-]+\/pull\/\d+/);
  if (!match) {
    return null;
  }
  return GitHubPRRef.fromUrl(match[0]);
}
```

In the `parseLinearIssues` function, after the attachment search loop (after the `}` closing the for-of over attachments, around line 347), add:

```typescript
    // Fallback: search comments for PR URL if no attachment found
    if (!prRef) {
      const rawComments = issue.comments;
      let commentBodies: string[] = [];

      if (rawComments !== null && rawComments !== undefined) {
        if (Array.isArray(rawComments)) {
          // MCP format: [{body: "..."}, ...]
          commentBodies = rawComments
            .filter(
              (c): c is { body: string } =>
                typeof c === "object" && c !== null && typeof c.body === "string"
            )
            .map((c) => c.body);
        } else if (typeof rawComments === "object" && "nodes" in rawComments) {
          // GraphQL format: {nodes: [{body: "..."}, ...]}
          const nodes = (rawComments as { nodes: unknown[] }).nodes;
          if (Array.isArray(nodes)) {
            commentBodies = nodes
              .filter(
                (c): c is { body: string } =>
                  typeof c === "object" && c !== null && typeof c.body === "string"
              )
              .map((c) => c.body);
          }
        }
      }

      for (const body of commentBodies) {
        prRef = extractPrRefFromText(body);
        if (prRef) {
          break;
        }
      }
    }
```

**Step 5: Run tests to verify they pass**

Run: `bun test packages/daemon/src/state/__tests__/fetch.test.ts`
Expected: PASS

**Step 6: Run all checks**

```bash
bun test
bunx tsc --noEmit
bunx biome check packages/daemon/src/state/
```

Expected: All pass

**Step 7: Commit**

```bash
jj describe -m "LEG-124: add comment-based PR URL fallback in state machine"
```

---

## Data Pipeline Note

The comment fallback (Task 2) only activates when comments are present in the Linear data piped to the CLI. Currently the controller calls `linear_linear(action="search", ...)` which may not include comments in search results.

- **Task 1 (attachment creation)** is the primary fix — it ensures future PRs are detected via the existing attachment path.
- **Task 2 (comment fallback)** is a defensive layer that works IF comments are in the data. No controller changes needed now — the code gracefully handles missing comments (returns null, same as current behavior).
- If comment fallback proves valuable later, the controller can be updated to fetch individual issue details for issues in "Needs Review" state.

## Verification

After all tasks complete:

```bash
bun test                   # All tests pass (including new comment fallback tests)
bunx tsc --noEmit          # No type errors
bunx biome check src/      # No lint issues
```
