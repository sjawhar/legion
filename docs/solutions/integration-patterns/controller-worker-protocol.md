---
title: "Controller-Worker Communication Protocol"
date: 2026-02-15
category: integration-patterns
tags: [controller, worker, dispatch, resume, protocol]
related-issues: [LEG-122, LEG-125, LEG-128]
---

# Controller-Worker Communication Protocol

## Dispatch vs Resume

See `legion-controller/SKILL.md` for the operational protocol. Key distinction:

- **Dispatch** = new process + new session via `POST /workers`
- **Resume** = prompt an existing session via `prompt_async` on the worker's port

Resume preserves full conversation context — use it for feedback relay, address-comments, and retro.

## Prompt Discipline

The controller tells the worker WHAT to do (mode + issue ID), not HOW to do it. The workflow skill guides execution.

**Good:** `/legion-worker implement mode for LEG-122 — address comments`
**Bad:** `Fix the state file race condition in persistState() and replace the empty catch block with console.error`

When the controller gives step-by-step instructions, workers skip their workflow skills (receiving-code-review, TDD, etc.) and go straight to implementing the specific instructions.

## Workspace Setup

Each issue gets its own jj workspace as a child of the current working state:
```bash
jj workspace add /path/to/workspace --name issue-id --revision @
```

Rules:
- Never put two workspaces on the same revision (causes divergent changes)
- Each workspace should be a child of the revision it needs to build on
- Workers create changes within their workspace; they should not affect the parent

## Session Identity

The daemon computes deterministic session IDs: `computeSessionId(teamId, issueId, mode)`. This enables:
- Idempotent dispatch (same issue+mode → same session ID)
- Session resumption after process restarts (sessions persist in SQLite)
- `initializeSession` handles 409 Duplicate gracefully (session already exists = success)

## Implement → Review Handoff

The implementer does NOT use `worker-done`. The flow is:
1. Implementer opens a **draft PR** (branch and title contain issue ID)
2. Linear's GitHub integration auto-transitions the issue to Needs Review
3. State machine sees: Needs Review, no worker-done, no live worker → `dispatch_reviewer`
4. Controller runs quality gate, then dispatches reviewer

This is the only handoff that relies on a side-channel (Linear's GitHub integration) rather than explicit label signaling.

## Review → Implement Feedback Loop

The reviewer signals via PR draft status:
- **PR ready** (not draft) = approved → controller transitions to Retro
- **PR draft** = changes requested → controller resumes implementer for address-comments

The reviewer MUST post specific review comments on the PR when requesting changes. Converting to draft without comments leaves the implementer with nothing to address.

## SDK Dependency

The daemon uses `@opencode-ai/sdk` from a custom fork (`sjawhar/opencode`) that supports custom session IDs on `POST /session`. The published npm package does not yet have this feature ([pending upstream](https://github.com/anomalyco/opencode/pull/13004)). The fork is consumed as a GitHub release tarball:
```json
"@opencode-ai/sdk": "https://github.com/sjawhar/opencode/releases/download/v1.2.4-post.2/opencode-ai-sdk-1.2.4-post.1.tgz"
```
