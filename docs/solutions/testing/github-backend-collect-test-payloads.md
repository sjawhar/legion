---
title: "GitHub backend /state/collect test payloads require exact GitHubProjectItem shape"
category: testing
tags:
  - github-backend
  - state-collect
  - integration-tests
  - silent-failure
date: 2026-04-05
status: active
module: daemon
related_issues:
  - "sjawhar-legion-203"
symptoms:
  - "/state/collect returns empty issues"
  - "issueStateCache is empty after collect"
  - "test expects 422 but gets 200"
  - "dispatch validation not working in tests"
---

# GitHub backend /state/collect test payloads require exact GitHubProjectItem shape

## Problem

Integration tests for `/state/collect` with the GitHub backend silently return
`{ issues: {} }` when the test payload doesn't match the `GitHubProjectItem`
interface. The parser skips unrecognized items without error, so:

- Tests pass with 200 OK
- No issues are parsed
- Downstream state (caches, validation gates) stays empty
- Tests that depend on cached state see cache misses instead of populated data

This is a **silent failure** — the test appears to work but proves nothing.

## Root cause

The `GitHubTracker.parseIssues()` method in `backends/github.ts` requires:

1. `content.type === "Issue"` (must be exactly the string `"Issue"`)
2. `content.number` (must be a number)
3. `content.repository` (must be a string in `"owner/repo"` format)
4. `status` at the top level (not inside `fieldValueByName`)
5. `labels` as a flat string array (not `{ nodes: [{ name: "..." }] }`)

Items missing any of these are silently skipped via `continue`.

## Correct test payload format

```typescript
const issues = [
  {
    content: {
      number: 42,
      repository: "test/repo",
      url: "https://github.com/test/repo/issues/42",
      type: "Issue",
    },
    status: "Needs Review",
    labels: ["worker-done"],
  },
];

await requestJson("/state/collect", {
  method: "POST",
  body: JSON.stringify({ backend: "github", issues }),
});
```

The `issues` field must be a **flat array** (not `{ items: [...] }` or
`{ nodes: [...] }`). The `extractItems()` function accepts either a raw array
or `{ items: [...] }`, but NOT `{ nodes: [...] }`.

## Wrong formats (all silently produce empty results)

```typescript
// WRONG: nodes wrapper (not recognized by extractItems)
issues: { nodes: [{ content: { url, title }, fieldValueByName: { name } }] }

// WRONG: missing content.type
issues: [{ content: { number: 42, repository: "test/repo" }, status: "Todo", labels: [] }]

// WRONG: labels as objects
issues: [{ content: {...}, status: "Todo", labels: [{ name: "worker-done" }] }]

// WRONG: status inside fieldValueByName
issues: [{ content: {...}, fieldValueByName: { name: "Needs Review" } }]
```

## Derived issue ID format

The parser builds issue IDs as `{owner}-{repo}-{number}` (lowercased, non-alphanumeric
replaced with hyphens). For `repository: "test/repo"` and `number: 42`, the issue ID
is `test-repo-42`. Cache lookups and dispatch validation use this normalized form.

## How to verify your test payload works

After calling `/state/collect`, check the response body:

```typescript
const collectRes = await requestJson("/state/collect", { ... });
const body = await collectRes.json();
console.log(Object.keys(body.issues)); // Should not be empty
```

If `body.issues` is `{}`, your payload is being silently skipped.
