---
title: "Multi-Backend Mutation Target Pattern: IssueSource Threading for Unambiguous Issue Identity"
category: daemon
tags:
  - interface-design
  - multi-backend
  - issue-tracker
  - mutation
  - github
  - linear
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "sjawhar-legion-494"
symptoms:
  - "parseIssueIdParts returns wrong owner for hyphenated org names"
  - "GitHub label removal or transition targets wrong repo"
  - "issueId ambiguity in multi-repo projects"
---

# Multi-Backend Mutation Target Pattern: IssueSource Threading for Unambiguous Issue Identity

Learnings from adding mutation methods (status transitions, label removal) to the `IssueTracker` interface (#494). The core challenge: the `issueId` string format (`owner-repo-number`) is lossy when owners or repos contain hyphens, so mutations need an unambiguous identity.

## The Problem: Lossy issueId Format

`parseIssueIdParts("my-org-repo-100")` splits on hyphens and must guess the owner/repo boundary. It parses as `{owner: "my", repo: "org-repo", number: "100"}` — wrong if the owner is `my-org`. This is a known limitation documented in tests, but it means **any mutation that uses `parseIssueIdParts` as the sole identity can target the wrong repo**.

## Pattern: IssueMutationTarget with Preferred Source

Instead of changing the lossy `issueId` format everywhere, introduce a composite identity type that carries both representations:

```typescript
export interface IssueMutationTarget {
  issueId: string;          // Always available (but ambiguous)
  source?: IssueSource | null; // Canonical owner/repo/number (from cached state)
}
```

The mutation implementation (`resolveTarget`) prefers `source` when available:

```typescript
private resolveTarget(target: IssueMutationTarget): { owner: string; repo: string; number: number } {
  if (target.source) {
    return { owner: target.source.owner, repo: target.source.repo, number: target.source.number };
  }
  // Fallback to lossy parsing — only for cases where source wasn't threaded through
  return parseIssueIdParts(target.issueId);
}
```

## Key Decisions

### Optional Interface Methods for Asymmetric Backends

The GitHub backend supports daemon-side mutations (CLI/GraphQL), but the Linear backend does mutations through the controller's MCP connection. Making mutation methods optional on the interface honestly represents this asymmetry:

```typescript
export interface IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[];
  transitionIssue?(target: IssueMutationTarget, newStatus: IssueStatusLiteral): Promise<void>;
  removeLabel?(target: IssueMutationTarget, label: string): Promise<void>;
}
```

Callers check `if (tracker.transitionIssue)` before invoking. The Linear implementation throws explicit "not implemented" errors if called directly.

### Threading IssueSource Through the Call Chain

The `IssueSource` is available from cached `ParsedIssue` state (populated during `parseIssues` from the raw API data). When the server receives an advance request for an `issueId`, it looks up the cached issue to get the canonical `source`, then passes both to mutation methods.

**Wide surface area warning:** Adding a new parameter to the `IssueTracker` interface requires updating: the interface, both backend implementations (GitHub + Linear), all callers in `server.ts`, and mutation tests. This is a ~5-file change even for a single parameter addition.

## When to Apply

- Any new `IssueTracker` mutation method should accept `IssueMutationTarget`, not raw `issueId`
- The `resolveTarget` pattern (prefer `source`, fall back to parsing) should be the standard for GitHub mutations
- When the cached state has `IssueSource` available, always thread it through — don't discard it and re-parse later

## Gotcha: Non-Blocking Side Effects

When a mutation is a side effect of a larger operation (e.g., removing `worker-active` label before re-dispatching a worker), wrap it in try/catch to prevent the side effect from blocking the primary operation:

```typescript
// Label removal should not prevent the redispatch
try {
  if (tracker.removeLabel) {
    await tracker.removeLabel(target, "worker-active");
  }
} catch (e) {
  console.error(`Failed to remove label, continuing with dispatch: ${e}`);
}
// Proceed with dispatch regardless
```
