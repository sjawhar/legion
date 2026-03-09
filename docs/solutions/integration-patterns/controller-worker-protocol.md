---
title: "Controller-Worker Communication Protocol"
date: 2026-02-15
category: integration-patterns
tags:
  - controller
  - worker
  - dispatch
  - resume
  - protocol
status: active
---

# Controller-Worker Communication Protocol

## Dispatch vs Resume

See `legion-controller/SKILL.md` for the operational protocol. Key distinction:

- **Dispatch** = new session on shared serve via `POST /workers` (idempotent, ~10ms)
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

## Implement → Testing → Review Handoff

[HISTORICAL] This section predates the behavioral testing gate. The current flow is:

1. Implementer opens a **draft PR**, adds `worker-done`, and exits
2. State machine: In Progress + `worker-done` → `transition_to_testing`
3. Controller runs quality gate, dispatches tester
4. Tester passes → `transition_to_needs_review`
5. Controller dispatches reviewer

The implementer now uses explicit `worker-done` signaling instead of relying on Linear's auto-transition side-channel.

## Review → Implement → Testing Feedback Loop

The reviewer signals via PR draft status:
- **PR ready** (not draft) = approved → controller transitions to Retro
- **PR draft** = changes requested → controller transitions to In Progress, resumes implementer. Implementer's fixes go through the testing gate again before returning to review.

The reviewer MUST post specific review comments on the PR when requesting changes. Converting to draft without comments leaves the implementer with nothing to address.

## SDK Dependency

The daemon uses `@opencode-ai/sdk` from a custom fork (`sjawhar/opencode`) that supports custom session IDs on `POST /session`. The published npm package does not yet have this feature ([pending upstream](https://github.com/anomalyco/opencode/pull/13004)). The fork is consumed as a GitHub release tarball:
```json
"@opencode-ai/sdk": "https://github.com/sjawhar/opencode/releases/download/v1.2.4-post.2/opencode-ai-sdk-1.2.4-post.1.tgz"
```
