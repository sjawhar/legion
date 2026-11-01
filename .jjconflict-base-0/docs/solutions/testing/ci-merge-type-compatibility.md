---
title: "CI Merge Type Compatibility: Partial<T> Spread Failures"
category: testing
tags:
  - ci
  - typescript
  - github-actions
  - type-safety
  - test-fixtures
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "406"
symptoms:
  - "CI typecheck fails but local tsc passes"
  - "Type is not assignable to type IssueStateDict"
  - "Types of property are incompatible"
  - "Type 'X | undefined' is not assignable to type 'X'"
---

# CI Merge Type Compatibility: Partial<T> Spread Failures

## Problem

GitHub Actions for PRs creates a **merge commit** of the PR branch with the
base branch before running checks. When main has added fields to shared types
that the PR branch doesn't include in test fixtures, CI will fail with type
errors even though local `tsc --noEmit` passes.

## Root Cause

Given a test helper:

```typescript
function createIssueState(overrides: Partial<IssueStateDict> = {}): IssueStateDict {
  return {
    status: "Todo",
    labels: [],
    // ... other fields
    ...overrides,
  };
}
```

If main adds `blockedByIds: string[]` to `IssueStateDict` but the test helper
doesn't include it:

1. `Partial<IssueStateDict>` now includes `blockedByIds?: string[]`
2. The spread `...overrides` can set `blockedByIds` to `undefined`
3. TypeScript infers the return as `{ ..., blockedByIds: string[] | undefined }`
4. This doesn't satisfy `IssueStateDict.blockedByIds: string[]` (required, not optional)

**Locally** the type passes because the branch's `IssueStateDict` doesn't
have `blockedByIds`. **On CI** the merged code has it.

## Solution

**Always include all required fields in test fixture factories.** Check main's
version of shared types before writing fixtures:

```bash
# Before writing test fixtures, check what main has
git diff main -- packages/daemon/src/state/types.ts | grep "^+"
```

For existing fixtures, add new fields with sensible defaults:

```typescript
function createIssueState(overrides: Partial<IssueStateDict> = {}): IssueStateDict {
  return {
    status: "Todo",
    labels: [],
    blockedByIds: [],  // Added for main compatibility
    // ... other fields
    ...overrides,
  };
}
```

## When This Happens

- Feature branch carries commits from another issue (e.g., issue #114)
- Those commits modify shared types
- Main independently receives those type changes (via a different PR)
- The feature branch's test fixtures were written against the branch's types
- CI merges with main, creating a superset of both type definitions

## Prevention

1. Before writing test fixture factories, check `IssueStateDict` (and similar
   shared types) on both the branch AND main
2. Include all fields from both versions in the fixture defaults
3. Run `bunx tsc --noEmit` locally after pulling latest main to catch this
   before CI
