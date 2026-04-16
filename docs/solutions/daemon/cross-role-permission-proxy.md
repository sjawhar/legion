---
title: "Cross-role permission proxy via daemon endpoints"
category: daemon
tags:
  - github-apps
  - permissions
  - graphql
  - daemon-endpoint
  - cross-role
  - draft-status
  - security
module: daemon
related_issues:
  - "574"
symptoms:
  - "gh pr ready fails with permission error for review bot"
  - "markPullRequestReadyForReview requires contents:write"
  - "worker needs permission from a different role's token"
  - "GitHub App token lacks required permission for specific mutation"
date: 2026-04-16
status: active
---

# Cross-Role Permission Proxy via Daemon Endpoints

## Problem

In Legion's multi-app GitHub setup, each worker role (implement, review) runs with a
separate GitHub App token that has intentionally limited permissions. The review bot
(`legion-review`) has `pull_requests:write` but intentionally lacks `contents:write`
to prevent the reviewer from pushing code.

However, GitHub's `markPullRequestReadyForReview` and `convertPullRequestToDraft`
GraphQL mutations require `contents:write` — not just `pull_requests:write`. This is
a [known GitHub quirk](https://github.com/cli/cli/issues/8910) where the permission
model doesn't match the semantic intent of the operation.

This means the review worker cannot toggle PR draft status directly, even though
toggling draft status is a core part of the review signaling protocol (draft = changes
requested, ready = approved).

## Solution

Add a daemon endpoint that **proxies the operation through the correct role's token**.
The daemon has access to all role tokens via the `TokenManager`, so it can use the
implement token (which has `contents:write`) to perform the mutation on behalf of the
review worker.

### Architecture

```
Review Worker                    Daemon                         GitHub API
     │                             │                                │
     │  POST /pr/draft-status      │                                │
     │  {prNodeId, ready, owner}   │                                │
     │────────────────────────────>│                                │
     │                             │  getToken("implement", owner)  │
     │                             │──────────────────────────────>│
     │                             │  <── implement token ──────────│
     │                             │                                │
     │                             │  GraphQL mutation               │
     │                             │  (with implement token)        │
     │                             │──────────────────────────────>│
     │                             │  <── {id, isDraft} ────────────│
     │                             │                                │
     │  <── {prNodeId, isDraft} ───│                                │
```

### Key Design Decisions

1. **Daemon as proxy, not token distributor**: The daemon performs the operation itself
   rather than handing the implement token to the review worker. This preserves the
   security boundary — the review worker never sees the implement token.

2. **Minimal request surface**: The endpoint accepts only `{prNodeId, ready, owner}` —
   the minimum needed to perform the operation. The `owner` field identifies which
   repo's implement token to use (for multi-org setups).

3. **Input validation**: The endpoint validates `prNodeId` starts with `PR_` (GitHub's
   node ID prefix for pull requests) to prevent misuse. Note: this is a heuristic —
   GitHub node IDs are opaque and the prefix could theoretically change.

4. **Graceful degradation**: When GitHub Apps are not configured (legacy mode), the
   review workflow falls back to `gh pr ready` with the user's token, which has all
   permissions.

## When to Apply This Pattern

Use daemon-proxied endpoints when:
- A worker needs to perform an operation requiring a **different role's** permissions
- The permission gap is due to **intentional security boundaries** (not misconfiguration)
- The operation is **infrequent and targeted** (not high-throughput data access)

Do NOT use this pattern when:
- The worker should have the permission directly (fix the App permissions instead)
- The operation is read-only (usually doesn't need elevated permissions)
- High-throughput access is needed (add proper token scoping instead)

## Implementation Reference

- **GraphQL function**: `setDraftStatus()` in `packages/daemon/src/daemon/github-apps.ts`
- **Server endpoint**: `POST /pr/draft-status` in `packages/daemon/src/daemon/server.ts`
- **Review workflow**: Draft status toggle in `.opencode/skills/legion-worker/workflows/review.md`
- **GitHub Apps setup**: `docs/setup/github-apps.md`

## Gotchas

1. **`contents:write` for draft mutations**: This is the root cause — GitHub requires
   `contents:write` for `markPullRequestReadyForReview` and `convertPullRequestToDraft`,
   even though these are PR metadata operations. This is unlikely to change.

2. **Error handling in workflow scripts**: When the review workflow calls the daemon
   endpoint via `curl`, use `curl -sf` carefully — the `-s` flag silently swallows
   connection errors. Consider checking the HTTP status code explicitly if the draft
   status toggle is critical to the review verdict.

3. **PR node ID format**: The endpoint validates `prNodeId.startsWith("PR_")` but
   GitHub node IDs are opaque. If GitHub changes the prefix format, this validation
   will break. The validation is a defense-in-depth measure, not a guarantee.

## Related

- [Using PR Draft Status for Review Signaling](../integration-issues/github-graphql-pr-draft-status.md) — the signaling protocol that depends on draft status toggling
- [GitHub Apps Setup](../../setup/github-apps.md) — permission model for implement vs review roles
